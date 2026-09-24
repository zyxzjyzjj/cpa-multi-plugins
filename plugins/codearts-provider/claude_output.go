package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync/atomic"
	"time"
)

// This file renders Anthropic Messages output for clients that speak the
// Claude protocol.
//
// Why the plugin renders Anthropic itself. CLIProxyAPI picks a plugin executor's
// response format from the executor_output_formats it declares. When the client
// protocol is declared, the host forwards every plugin chunk to the client
// handler unchanged; when it is not declared, the host translates the plugin's
// output through its own OpenAI→target translator. The framing differs between
// the two paths, and in a way the plugin cannot otherwise detect:
//
//   - Chat Completions (/v1/chat/completions): the host writes
//     "data: <chunk>\n\n" around each chunk it receives, so a plugin must emit
//     bare JSON chunk objects. Emitting SSE frames here yields "data: data: {...}".
//   - Anthropic Messages (/v1/messages): the host writes the chunk bytes
//     unchanged, and its OpenAI→Claude stream translator only accepts frames that
//     already carry a "data: " prefix (internal/translator/openai/claude returns
//     no frames for anything else). Bare JSON chunks are silently dropped, which
//     is why the stream previously arrived as message_delta/message_stop only.
//
// The ExecutorRequest cannot tell the two apart: prepareExecutorCall overwrites
// req.Format and opts.SourceFormat with the formats the host selected, so
// SourceFormat is "openai" for both an OpenAI client and an Anthropic client.
// Declaring "claude" in executor_output_formats makes the host select it for
// Anthropic clients and pass the frames straight through, which gives the plugin
// an explicit signal (req.Format == "claude") and the responsibility to produce
// protocol-correct Anthropic SSE itself.
//
// The event shapes below mirror the host's own OpenAI→Claude translator
// (internal/translator/openai/claude/openai_claude_response.go) so that a client
// sees the same stream CPA would produce for any OpenAI-compatible upstream.

const (
	protocolOpenAI = "openai"
	protocolClaude = "claude"
)

// clientProtocol maps the ExecutorRequest.Format the host selected onto the
// response protocol the plugin has to render.
func clientProtocol(format string) string {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "claude", "anthropic", "anthropic-messages", "messages":
		return protocolClaude
	default:
		return protocolOpenAI
	}
}

var claudeToolIDSanitizer = regexp.MustCompile(`[^a-zA-Z0-9_-]`)

var claudeToolIDCounter uint64

// claudeToolID makes a tool_use id conform to Anthropic's ^[a-zA-Z0-9_-]+$.
func claudeToolID(id string) string {
	sanitized := claudeToolIDSanitizer.ReplaceAllString(strings.TrimSpace(id), "_")
	if sanitized == "" {
		sanitized = fmt.Sprintf("toolu_%d_%d", time.Now().UnixNano(), atomic.AddUint64(&claudeToolIDCounter, 1))
	}
	return sanitized
}

// anthropicEvent renders one Anthropic SSE event. The host writes these bytes to
// an Anthropic client unchanged, so the trailing blank line is part of the frame.
func anthropicEvent(event string, payload any) []byte {
	raw, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		return nil
	}
	frame := make([]byte, 0, len(event)+len(raw)+16)
	frame = append(frame, "event: "...)
	frame = append(frame, event...)
	frame = append(frame, "\ndata: "...)
	frame = append(frame, raw...)
	frame = append(frame, "\n\n"...)
	return frame
}

// anthropicTool tracks one streamed tool call so its arguments can be delivered
// as a single input_json_delta when the block closes.
type anthropicTool struct {
	openAIIndex int
	blockIndex  int
	id          string
	name        string
	arguments   strings.Builder
	started     bool
	closed      bool
}

// anthropicStreamRenderer converts OpenAI chat.completion.chunk payloads into
// Anthropic Messages SSE events.
type anthropicStreamRenderer struct {
	messageID string
	model     string

	started   bool
	nextIndex int

	openKind  string
	openIndex int
	openTool  *anthropicTool

	tools         []*anthropicTool
	toolsByOpenAI map[int]*anthropicTool

	sawToolCall bool
	sawContent  bool

	inputTokens  int64
	outputTokens int64
	cachedTokens int64
	cacheWrite   int64
	hasUsage     bool

	finishReason string
	deltaSent    bool
	stopSent     bool
}

