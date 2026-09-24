package main

import (
	"encoding/json"
	"strings"
	"time"
)

// This file translates between the OpenAI chat-completions protocol that
// CLIProxyAPI speaks and the proprietary CodeArts Doer protocol used by the
// official IDE extension.
//
// Native upstream streaming is not standard SSE: frames are JSON objects
// separated by a blank line, heartbeat frames are SSE comments (`:heartbeat`),
// and the end-of-stream marker is a JSON body whose `text` field is "[DONE]"
// rather than a `data: [DONE]` frame. This translator normalises all of that
// into OpenAI `chat.completion.chunk` frames.

const doneSentinel = "[DONE]"

// nativeChunk is one decoded CodeArts Doer stream frame. Only the fields the
// plugin consumes are modelled.
type nativeChunk struct {
	ID             string          `json:"id"`
	Text           string          `json:"text"`
	Type           string          `json:"type"`
	Stage          json.RawMessage `json:"stage"`
	Delta          *nativeDelta    `json:"delta"`
	ErrorCode      string          `json:"error_code"`
	ErrorMsg       string          `json:"error_msg"`
	Model          string          `json:"model"`
	ModelName      string          `json:"model_name"`
	ChatID         string          `json:"chat_id"`
	PromptTokens   int             `json:"prompt_tokens"`
	CompleteTokens int             `json:"completion_tokens"`
	TotalTokens    int             `json:"total_tokens"`
}

type nativeDelta struct {
	Content          string           `json:"content"`
	ReasoningContent string           `json:"reasoning_content"`
	ToolCalls        []nativeToolCall `json:"tool_calls"`
}

type nativeToolCall struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

func (c *nativeChunk) isDone() bool {
	return c != nil && strings.TrimSpace(c.Text) == doneSentinel
}

func (c *nativeChunk) isStage() bool {
	return c != nil && c.Type == "stage" && len(c.Stage) > 0
}

func (c *nativeChunk) isStart() bool {
	return c != nil && c.Type == "answer"
}

func (c *nativeChunk) hasDelta() bool {
	if c == nil || c.Delta == nil {
		return false
	}
	return c.Delta.Content != "" || c.Delta.ReasoningContent != "" || len(c.Delta.ToolCalls) > 0
}

func (c *nativeChunk) isUsage() bool {
	return c != nil && c.PromptTokens != 0 && c.Delta == nil && !c.isStage() && c.Text == ""
}

func (c *nativeChunk) isCompress() bool {
	return c != nil && strings.Contains(c.Text, "<compress_pre>") && c.Delta == nil
}

// openAIDelta is the incremental payload inside a chat.completion.chunk.
type openAIDelta struct {
	Role             string           `json:"role,omitempty"`
	Content          string           `json:"content,omitempty"`
	ReasoningContent string           `json:"reasoning_content,omitempty"`
	ToolCalls        []openAIToolCall `json:"tool_calls,omitempty"`
}

type openAIToolCall struct {
	Index    int                 `json:"index"`
	ID       string              `json:"id,omitempty"`
	Type     string              `json:"type,omitempty"`
	Function *openAIToolCallFunc `json:"function,omitempty"`
}

type openAIToolCallFunc struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

type openAIChoice struct {
	Index        int          `json:"index"`
	Delta        *openAIDelta `json:"delta,omitempty"`
	Message      *openAIMsg   `json:"message,omitempty"`
	FinishReason *string      `json:"finish_reason,omitempty"`
}

type openAIMsg struct {
	Role             string           `json:"role"`
	Content          string           `json:"content"`
	ReasoningContent string           `json:"reasoning_content,omitempty"`
	ToolCalls        []openAIToolCall `json:"tool_calls,omitempty"`
}

type openAIUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	// PromptTokensDetails carries the cache counters OpenAI-compatible upstreams
	// report. It is forwarded when present so the aggregation stays faithful, and
	// the accessors below tolerate its absence.
	PromptTokensDetails *openAIPromptTokensDetails `json:"prompt_tokens_details,omitempty"`
}

