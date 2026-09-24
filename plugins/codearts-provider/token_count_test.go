package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func tokenCountPayload(t *testing.T, req executorRequest) map[string]json.RawMessage {
	t.Helper()
	testHost(t, func(method string, request any) (json.RawMessage, error) {
		t.Errorf("token counting must stay offline: %s", method)
		return nil, fmt.Errorf("unexpected host callback")
	})
	raw, err := executorCountTokens(mustMarshal(t, req))
	response := testEnvelopeResult[pluginapi.ExecutorResponse](t, raw, err)
	if response.Headers.Get("Content-Type") != "application/json" {
		t.Fatal("token count response lost its content type")
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(response.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	return payload
}

func countFixture(t *testing.T, input any) inputTokenEstimate {
	t.Helper()
	req := executorRequest{}
	req.Payload = mustMarshal(t, input)
	return countInputTokens(req)
}

func TestCountTokensCountsTranslatedInputOnly(t *testing.T) {
	payload := []byte(`{"messages":[{"role":"user","content":"你好，请检查这段代码。"}]}`)
	for _, protocol := range []string{"openai", "claude"} {
		t.Run(protocol, func(t *testing.T) {
			req := executorRequest{}
			req.Format = protocol
			req.Payload = bytes.Clone(payload)
			base := tokenCountPayload(t, req)
			var total int
			_ = json.Unmarshal(base["total_tokens"], &total)
			if total <= 0 || string(base["estimated"]) != "true" {
				t.Fatalf("missing estimate or prompt tokens: %s", mustMarshal(t, base))
			}
			if protocol == "claude" && !bytes.Equal(base["input_tokens"], base["total_tokens"]) {
				t.Fatal("Anthropic count must use the same input estimate")
			}
			for _, original := range [][]byte{payload, []byte(`{"messages":[{"role":"user","content":"` + strings.Repeat("original-only ", 1000) + `"}]}`)} {
				req.OriginalRequest = original
				actual := tokenCountPayload(t, req)
				if !bytes.Equal(actual["total_tokens"], base["total_tokens"]) {
					t.Fatal("original request was counted again or overrode the translated payload")
				}
			}
			if !bytes.Equal(req.Payload, payload) {
				t.Fatal("token counting mutated the request")
			}
		})
	}
}

func TestCountTokensFallbackDoesNotOverrideEmptyTranslatedInput(t *testing.T) {
	original := []byte(`{"messages":[{"role":"user","content":"hello"}]}`)
	req := executorRequest{}
	req.Payload = original
	want := countInputTokens(req).Tokens
	req.OriginalRequest = original
	for _, absent := range [][]byte{nil, []byte("  \n"), []byte("null")} {
		req.Payload = absent
		if got := countInputTokens(req).Tokens; got != want {
			t.Fatalf("missing payload did not fall back to the original: got %d want %d", got, want)
		}
	}
	for _, empty := range []string{`{}`, `{"messages":[]}`} {
		req.Payload = []byte(empty)
		if got := countInputTokens(req).Tokens; got != 0 {
			t.Fatalf("an explicitly empty translated request must not revive original history: %d", got)
		}
	}
	if got := countInputTokens(executorRequest{}).Tokens; got != 0 {
		t.Fatalf("empty request counted %d tokens", got)
	}
}

func TestCountTokensIgnoresTransportFormattingAndOptions(t *testing.T) {
	req := executorRequest{}
	req.Payload = []byte(`{"messages":[{"role":"user","content":"中文\n<hi>🙂"}]}`)
	want := countInputTokens(req).Tokens
	req.Payload = []byte("{\n  \"messages\": [ { \"content\": \"\\u4e2d\\u6587\\n\\u003chi\\u003e\\ud83d\\ude42\", \"role\": \"user\" } ],\n" +
		`"model":"irrelevant","stream":true,"stream_options":{"include_usage":true},"max_tokens":100000,"temperature":0.1,"metadata":{"trace":"` + strings.Repeat("metadata", 10000) + `"}}`)
	if got := countInputTokens(req).Tokens; got != want {
		t.Fatalf("JSON encoding or non-prompt options changed tokens: got %d want %d", got, want)
	}
}

func TestCountTokensIncludesCJKHistoryAndToolOutput(t *testing.T) {
	text := strings.Repeat("中文", 500)
	message := map[string]any{"role": "user", "content": text}
	one := countFixture(t, map[string]any{"messages": []any{message}}).Tokens
	if one < 1000 {
		t.Fatalf("CJK was still treated as four-byte English tokens: %d", one)
	}
	two := countFixture(t, map[string]any{"messages": []any{message, message}}).Tokens
	if two <= one || two > one*2 {
		t.Fatalf("deliberate repeated messages must each count once: one=%d two=%d", one, two)
	}
	withToolOutput := countFixture(t, map[string]any{"messages": []any{message,
		map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": "call1", "type": "function", "function": map[string]any{"name": "lookup", "arguments": `{"query":"中文"}`}}}},
		map[string]any{"role": "tool", "tool_call_id": "call1", "content": text},
	}}).Tokens
	if withToolOutput <= two {
		t.Fatal("tool call arguments/identity and tool output were omitted")
	}
	if again := countFixture(t, map[string]any{"messages": []any{message}}).Tokens; again != one {
		t.Fatal("history leaked from an earlier count request")
	}
}

func TestCountTokensToolDefinitionsAndSystemHaveEquivalentForms(t *testing.T) {
	schema := map[string]any{"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string", "description": "中文 <query>"}}}
	normalized := map[string]any{
		"messages": []any{map[string]any{"role": "system", "content": "Be helpful."}, map[string]any{"role": "user", "content": "hi"}},
		"tools":    []any{map[string]any{"type": "function", "function": map[string]any{"name": "lookup", "description": "Find results", "parameters": schema}}},
	}
	original := map[string]any{
		"system":   []any{map[string]any{"type": "text", "text": "Be helpful.", "cache_control": map[string]any{"type": "ephemeral"}}},
		"messages": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "hi"}}}},
		"tools":    []any{map[string]any{"name": "lookup", "description": "Find results", "input_schema": schema}},
	}
	want := countFixture(t, normalized).Tokens
	if got := countFixture(t, original).Tokens; got != want {
		t.Fatalf("equivalent system/tool definitions differ: got %d want %d", got, want)
	}
	delete(normalized, "tools")
	if withoutTools := countFixture(t, normalized).Tokens; withoutTools >= want {
		t.Fatal("tool definition and schema did not contribute input tokens")
	}
}