func newAnthropicStreamRenderer(model string) *anthropicStreamRenderer {
	return &anthropicStreamRenderer{
		model:         model,
		openIndex:     -1,
		toolsByOpenAI: map[int]*anthropicTool{},
	}
}

// feed consumes one OpenAI chunk payload (no `data:` prefix and no [DONE]
// marker) and returns the Anthropic events to forward.
func (r *anthropicStreamRenderer) feed(payload []byte) [][]byte {
	if len(payload) == 0 {
		return nil
	}
	if len(payload) > 0 && payload[0] != '{' {
		// The renderer only understands JSON chunks; anything else (a heartbeat
		// comment, a bare [DONE]) has already been filtered by the caller.
		return nil
	}
	if fault := extractionFault(payload); len(fault) > 0 {
		// An OpenAI error object would be parsed as an unknown message type by an
		// Anthropic client, so it is re-framed as a terminal error event.
		return [][]byte{anthropicEvent("error", map[string]any{
			"type": "error",
			"error": map[string]any{
				"type":    "api_error",
				"message": fault,
			},
		})}
	}

	var chunk openAIChunk
	if errUnmarshal := json.Unmarshal(payload, &chunk); errUnmarshal != nil {
		return nil
	}
	if chunk.ID != "" && r.messageID == "" {
		r.messageID = chunk.ID
	}
	if chunk.Model != "" {
		r.model = chunk.Model
	}
	if chunk.Usage != nil {
		r.observeUsage(chunk.Usage)
	}

	var out [][]byte
	for _, choice := range chunk.Choices {
		if choice.Delta != nil {
			out = append(out, r.startMessage()...)
			if reasoning := choice.Delta.ReasoningContent; reasoning != "" {
				out = append(out, r.thinkingDelta(reasoning)...)
			}
			if content := choice.Delta.Content; content != "" {
				out = append(out, r.textDelta(content)...)
			}
			if len(choice.Delta.ToolCalls) > 0 {
				out = append(out, r.toolCallDeltas(choice.Delta.ToolCalls)...)
			}
		}
		if choice.FinishReason != nil && strings.TrimSpace(*choice.FinishReason) != "" {
			r.finishReason = normalizeFinishReason(strings.TrimSpace(*choice.FinishReason), r.sawToolCall, r.toolArgumentsValid())
			out = append(out, r.closeOpenBlock()...)
		}
	}

	// message_delta/message_stop are only emitted once the generation is over:
	// either an explicit finish reason arrived, or a trailing usage-only chunk
	// followed content. This mirrors the host translator so the two paths emit
	// the same event sequence.
	trailingUsage := r.hasUsage && len(chunk.Choices) == 0 && r.sawContent
	if !r.deltaSent && (r.finishReason != "" || trailingUsage) && r.hasUsage {
		out = append(out, r.closeOpenBlock()...)
		out = append(out, r.messageDelta()...)
		out = append(out, r.messageStop()...)
	}
	return out
}

// finish closes the stream when the upstream connection ends, whether or not the
// upstream sent a finish reason or usage.
func (r *anthropicStreamRenderer) finish() [][]byte {
	var out [][]byte
	out = append(out, r.closeOpenBlock()...)
	for _, tool := range r.tools {
		if tool.closed {
			continue
		}
		if !tool.started {
			// Some upstreams leave function.name empty for the whole stream.
			// Dropping such a call loses tool_use entirely, so a placeholder name
			// is used when the call carries any signal at all.
			if tool.id == "" && tool.name == "" && tool.arguments.Len() == 0 {
				tool.closed = true
				continue
			}
			if tool.name == "" {
				tool.name = fmt.Sprintf("tool_%d", tool.openAIIndex)
			}
			out = append(out, r.startToolBlock(tool)...)
		}
		out = append(out, r.closeToolBlock(tool)...)
	}
	out = append(out, r.messageDelta()...)
	out = append(out, r.messageStop()...)
	return out
}

func (r *anthropicStreamRenderer) startMessage() [][]byte {
	if r.started {
		return nil
	}
	r.started = true
	if strings.TrimSpace(r.messageID) == "" {
		r.messageID = "msg_" + randomHex(16)
	}
	// input_tokens is usually only known after the terminal usage chunk, so it is
	// reported in message_delta as well; 0 here matches the host's own stream
	// translation for OpenAI-compatible upstreams.
	usage := map[string]any{"input_tokens": r.usageInputTokens(), "output_tokens": 0}
	return [][]byte{anthropicEvent("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            r.messageID,
			"type":          "message",
			"role":          "assistant",
			"model":         r.model,
			"content":       []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         usage,
		},
	})}
}