type openAIPromptTokensDetails struct {
	CachedTokens     int `json:"cached_tokens,omitempty"`
	CacheWriteTokens int `json:"cache_write_tokens,omitempty"`
}

func (u *openAIUsage) cachedTokens() int {
	if u == nil || u.PromptTokensDetails == nil {
		return 0
	}
	return u.PromptTokensDetails.CachedTokens
}

func (u *openAIUsage) cacheWriteTokens() int {
	if u == nil || u.PromptTokensDetails == nil {
		return 0
	}
	return u.PromptTokensDetails.CacheWriteTokens
}

type openAICompletion struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"`
	Created int64          `json:"created"`
	Model   string         `json:"model"`
	Choices []openAIChoice `json:"choices"`
	Usage   *openAIUsage   `json:"usage,omitempty"`
}

type openAIChunk struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"`
	Created int64          `json:"created"`
	Model   string         `json:"model"`
	Choices []openAIChoice `json:"choices"`
	Usage   *openAIUsage   `json:"usage,omitempty"`
}

// sseDecoder splits a byte stream into upstream frames. It mirrors the client
// parser: frames are blank-line delimited, heartbeats are skipped and a frame's
// JSON body starts at the first "{".
type sseDecoder struct {
	buffer strings.Builder
}

// push feeds raw bytes into the decoder and returns the complete frames found
// so far. The trailing partial frame is retained for the next call.
func (d *sseDecoder) push(data []byte) []string {
	d.buffer.Write(data)
	all := strings.ReplaceAll(d.buffer.String(), "\r\n", "\n")

	var frames []string
	for {
		index := strings.Index(all, "\n\n")
		if index < 0 {
			break
		}
		frame := all[:index]
		all = all[index+2:]
		if trimmed := strings.TrimSpace(frame); trimmed != "" {
			frames = append(frames, trimmed)
		}
	}
	d.buffer.Reset()
	d.buffer.WriteString(all)
	return frames
}

// parseFrame decodes one upstream frame into a nativeChunk. It returns nil for
// heartbeats and frames that do not contain a complete JSON object, which the
// client tolerates as well.
func parseFrame(frame string) (*nativeChunk, bool) {
	for _, line := range strings.Split(frame, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, ":") {
			// SSE comment, for example the upstream ":heartbeat" keep-alive.
			continue
		}
		if strings.HasPrefix(trimmed, "data:") {
			trimmed = strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
		}
		start := strings.Index(trimmed, "{")
		if start < 0 {
			continue
		}
		payload := trimmed[start:]
		if !strings.HasSuffix(strings.TrimSpace(payload), "}") {
			return nil, false
		}
		var chunk nativeChunk
		if errUnmarshal := json.Unmarshal([]byte(payload), &chunk); errUnmarshal != nil {
			return nil, false
		}
		return &chunk, true
	}
	return nil, false
}

// nativeTranslator converts a native upstream stream into OpenAI frames.
type nativeTranslator struct {
	decoder      sseDecoder
	completionID string
	model        string
	created      int64

	// fullTextMode tracks cumulative `text` frames so only the incremental
	// suffix is emitted; the client does the same with its delta/full parser
	// split.
	lastFullText string
	sawDelta     bool

	promptTokens     int
	completionTokens int
	totalTokens      int
	finishReason     string
}

func newNativeTranslator(model string) *nativeTranslator {
	return &nativeTranslator{
		completionID: "chatcmpl-" + randomHex(16),
		model:        model,
		created:      time.Now().Unix(),
	}
}

// translate consumes upstream bytes and returns zero or more OpenAI SSE frames
// ready to be written to the client. It never returns a terminating frame; the
// caller appends `data: [DONE]` once the upstream stream ends.
func (t *nativeTranslator) translate(data []byte) [][]byte {
	var out [][]byte
	for _, frame := range t.decoder.push(data) {
		chunk, ok := parseFrame(frame)
		if !ok || chunk == nil {
			continue
		}
		out = append(out, t.translateChunk(chunk)...)
	}
	return out
}

