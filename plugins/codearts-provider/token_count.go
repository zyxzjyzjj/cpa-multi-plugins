package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"unicode"
	"unicode/utf8"
)

// These are local estimates of framing and media overhead, not a particular
// model's tokenizer. Never use them as a hard guarantee that input will fit.
const (
	estimatedMessageTokens = 4
	estimatedReplyTokens   = 3
	estimatedToolTokens    = 8
	estimatedMediaTokens   = 2048
)

type inputTokenEstimate struct {
	Tokens   int
	Warnings []string
}

type promptTokenCounter struct {
	tokens int
	media  bool
}

func countInputTokens(req executorRequest) inputTokenEstimate {
	// CPA supplies the translated payload and the original client request.
	// They describe the same input: count the payload alone, falling back only
	// when it is absent. An intentionally empty object/array remains authoritative.
	body := bytes.TrimSpace(req.Payload)
	if len(body) == 0 || bytes.Equal(body, []byte("null")) {
		body = bytes.TrimSpace(req.OriginalRequest)
	}
	if len(body) == 0 || bytes.Equal(body, []byte("null")) {
		return inputTokenEstimate{}
	}
	var request map[string]any
	if json.Unmarshal(body, &request) != nil {
		return inputTokenEstimate{Tokens: estimateTextTokens(string(body)), Warnings: []string{"Unstructured input was estimated as text"}}
	}
	c := &promptTokenCounter{}
	for _, field := range []string{"system", "instructions"} {
		if value := request[field]; value != nil {
			c.message(map[string]any{"role": "system", "content": value})
		}
	}
	if messages, ok := request["messages"].([]any); ok {
		for _, message := range messages {
			c.message(message)
		}
	}
	// Responses-style originals are supported when no translated payload exists.
	if input, ok := request["input"].([]any); ok {
		for _, item := range input {
			c.message(item)
		}
	} else if input := request["input"]; input != nil {
		c.message(map[string]any{"role": "user", "content": input})
	}
	if prompt := request["prompt"]; prompt != nil {
		c.content(prompt)
	}
	for _, field := range []string{"tools", "functions"} {
		if tools, ok := request[field].([]any); ok {
			for _, value := range tools {
				tool, ok := value.(map[string]any)
				if !ok {
					continue
				}
				if function, ok := tool["function"].(map[string]any); ok {
					tool = function
				}
				c.tokens += estimatedToolTokens
				for _, name := range []string{"name", "description", "type"} {
					c.text(tool[name])
				}
				if schema, ok := tool["parameters"]; ok {
					c.data(schema)
				} else {
					c.data(tool["input_schema"])
				}
			}
		}
	}
	if format, ok := request["response_format"].(map[string]any); ok {
		if schema, ok := format["json_schema"]; ok {
			c.data(schema)
		}
	}
	if c.tokens > 0 {
		c.tokens += estimatedReplyTokens
	}
	result := inputTokenEstimate{Tokens: c.tokens}
	if c.media {
		result.Warnings = []string{"Images and other opaque media use a 2048-token placeholder each; actual usage depends on the model and media. URLs are not fetched"}
	}
	return result
}

func (c *promptTokenCounter) text(value any) {
	if text, ok := value.(string); ok {
		c.tokens += estimateTextTokens(text)
	}
}

// Tool schemas and structured tool inputs are model-visible data. Marshal only
// these objects, removing transport JSON whitespace/escaping from the estimate.
func (c *promptTokenCounter) data(value any) {
	if value != nil {
		var encoded bytes.Buffer
		encoder := json.NewEncoder(&encoded)
		encoder.SetEscapeHTML(false)
		_ = encoder.Encode(value)
		c.tokens += estimateTextTokens(strings.TrimSpace(encoded.String()))
	}
}

func (c *promptTokenCounter) message(value any) {
	message, ok := value.(map[string]any)
	if !ok {
		c.content(value)
		return
	}
	c.tokens += estimatedMessageTokens
	role := message["role"]
	if role == nil {
		switch message["type"] {
		case "function_call":
			role = "assistant"
		case "function_call_output":
			role = "tool"
		default:
			role = "user"
		}
	}
	c.text(role)
	switch message["type"] {
	case "function_call":
		c.toolCall(message)
		return
	case "function_call_output":
		c.text(message["call_id"])
		c.content(message["output"])
		return
	}
	c.text(message["name"])
	c.text(message["tool_call_id"])
	c.content(message["content"])
	c.text(message["reasoning_content"])
	if calls, ok := message["tool_calls"].([]any); ok {
		for _, call := range calls {
			c.toolCall(call)
		}
	}
	if call := message["function_call"]; call != nil {
		c.toolCall(call)
	}
	switch message["type"] {
	case "input_text", "input_image", "input_file":
		c.content(message)
	}
}

func (c *promptTokenCounter) toolCall(value any) {
	call, ok := value.(map[string]any)
	if !ok {
		return
	}
	c.tokens += estimatedToolTokens
	if id, ok := call["call_id"]; ok {
		c.text(id)
	} else {
		c.text(call["id"])
	}
	if function, ok := call["function"].(map[string]any); ok {
		call = function
	}
	c.text(call["name"])
	if arguments, ok := call["arguments"].(string); ok {
		c.text(arguments)
	} else if input, ok := call["input"]; ok {
		c.data(input)
	} else {
		c.data(call["arguments"])
	}
}

func (c *promptTokenCounter) content(value any) {
	switch content := value.(type) {
	case string:
		c.text(content)
	case []any:
		for _, block := range content {
			c.content(block)
		}
	case map[string]any:
		switch content["type"] {
		case "text", "input_text", "output_text":
			c.text(content["text"])
		case "document":
			c.text(content["title"])
			c.text(content["context"])
			source, _ := content["source"].(map[string]any)
			switch source["type"] {
			case "text":
				c.text(source["data"])
			case "content":
				c.content(source["content"])
			default:
				c.tokens += estimatedMediaTokens
				c.media = true
			}
		case "image", "image_url", "input_image", "input_audio", "audio", "video", "video_url", "file", "input_file", "redacted_thinking":
			// Base64/URLs are transport representations, not prompt text. The
			// placeholder is explicitly flagged because resolution/duration and
			// the provider's multimodal tokenizer cannot be inferred locally.
			c.tokens += estimatedMediaTokens
			c.media = true
		case "tool_use":
			c.toolCall(content)
		case "tool_result":
			c.tokens += estimatedToolTokens
			c.text(content["tool_use_id"])
			c.content(content["content"])
		case "thinking":
			c.text(content["thinking"])
		default:
			c.data(content)
		}
	}
}

// Unicode-aware fallback for the actual text, after decoding JSON. ASCII text
// is grouped by roughly four characters, digits by three, punctuation by two.
// CJK/other non-ASCII letters count individually; astral symbols such as emoji
// get two tokens. Vocabulary, framing and reasoning rules still vary by model.
func estimateTextTokens(text string) int {
	total, run, divisor := 0, 0, 4
	flush := func() {
		total += (run + divisor - 1) / divisor
		run = 0
	}
	for _, char := range text {
		if char >= utf8.RuneSelf {
			flush()
			if utf8.RuneLen(char) == 4 && !unicode.IsLetter(char) {
				total += 2
			} else {
				total++
			}
			continue
		}
		next := 4
		if char >= '0' && char <= '9' {
			next = 3
		} else if !unicode.IsLetter(char) && !strings.ContainsRune(" \t\n\r", char) {
			next = 2
		}
		if next != divisor {
			flush()
			divisor = next
		}
		run++
	}
	flush()
	return total
}