func TestCountTokensResponsesFallbackCountsToolCallOnce(t *testing.T) {
	call := map[string]any{"type": "function_call", "id": "transport-item-not-a-prompt-id", "call_id": "call1", "name": "lookup", "arguments": `{"q":"hi"}`}
	responses := map[string]any{"instructions": "Be helpful.", "input": []any{map[string]any{"role": "user", "content": "hello"}, call, map[string]any{"type": "function_call_output", "call_id": "call1", "output": "found"}}}
	chat := map[string]any{"messages": []any{
		map[string]any{"role": "system", "content": "Be helpful."}, map[string]any{"role": "user", "content": "hello"},
		map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": "call1", "type": "function", "function": map[string]any{"name": "lookup", "arguments": `{"q":"hi"}`}}}},
		map[string]any{"role": "tool", "tool_call_id": "call1", "content": "found"},
	}}
	if got, want := countFixture(t, responses).Tokens, countFixture(t, chat).Tokens; got != want {
		t.Fatalf("Responses item wrapper or function name was counted twice: got %d want %d", got, want)
	}
}

func TestCountTokensDoesNotTokenizeEncodedImages(t *testing.T) {
	var baseline int
	for _, source := range []string{"data:image/png;base64,AQ==", "data:image/png;base64," + strings.Repeat("A", 1024*1024), "https://example.invalid/image.png"} {
		result := countFixture(t, map[string]any{"messages": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "image_url", "image_url": map[string]any{"url": source}}}}}})
		if baseline == 0 {
			baseline = result.Tokens
		}
		if result.Tokens != baseline || len(result.Warnings) == 0 {
			t.Fatalf("encoded image/URL counted as text or lacks uncertainty notice: %+v", result)
		}
	}
	image := map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": "AQ=="}}
	result := countFixture(t, map[string]any{"messages": []any{map[string]any{"role": "user", "content": []any{image, image}}}})
	if result.Tokens <= baseline || result.Tokens > baseline*2 {
		t.Fatal("multiple images did not contribute separate media estimates")
	}
}

func TestCountTokensTextDocumentsUseTheirContent(t *testing.T) {
	document := func(text string) any {
		return map[string]any{"messages": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "document", "source": map[string]any{"type": "text", "data": text}}}}}}
	}
	short := countFixture(t, document("hi"))
	long := countFixture(t, document(strings.Repeat("中文", 1000)))
	if long.Tokens <= short.Tokens || len(long.Warnings) != 0 {
		t.Fatal("available document text was replaced with an opaque-media placeholder")
	}
}