func (t *nativeTranslator) translateChunk(chunk *nativeChunk) [][]byte {
	if chunk.ID != "" {
		t.completionID = "chatcmpl-" + chunk.ID
	}
	switch {
	case chunk.isDone():
		return nil
	case chunk.isStage():
		// Reasoning stage markers carry no user-visible text.
		return nil
	case chunk.isCompress():
		return nil
	case chunk.hasDelta():
		t.sawDelta = true
		delta := &openAIDelta{
			Content:          chunk.Delta.Content,
			ReasoningContent: chunk.Delta.ReasoningContent,
		}
		if len(chunk.Delta.ToolCalls) > 0 {
			delta.ToolCalls = make([]openAIToolCall, 0, len(chunk.Delta.ToolCalls))
			for _, call := range chunk.Delta.ToolCalls {
				item := openAIToolCall{Index: call.Index, ID: call.ID, Type: call.Type}
				if item.Type == "" {
					item.Type = "function"
				}
				item.Function = &openAIToolCallFunc{Name: call.Function.Name, Arguments: call.Function.Arguments}
				delta.ToolCalls = append(delta.ToolCalls, item)
			}
			t.finishReason = "tool_calls"
		}
		return [][]byte{t.marshalChunk(delta, nil)}
	case chunk.isStart():
		return nil
	case chunk.Text != "":
		// Cumulative full-text mode: emit only the newly appended suffix.
		increment := chunk.Text
		if t.sawDelta {
			// Deltas already delivered this text; a full-text frame here would
			// duplicate content.
			return nil
		}
		if strings.HasPrefix(chunk.Text, t.lastFullText) {
			increment = chunk.Text[len(t.lastFullText):]
		}
		t.lastFullText = chunk.Text
		if increment == "" {
			return nil
		}
		return [][]byte{t.marshalChunk(&openAIDelta{Content: increment}, nil)}
	case chunk.isUsage():
		t.promptTokens = chunk.PromptTokens
		t.completionTokens = chunk.CompleteTokens
		t.totalTokens = chunk.TotalTokens
		return nil
	}
	return nil
}

// marshalChunk renders one OpenAI chat.completion.chunk SSE frame.
func (t *nativeTranslator) marshalChunk(delta *openAIDelta, finish *string) []byte {
	chunk := openAIChunk{
		ID:      t.completionID,
		Object:  "chat.completion.chunk",
		Created: t.created,
		Model:   t.model,
		Choices: []openAIChoice{{Index: 0, Delta: delta, FinishReason: finish}},
	}
	raw, errMarshal := json.Marshal(chunk)
	if errMarshal != nil {
		return nil
	}
	return append(append([]byte("data: "), raw...), '\n', '\n')
}

// finalChunk emits the terminal content chunk carrying the finish reason and
// usage, followed by the SSE done marker.
func (t *nativeTranslator) finalFrames() [][]byte {
	// Flush a final SSE event even when the upstream omits the blank line.
	trailing := t.translate([]byte("\n\n"))
	reason := t.finishReason
	if reason == "" {
		reason = "stop"
	}
	frames := append(trailing, t.marshalChunk(&openAIDelta{}, &reason))
	if t.totalTokens > 0 || t.promptTokens > 0 || t.completionTokens > 0 {
		usage := &openAIUsage{
			PromptTokens:     t.promptTokens,
			CompletionTokens: t.completionTokens,
			TotalTokens:      t.totalTokens,
		}
		chunk := openAIChunk{
			ID:      t.completionID,
			Object:  "chat.completion.chunk",
			Created: t.created,
			Model:   t.model,
			Choices: []openAIChoice{},
			Usage:   usage,
		}
		if raw, errMarshal := json.Marshal(chunk); errMarshal == nil {
			frames = append(frames, append(append([]byte("data: "), raw...), '\n', '\n'))
		}
	}
	frames = append(frames, []byte("data: [DONE]\n\n"))
	return frames
}

