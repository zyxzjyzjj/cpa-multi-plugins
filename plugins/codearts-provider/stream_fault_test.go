package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

// The envelope below is captured live from an exhausted benefit allowance on
// 2026-09-21, inside an HTTP 200 SSE body that otherwise ended cleanly.
const quotaFaultFrame = `{"error_code":"InferHub.4291.200","error_msg":"insufficient quota","details":[{"error_code":"InferHub.4291.200","error_msg":"requestId: abe754a5417344caa306d5b38a57923b"},{"error_code":"InferHub.4291.200","error_msg":"timestamps: 202609211921"},{"error_code":"InferHub.4291.200","error_msg":"modelId: glm-5.3-flash"},{"error_code":"InferHub.4291.200","error_msg":"traceId: 17c59748cd004b6fadf4660383301ad1"}]}`

func TestStreamFrameFaultClassifiesQuotaEnvelope(t *testing.T) {
	fault := streamFrameFault([]byte(quotaFaultFrame))
	if fault == nil {
		t.Fatal("an in-stream quota envelope was not detected")
	}
	if fault.Code != "insufficient_quota" {
		t.Errorf("code = %q, want insufficient_quota", fault.Code)
	}
	if fault.Status != http.StatusForbidden {
		t.Errorf("status = %d, want %d", fault.Status, http.StatusForbidden)
	}
	if !strings.Contains(fault.Message, "InferHub.4291.200") || !strings.Contains(fault.Message, "insufficient quota") {
		t.Errorf("message lost the upstream diagnosis: %q", fault.Message)
	}
	// The details only repeat the code alongside trace decorations, which are
	// noise for an operator and must not crowd out the real message.
	if strings.Contains(fault.Message, "requestId") || strings.Contains(fault.Message, "traceId") {
		t.Errorf("message leaked trace decorations: %q", fault.Message)
	}
}

func TestStreamFrameFaultIgnoresTraffic(t *testing.T) {
	cases := map[string]string{
		"empty":              "",
		"done sentinel":      "[DONE]",
		"malformed":          `{"error_code":`,
		"success envelope":   `{"error_code":"0000","error_msg":"success","result":{"daily_token_limit":1}}`,
		"zero code":          `{"error_code":"0","choices":[{"delta":{"content":"hi"}}]}`,
		"plain chunk":        `{"choices":[{"index":0,"delta":{"content":"2"},"finish_reason":"stop"}]}`,
		"usage chunk":        `{"id":"x","model":"glm-5.2","usage":{"total_tokens":9}}`,
		"content plus code":  `{"error_code":"InferHub.4291.200","choices":[{"delta":{"content":"partial"}}]}`,
		"native stage frame": `{"type":"stage","stage":[{"name":"plan"}]}`,
	}
	for name, payload := range cases {
		if fault := streamFrameFault([]byte(payload)); fault != nil {
			t.Errorf("%s: invented a fault: %+v", name, fault)
		}
	}
}

func TestStreamFrameFaultRateLimitAndUnknownClassification(t *testing.T) {
	rate := streamFrameFault([]byte(`{"error_code":"Gate.429","error_msg":"Too Many Requests"}`))
	if rate == nil || rate.Code != "rate_limit_exceeded" || rate.Status != http.StatusTooManyRequests {
		t.Fatalf("rate limit classification failed: %+v", rate)
	}
	unknown := streamFrameFault([]byte(`{"error_code":"InferHub.5000","error_msg":"backend exploded"}`))
	if unknown == nil || unknown.Code != "upstream_error" || unknown.Status != http.StatusBadGateway {
		t.Fatalf("unknown classification failed: %+v", unknown)
	}
}

