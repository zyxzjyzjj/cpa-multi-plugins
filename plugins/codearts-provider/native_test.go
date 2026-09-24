package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestNativeTranslatorDeltaStream covers the delta protocol: frames carry
// incremental content, reasoning and tool calls, and the stream terminates with
// the upstream "[DONE]" text frame followed by our own SSE done marker.
func TestNativeTranslatorDeltaStream(t *testing.T) {
	upstream := strings.Join([]string{
		`{"type":"stage","stage":["understanding"],"chat_id":"abc"}`,
		``,
		`{"type":"answer","id":"resp1"}`,
		``,
		`{"delta":{"reasoning_content":"thinking..."}}`,
		``,
		`{"delta":{"content":"Hello"}}`,
		``,
		`{"delta":{"content":" world"}}`,
		``,
		`{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}`,
		``,
		`{"text":"[DONE]"}`,
		``,
	}, "\n")

	translator := newNativeTranslator("test-model")
	frames := translator.translate([]byte(upstream))
	frames = append(frames, translator.finalFrames()...)

	var content, reasoning strings.Builder
	var usage *openAIUsage
	var sawDone bool
	var finish string
	for _, frame := range frames {
		payload := decodeSSEPayload(t, frame)
		if payload == doneSentinel {
			sawDone = true
			continue
		}
		var chunk openAIChunk
		if errUnmarshal := json.Unmarshal([]byte(payload), &chunk); errUnmarshal != nil {
			t.Fatalf("frame %q is not a valid chat.completion.chunk: %v", payload, errUnmarshal)
		}
		if chunk.Object != "chat.completion.chunk" {
			t.Fatalf("unexpected object type %q", chunk.Object)
		}
		if chunk.Usage != nil {
			usage = chunk.Usage
		}
		for _, choice := range chunk.Choices {
			if choice.Delta != nil {
				content.WriteString(choice.Delta.Content)
				reasoning.WriteString(choice.Delta.ReasoningContent)
			}
			if choice.FinishReason != nil && *choice.FinishReason != "" {
				finish = *choice.FinishReason
			}
		}
	}

	if got := content.String(); got != "Hello world" {
		t.Errorf("content = %q, want %q", got, "Hello world")
	}
	if got := reasoning.String(); got != "thinking..." {
		t.Errorf("reasoning = %q, want %q", got, "thinking...")
	}
	if usage == nil || usage.TotalTokens != 18 {
		t.Errorf("usage = %+v, want total_tokens 18", usage)
	}
	if !sawDone {
		t.Error("stream did not end with the SSE done marker")
	}
	if finish != "stop" {
		t.Errorf("finish_reason = %q, want %q", finish, "stop")
	}
}

// TestNativeTranslatorFullTextStream covers the cumulative full-text protocol,
// where each frame repeats the entire answer and only the suffix is new.
func TestNativeTranslatorFullTextStream(t *testing.T) {
	translator := newNativeTranslator("test-model")
	var content strings.Builder
	for _, frame := range []string{
		`{"text":"Hel"}` + "\n\n",
		`{"text":"Hello"}` + "\n\n",
		`{"text":"Hello there"}` + "\n\n",
		`{"text":"[DONE]"}` + "\n\n",
	} {
		for _, out := range translator.translate([]byte(frame)) {
			payload := decodeSSEPayload(t, out)
			var chunk openAIChunk
			if errUnmarshal := json.Unmarshal([]byte(payload), &chunk); errUnmarshal != nil {
				t.Fatalf("invalid chunk: %v", errUnmarshal)
			}
			for _, choice := range chunk.Choices {
				if choice.Delta != nil {
					content.WriteString(choice.Delta.Content)
				}
			}
		}
	}
	if got := content.String(); got != "Hello there" {
		t.Fatalf("content = %q, want %q", got, "Hello there")
	}
}