func (r *anthropicStreamRenderer) thinkingDelta(text string) [][]byte {
	var out [][]byte
	if r.openKind != "thinking" {
		out = append(out, r.closeOpenBlock()...)
		r.openKind = "thinking"
		r.openIndex = r.nextIndex
		r.nextIndex++
		out = append(out, anthropicEvent("content_block_start", map[string]any{
			"type":          "content_block_start",
			"index":         r.openIndex,
			"content_block": map[string]any{"type": "thinking", "thinking": ""},
		}))
	}
	r.sawContent = true
	out = append(out, anthropicEvent("content_block_delta", map[string]any{
		"type":  "content_block_delta",
		"index": r.openIndex,
		"delta": map[string]any{"type": "thinking_delta", "thinking": text},
	}))
	return out
}

func (r *anthropicStreamRenderer) textDelta(text string) [][]byte {
	var out [][]byte
	if r.openKind != "text" {
		out = append(out, r.closeOpenBlock()...)
		r.openKind = "text"
		r.openIndex = r.nextIndex
		r.nextIndex++
		out = append(out, anthropicEvent("content_block_start", map[string]any{
			"type":          "content_block_start",
			"index":         r.openIndex,
			"content_block": map[string]any{"type": "text", "text": ""},
		}))
	}
	r.sawContent = true
	out = append(out, anthropicEvent("content_block_delta", map[string]any{
		"type":  "content_block_delta",
		"index": r.openIndex,
		"delta": map[string]any{"type": "text_delta", "text": text},
	}))
	return out
}

func (r *anthropicStreamRenderer) toolCallDeltas(calls []openAIToolCall) [][]byte {
	var out [][]byte
	for _, call := range calls {
		tool := r.toolFor(call.Index)
		if call.ID != "" {
			tool.id = call.ID
		}
		if call.Function != nil {
			// The name is only recorded until the block is announced: restating it
			// after content_block_start would drift from what the client was told.
			if !tool.started && call.Function.Name != "" {
				tool.name = call.Function.Name
			}
			if call.Function.Arguments != "" {
				tool.arguments.WriteString(call.Function.Arguments)
			}
		}
		if !tool.started && tool.id != "" && tool.name != "" {
			out = append(out, r.closeOpenBlock()...)
			out = append(out, r.startToolBlock(tool)...)
		}
	}
	return out
}

func (r *anthropicStreamRenderer) toolFor(openAIIndex int) *anthropicTool {
	if tool, ok := r.toolsByOpenAI[openAIIndex]; ok {
		return tool
	}
	tool := &anthropicTool{openAIIndex: openAIIndex, blockIndex: -1}
	r.toolsByOpenAI[openAIIndex] = tool
	r.tools = append(r.tools, tool)
	return tool
}

func (r *anthropicStreamRenderer) startToolBlock(tool *anthropicTool) [][]byte {
	if tool.started {
		return nil
	}
	tool.started = true
	tool.blockIndex = r.nextIndex
	r.nextIndex++
	r.openKind = "tool_use"
	r.openIndex = tool.blockIndex
	r.openTool = tool
	r.sawToolCall = true
	return [][]byte{anthropicEvent("content_block_start", map[string]any{
		"type":  "content_block_start",
		"index": tool.blockIndex,
		"content_block": map[string]any{
			"type":  "tool_use",
			"id":    claudeToolID(tool.id),
			"name":  tool.name,
			"input": map[string]any{},
		},
	})}
}

func (r *anthropicStreamRenderer) closeToolBlock(tool *anthropicTool) [][]byte {
	if tool.closed {
		return nil
	}
	tool.closed = true
	var out [][]byte
	if tool.arguments.Len() > 0 {
		out = append(out, anthropicEvent("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": tool.blockIndex,
			"delta": map[string]any{
				"type":         "input_json_delta",
				"partial_json": fixToolArguments(tool.arguments.String()),
			},
		}))
	}
	out = append(out, anthropicEvent("content_block_stop", map[string]any{
		"type":  "content_block_stop",
		"index": tool.blockIndex,
	}))
	if r.openTool == tool {
		r.openKind = ""
		r.openIndex = -1
		r.openTool = nil
	}
	return out
}