// aggregate collects translated frames into a single non-streaming completion.
type aggregator struct {
	id      string
	model   string
	created int64

	content      strings.Builder
	reasoning    strings.Builder
	toolCalls    []openAIToolCall
	usage        *openAIUsage
	finishReason string
}

func newAggregator(id, model string) *aggregator {
	if id == "" {
		id = "chatcmpl-" + randomHex(16)
	}
	return &aggregator{id: id, model: model, created: time.Now().Unix()}
}

// addFrame consumes one upstream frame in OpenAI chunk form.
func (a *aggregator) addFrame(frame []byte) {
	payload := strings.TrimSpace(string(frame))
	payload = strings.TrimPrefix(payload, "data:")
	payload = strings.TrimSpace(payload)
	if payload == "" || payload == doneSentinel {
		return
	}
	var chunk openAIChunk
	if errUnmarshal := json.Unmarshal([]byte(payload), &chunk); errUnmarshal != nil {
		return
	}
	if chunk.ID != "" {
		a.id = chunk.ID
	}
	if chunk.Model != "" {
		a.model = chunk.Model
	}
	if chunk.Usage != nil {
		a.usage = chunk.Usage
	}
	for _, choice := range chunk.Choices {
		if choice.Delta != nil {
			a.content.WriteString(choice.Delta.Content)
			a.reasoning.WriteString(choice.Delta.ReasoningContent)
			if len(choice.Delta.ToolCalls) > 0 {
				a.mergeToolCalls(choice.Delta.ToolCalls)
			}
		}
		if choice.FinishReason != nil && *choice.FinishReason != "" {
			a.finishReason = *choice.FinishReason
		}
	}
}

// mergeToolCalls accumulates streamed tool-call fragments by index.
func (a *aggregator) mergeToolCalls(incoming []openAIToolCall) {
	for _, call := range incoming {
		for len(a.toolCalls) <= call.Index {
			a.toolCalls = append(a.toolCalls, openAIToolCall{Index: len(a.toolCalls)})
		}
		target := &a.toolCalls[call.Index]
		if call.ID != "" {
			target.ID = call.ID
		}
		if call.Type != "" {
			target.Type = call.Type
		}
		if call.Function != nil {
			if target.Function == nil {
				target.Function = &openAIToolCallFunc{}
			}
			if call.Function.Name != "" {
				target.Function.Name = call.Function.Name
			}
			// Arguments arrive as partial JSON strings that must be joined.
			target.Function.Arguments += call.Function.Arguments
		}
	}
}

// completion renders the aggregated result as a non-streaming response body.
func (a *aggregator) completion() []byte {
	reason := a.finishReason
	if reason == "" {
		if len(a.toolCalls) > 0 {
			reason = "tool_calls"
		} else {
			reason = "stop"
		}
	}
	message := &openAIMsg{
		Role:    "assistant",
		Content: a.content.String(),
	}
	if reasoning := a.reasoning.String(); reasoning != "" {
		message.ReasoningContent = reasoning
	}
	if len(a.toolCalls) > 0 {
		message.ToolCalls = a.toolCalls
	}
	body := openAICompletion{
		ID:      a.id,
		Object:  "chat.completion",
		Created: a.created,
		Model:   a.model,
		Choices: []openAIChoice{{Index: 0, Message: message, FinishReason: &reason}},
		Usage:   a.usage,
	}
	raw, errMarshal := json.Marshal(body)
	if errMarshal != nil {
		return []byte(`{"error":{"message":"failed to encode completion","type":"server_error"}}`)
	}
	return raw
}

// agentPassthrough translates the OpenAI-compatible agent-mode stream. Those
// frames are already OpenAI SSEn so they only need usage bookkeeping for the
// non-streaming aggregation path.
func isDoneFrame(frame []byte) bool {
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(frame)), "data:")) == doneSentinel
}