// TestNativeTranslatorSkipsHeartbeatAndToleratesSplitFrames verifies that
// heartbeat comments are ignored and that frames split across reads are
// reassembled.
func TestNativeTranslatorSkipsHeartbeatAndToleratesSplitFrames(t *testing.T) {
	translator := newNativeTranslator("test-model")

	// Heartbeat frames carry no JSON body.
	if frames := translator.translate([]byte(":heartbeat\n\n")); len(frames) != 0 {
		t.Fatalf("heartbeat produced %d frames, want 0", len(frames))
	}

	// Send one JSON frame split across three reads.
	part := `{"delta":{"content":"split"}}`
	var frames [][]byte
	frames = append(frames, translator.translate([]byte(part[:10]))...)
	frames = append(frames, translator.translate([]byte(part[10:20]))...)
	frames = append(frames, translator.translate([]byte(part[20:]+"\n\n"))...)

	var content strings.Builder
	for _, frame := range frames {
		var chunk openAIChunk
		if errUnmarshal := json.Unmarshal([]byte(decodeSSEPayload(t, frame)), &chunk); errUnmarshal != nil {
			t.Fatalf("invalid chunk: %v", errUnmarshal)
		}
		for _, choice := range chunk.Choices {
			if choice.Delta != nil {
				content.WriteString(choice.Delta.Content)
			}
		}
	}
	if got := content.String(); got != "split" {
		t.Fatalf("split-frame content = %q, want %q", got, "split")
	}
}

func TestNativeTranslatorToolCalls(t *testing.T) {
	translator := newNativeTranslator("test-model")
	upstream := `{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"read_file","arguments":"{\"pa"}}]}}` + "\n\n" +
		`{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"th\":\"a.go\"}"}}]}}` + "\n\n" +
		`{"text":"[DONE]"}` + "\n\n"

	aggregator := newAggregator("resp", "test-model")
	for _, frame := range translator.translate([]byte(upstream)) {
		aggregator.addFrame(frame)
	}
	body := aggregator.completion()

	var completion openAICompletion
	if errUnmarshal := json.Unmarshal(body, &completion); errUnmarshal != nil {
		t.Fatalf("aggregated completion is not valid JSON: %v", errUnmarshal)
	}
	if len(completion.Choices) != 1 || completion.Choices[0].Message == nil {
		t.Fatalf("completion has no message: %s", body)
	}
	message := completion.Choices[0].Message
	if len(message.ToolCalls) != 1 {
		t.Fatalf("tool calls = %d, want 1", len(message.ToolCalls))
	}
	call := message.ToolCalls[0]
	if call.Function == nil || call.Function.Name != "read_file" {
		t.Fatalf("tool call function = %+v, want read_file", call.Function)
	}
	// Streamed argument fragments must be concatenated, not replaced.
	if call.Function.Arguments != `{"path":"a.go"}` {
		t.Fatalf("arguments = %q, want %q", call.Function.Arguments, `{"path":"a.go"}`)
	}
	if completion.Choices[0].FinishReason == nil || *completion.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("finish_reason = %v, want tool_calls", completion.Choices[0].FinishReason)
	}
}

// TestAggregatorAgentModePassthrough covers the OpenAI-compatible agent mode,
// where upstream frames are already OpenAI chunks.
func TestAggregatorAgentModePassthrough(t *testing.T) {
	body := strings.Join([]string{
		`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"Hi"}}]}`,
		``,
		`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"content":" there"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	renderer := newStreamRenderer(&Config{APIMode: "agent"}, "m", protocolOpenAI)
	aggregator := newAggregator("", "m")
	for _, frame := range renderer.feed([]byte(body)) {
		aggregator.addFrame(frame)
	}
	// The trailing done marker must not corrupt the aggregate.
	aggregator.addFrame([]byte("data: [DONE]\n\n"))

	var completion openAICompletion
	if errUnmarshal := json.Unmarshal(aggregator.completion(), &completion); errUnmarshal != nil {
		t.Fatalf("invalid completion: %v", errUnmarshal)
	}
	if completion.Choices[0].Message.Content != "Hi there" {
		t.Fatalf("content = %q, want %q", completion.Choices[0].Message.Content, "Hi there")
	}
	if completion.Usage == nil || completion.Usage.TotalTokens != 5 {
		t.Fatalf("usage = %+v, want total_tokens 5", completion.Usage)
	}
}

// decodeSSEPayload strips the "data: " prefix and returns the JSON payload, or
// the "[DONE]" sentinel.
func decodeSSEPayload(t *testing.T, frame []byte) string {
	t.Helper()
	text := strings.TrimSpace(string(frame))
	if !strings.HasPrefix(text, "data: ") {
		t.Fatalf("frame %q does not start with an SSE data prefix", text)
	}
	return strings.TrimSpace(strings.TrimPrefix(text, "data: "))
}