// closeOpenBlock terminates whichever content block is open. Anthropic requires
// content blocks to be strictly sequential, so every switch closes first.
func (r *anthropicStreamRenderer) closeOpenBlock() [][]byte {
	switch r.openKind {
	case "":
		return nil
	case "tool_use":
		tool := r.openTool
		r.openKind = ""
		r.openIndex = -1
		r.openTool = nil
		if tool == nil {
			return nil
		}
		return r.closeToolBlock(tool)
	default:
		index := r.openIndex
		r.openKind = ""
		r.openIndex = -1
		return [][]byte{anthropicEvent("content_block_stop", map[string]any{
			"type":  "content_block_stop",
			"index": index,
		})}
	}
}

func (r *anthropicStreamRenderer) messageDelta() [][]byte {
	if r.deltaSent {
		return nil
	}
	r.deltaSent = true
	reason := r.finishReason
	if reason == "" {
		reason = "stop"
	}
	usage := map[string]any{
		"input_tokens":  r.usageInputTokens(),
		"output_tokens": r.outputTokens,
	}
	if r.cachedTokens > 0 {
		usage["cache_read_input_tokens"] = r.cachedTokens
	}
	if r.cacheWrite > 0 {
		usage["cache_creation_input_tokens"] = r.cacheWrite
	}
	return [][]byte{anthropicEvent("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": claudeStopReason(reason, r.sawToolCall, r.toolArgumentsValid()), "stop_sequence": nil},
		"usage": usage,
	})}
}

func (r *anthropicStreamRenderer) messageStop() [][]byte {
	if r.stopSent {
		return nil
	}
	r.stopSent = true
	return [][]byte{anthropicEvent("message_stop", map[string]any{"type": "message_stop"})}
}

func (r *anthropicStreamRenderer) observeUsage(usage *openAIUsage) {
	r.hasUsage = true
	r.inputTokens = int64(usage.PromptTokens)
	r.outputTokens = int64(usage.CompletionTokens)
	r.cachedTokens = int64(usage.cachedTokens())
	r.cacheWrite = int64(usage.cacheWriteTokens())
}

// usageInputTokens converts OpenAI prompt tokens into Anthropic input tokens,
// which exclude cache reads.
func (r *anthropicStreamRenderer) usageInputTokens() int64 {
	input := r.inputTokens
	if r.cachedTokens > 0 {
		if input >= r.cachedTokens {
			input -= r.cachedTokens
		} else {
			input = 0
		}
	}
	return input
}

// toolArgumentsValid mirrors the host's check: a tool call whose arguments are
// not a JSON object must not be announced as a completed tool_use.
func (r *anthropicStreamRenderer) toolArgumentsValid() bool {
	for _, tool := range r.tools {
		if tool.arguments.Len() == 0 {
			continue
		}
		arguments := strings.TrimSpace(tool.arguments.String())
		if arguments == "" {
			return false
		}
		if arguments == "{}" {
			continue
		}
		fixed := strings.TrimSpace(fixToolArguments(arguments))
		if !json.Valid([]byte(fixed)) || !strings.HasPrefix(fixed, "{") {
			return false
		}
	}
	return true
}

// extractionFault returns the error message when an OpenAI chunk carries a
// top-level error object, and an empty string otherwise.
func extractionFault(payload []byte) string {
	var fault struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(payload, &fault) != nil || len(fault.Error) == 0 || string(fault.Error) == "null" {
		return ""
	}
	var detail struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(fault.Error, &detail) == nil && strings.TrimSpace(detail.Message) != "" {
		return detail.Message
	}
	if text := strings.TrimSpace(strings.Trim(string(fault.Error), `"`)); text != "" {
		return truncate(text, 300)
	}
	return "upstream reported an error"
}

// normalizeFinishReason maps an OpenAI finish reason onto the internal value the
// Anthropic stop_reason is derived from, mirroring the host translator's rules.
func normalizeFinishReason(reason string, sawToolCall, toolArgumentsValid bool) string {
	switch {
	case reason == "length", reason == "content_filter":
		return reason
	case sawToolCall:
		if toolArgumentsValid {
			return "tool_calls"
		}
		return "length"
	case reason == "tool_calls":
		return "stop"
	default:
		return reason
	}
}