func TestAggregateUpstreamFailsOnInStreamQuota(t *testing.T) {
	cfg := defaultConfig()
	cfg.DiscoverModels = false
	body := []byte("data: " + quotaFaultFrame + "\n\n" + "data: [DONE]\n\n")

	payload, errAggregate := aggregateUpstream(cfg, "glm-5.3-flash", body)
	if errAggregate == nil {
		t.Fatalf("exhausted allowance aggregated into a success: %s", payload)
	}
	var fault *upstreamStreamFault
	if !errors.As(errAggregate, &fault) {
		t.Fatalf("aggregation error is not the stream fault: %#v", errAggregate)
	}
	if fault.Code != "insufficient_quota" {
		t.Errorf("code = %q", fault.Code)
	}
}

func TestAggregateUpstreamStillReapsRealContent(t *testing.T) {
	cfg := defaultConfig()
	cfg.DiscoverModels = false
	body := []byte("data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"2\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")

	payload, errAggregate := aggregateUpstream(cfg, "glm-5.2", body)
	if errAggregate != nil {
		t.Fatalf("a normal completion was rejected: %v", errAggregate)
	}
	var completion openAICompletion
	if errUnmarshal := json.Unmarshal(payload, &completion); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	if len(completion.Choices) == 0 || completion.Choices[0].Message.Content != "2" {
		t.Fatalf("lost content: %s", payload)
	}
}

func TestUpstreamFailureEnvelopeCarriesFaultClassification(t *testing.T) {
	fault := &upstreamStreamFault{Code: "insufficient_quota", Message: "upstream reported InferHub.4291.200: insufficient quota", Status: http.StatusForbidden}
	raw, errEnvelope := upstreamFailureEnvelope(fault, nil)
	if errEnvelope != nil {
		t.Fatal(errEnvelope)
	}
	var decoded struct {
		OK    bool `json:"ok"`
		Error *struct {
			Code       string `json:"code"`
			HTTPStatus int    `json:"http_status"`
		} `json:"error"`
	}
	if errUnmarshal := json.Unmarshal(raw, &decoded); errUnmarshal != nil {
		t.Fatalf("envelope shape: %s (%v)", raw, errUnmarshal)
	}
	if decoded.OK || decoded.Error == nil {
		t.Fatalf("no error reported: %s", raw)
	}
	if decoded.Error.Code != "insufficient_quota" || decoded.Error.HTTPStatus != http.StatusForbidden {
		t.Errorf("classification lost in the envelope: %s", raw)
	}

	// A plain error keeps the previous generic mapping.
	plain, errPlain := upstreamFailureEnvelope(errPlainProbe{}, nil)
	if errPlain != nil {
		t.Fatal(errPlain)
	}
	if !bytes.Contains(plain, []byte("upstream_error")) || !bytes.Contains(plain, []byte("502")) {
		t.Errorf("plain error was remapped: %s", plain)
	}
}

type errPlainProbe struct{}

func (errPlainProbe) Error() string { return "upstream returned an empty response" }

func TestStreamRendererWithholdsFaultFrameInAgentMode(t *testing.T) {
	cfg := defaultConfig()
	cfg.DiscoverModels = false
	renderer := newStreamRenderer(cfg, "glm-5.3-flash", protocolOpenAI)
	frames := renderer.feed([]byte("data: " + quotaFaultFrame + "\n\ndata: [DONE]\n\n"))
	frames = append(frames, renderer.finish()...)

	for _, frame := range frames {
		if bytes.Contains(frame, []byte("InferHub.4291.200")) {
			t.Fatalf("the fault envelope was forwarded as content: %s", frame)
		}
	}
	if renderer.streamFault() == nil {
		t.Fatal("the renderer forwarded the stream without recording the fault")
	}
}

func TestBufferedStreamReportsFault(t *testing.T) {
	cfg := defaultConfig()
	cfg.DiscoverModels = false
	raw, errBuffered := bufferedStreamResponse(cfg, "glm-5.3-flash", protocolOpenAI,
		[]byte("data: "+quotaFaultFrame+"\n\ndata: [DONE]\n\n"))
	if errBuffered != nil {
		t.Fatal(errBuffered)
	}
	if !bytes.Contains(raw, []byte("insufficient_quota")) {
		t.Fatalf("buffered stream returned an envelope without the fault: %s", raw)
	}
}