// claudeStopReason maps an OpenAI finish reason onto Anthropic's stop_reason.
func claudeStopReason(reason string, sawToolCall, toolArgumentsValid bool) string {
	switch reason {
	case "length":
		return "max_tokens"
	case "content_filter":
		return "end_turn"
	case "tool_calls", "function_call":
		if sawToolCall && !toolArgumentsValid {
			return "max_tokens"
		}
		return "tool_use"
	default:
		return "end_turn"
	}
}

// fixToolArguments repairs the single-quoted JSON some upstreams emit. Anything
// that is not that specific shape is returned unchanged so no content is
// rewritten silently.
func fixToolArguments(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" || json.Valid([]byte(trimmed)) {
		return trimmed
	}
	if strings.Contains(trimmed, `"`) || !strings.Contains(trimmed, "'") {
		return trimmed
	}
	return strings.ReplaceAll(trimmed, "'", `"`)
}

// anthropicMessageFromCompletion renders an aggregated OpenAI completion as an
// Anthropic Messages response body. It is used for non-streaming /v1/messages
// requests now that the plugin declares "claude" as an output format, which makes
// the host pass the payload through instead of translating it.
func anthropicMessageFromCompletion(body []byte) ([]byte, error) {
	var completion openAICompletion
	if errUnmarshal := json.Unmarshal(body, &completion); errUnmarshal != nil {
		return nil, fmt.Errorf("decode aggregated completion: %w", errUnmarshal)
	}

	var (
		text         string
		reasoning    string
		toolCalls    []openAIToolCall
		finishReason string
	)
	if len(completion.Choices) > 0 {
		choice := completion.Choices[0]
		if choice.Message != nil {
			text = choice.Message.Content
			reasoning = choice.Message.ReasoningContent
			toolCalls = choice.Message.ToolCalls
		}
		if choice.FinishReason != nil {
			finishReason = strings.TrimSpace(*choice.FinishReason)
		}
	}

	content := make([]any, 0, len(toolCalls)+2)
	if strings.TrimSpace(reasoning) != "" {
		content = append(content, map[string]any{"type": "thinking", "thinking": reasoning})
	}
	if text != "" {
		content = append(content, map[string]any{"type": "text", "text": text})
	}
	argumentsValid := true
	for _, call := range toolCalls {
		input := any(map[string]any{})
		name := ""
		if call.Function != nil {
			name = call.Function.Name
			if arguments := strings.TrimSpace(call.Function.Arguments); arguments != "" {
				var parsed any
				if json.Unmarshal([]byte(fixToolArguments(arguments)), &parsed) == nil {
					if object, ok := parsed.(map[string]any); ok {
						input = object
					} else {
						argumentsValid = false
					}
				} else {
					argumentsValid = false
				}
			}
		}
		content = append(content, map[string]any{
			"type":  "tool_use",
			"id":    claudeToolID(call.ID),
			"name":  name,
			"input": input,
		})
	}

	usage := map[string]any{"input_tokens": int64(0), "output_tokens": int64(0)}
	if completion.Usage != nil {
		inputTokens := int64(completion.Usage.PromptTokens)
		cachedTokens := int64(completion.Usage.cachedTokens())
		if cachedTokens > 0 {
			if inputTokens >= cachedTokens {
				inputTokens -= cachedTokens
			} else {
				inputTokens = 0
			}
			usage["cache_read_input_tokens"] = cachedTokens
		}
		if cacheWrite := completion.Usage.cacheWriteTokens(); cacheWrite > 0 {
			usage["cache_creation_input_tokens"] = int64(cacheWrite)
		}
		usage["input_tokens"] = inputTokens
		usage["output_tokens"] = int64(completion.Usage.CompletionTokens)
	}

	out := map[string]any{
		"id":            completion.ID,
		"type":          "message",
		"role":          "assistant",
		"model":         completion.Model,
		"content":       content,
		"stop_reason":   claudeStopReason(finishReason, len(toolCalls) > 0, argumentsValid),
		"stop_sequence": nil,
		"usage":         usage,
	}
	raw, errMarshal := json.Marshal(out)
	if errMarshal != nil {
		return nil, errMarshal
	}
	return raw, nil
}
