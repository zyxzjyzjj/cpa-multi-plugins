package main

// The gateway reports quota and rate-limit failures inside an HTTP 200 stream.
// Those bytes used to arrive after the response status had already been handed to
// the client, so the host saw a successful empty stream and kept sending traffic
// to the exhausted account. These tests pin the gate that turns a pre-answer
// envelope back into a failed request the host can route around.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

const answerFrame = "data: {\"id\":\"1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"answer\"}}]}\n\n"

type gateHost struct {
	mu      sync.Mutex
	frames  []hostHTTPStreamChunk
	next    int
	emitted [][]byte
	errs    []string
	closes  int
}

// gateQuotaFrame is the captured live envelope wrapped in the SSE framing upstream uses.
const gateQuotaFrame = "data: " + quotaFaultFrame + "\n\n"

// runGateStream drives one async executor stream against a scripted upstream.
func runGateStream(t *testing.T, frames []hostHTTPStreamChunk) (envelope, *gateHost) {
	return runGateStreamAs(t, frames, "agent", protocolOpenAI)
}

func runGateStreamAs(t *testing.T, frames []hostHTTPStreamChunk, mode, format string) (envelope, *gateHost) {
	t.Helper()
	cfg := defaultConfig()
	cfg.APIMode = mode
	cfg.DiscoverModels = false
	cfg.ChatSessionHeartbeat = false
	useModelTestConfig(t, cfg)
	cred := credential{AccessKeyID: "gate-ak", SecretAccessKey: "gate-sk", SecurityToken: "gate-sts"}
	host := &gateHost{frames: frames}
	testHost(t, func(method string, request any) (json.RawMessage, error) {
		switch method {
		case "host.http.do_stream":
			return json.Marshal(hostHTTPStreamOpen{StatusCode: http.StatusOK, StreamID: "upstream-gate"})
		case "host.http.stream_read":
			host.mu.Lock()
			defer host.mu.Unlock()
			if host.next >= len(host.frames) {
				return json.Marshal(hostHTTPStreamChunk{Done: true})
			}
			frame := host.frames[host.next]
			host.next++
			return json.Marshal(frame)
		case "host.http.stream_close":
			return json.RawMessage(`{}`), nil
		case "host.stream.emit":
			req := request.(map[string]any)
			host.mu.Lock()
			defer host.mu.Unlock()
			if payload, ok := req["payload"].([]byte); ok && len(payload) > 0 {
				host.emitted = append(host.emitted, payload)
			}
			if text, ok := req["error"].(string); ok && text != "" {
				host.errs = append(host.errs, text)
			}
			return json.RawMessage(`{}`), nil
		case "host.stream.close":
			host.mu.Lock()
			defer host.mu.Unlock()
			host.closes++
			return json.RawMessage(`{}`), nil
		default:
			return nil, fmt.Errorf("unexpected host callback %s", method)
		}
	})
	req := executorRequest{}
	req.Model = "fixture-model"
	req.Format = format
	req.Payload = []byte(`{"messages":[{"role":"user","content":"hello"}]}`)
	req.StorageJSON = mustMarshal(t, cred)
	req.StreamID = "client-gate"
	raw, errStream := executorExecuteStream(mustMarshal(t, req))
	if errStream != nil {
		t.Fatal(errStream)
	}
	result := envelope{}
	if errUnmarshal := json.Unmarshal(raw, &result); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	return result, host
}

func TestPreAnswerMetadataDoesNotHideQuota(t *testing.T) {
	metadata := []string{
		`data: {"id":"1","choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}` + "\n\n",
		`data: {"id":"1","choices":[{"index":0,"delta":{},"finish_reason":null}]}` + "\n\n",
		`data: {"id":"1","choices":[],"usage":{"total_tokens":1}}` + "\n\n",
		"data: [DONE]\n\n",
		":heartbeat\n\n",
	}
	for _, format := range []string{protocolOpenAI, protocolClaude} {
		for index, prefix := range metadata {
			for _, split := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%d/split=%t", format, index, split), func(t *testing.T) {
					frames := []hostHTTPStreamChunk{{Payload: []byte(prefix + gateQuotaFrame), Done: true}}
					if split {
						frames = []hostHTTPStreamChunk{{Payload: []byte(prefix)}, {Payload: []byte(gateQuotaFrame), Done: true}}
					}
					result, host := runGateStreamAs(t, frames, "agent", format)
					if result.OK || result.Error == nil || result.Error.HTTPStatus != http.StatusForbidden {
						t.Fatalf("metadata hid quota error: %+v", result)
					}
					host.mu.Lock()
					defer host.mu.Unlock()
					if len(host.emitted) > 0 || len(host.errs) > 0 || host.closes != 0 {
						t.Fatalf("sent pre-answer metadata to client: %+v", host)
					}
				})
			}
		}
	}
}

func TestMetadataBeforeAnswerPreservesFrameOrder(t *testing.T) {
	role := `data: {"id":"1","choices":[{"index":0,"delta":{"role":"assistant"}}]}` + "\n\n"
	result, host := runGateStream(t, []hostHTTPStreamChunk{{Payload: []byte(role)}, {Payload: []byte(answerFrame), Done: true}})
	if !result.OK {
		t.Fatalf("answer was rejected: %+v", result.Error)
	}
	waitFor(t, "ordered output", func() bool {
		host.mu.Lock()
		defer host.mu.Unlock()
		return len(host.emitted) >= 2 && host.closes == 1
	})
	host.mu.Lock()
	defer host.mu.Unlock()
	if !strings.Contains(string(host.emitted[0]), `"role":"assistant"`) || !strings.Contains(string(host.emitted[1]), "answer") {
		t.Fatalf("metadata and answer reordered: %q", host.emitted)
	}
}

func TestNativeMetadataBeforeQuotaNeverHandsOver(t *testing.T) {
	result, host := runGateStreamAs(t, []hostHTTPStreamChunk{
		{Payload: []byte(`data: {"type":"stage","stage":[{"name":"plan"}]}` + "\n\n")},
		{Payload: []byte(gateQuotaFrame), Done: true},
	}, "native", protocolClaude)
	if result.OK || result.Error == nil || result.Error.HTTPStatus != http.StatusForbidden {
		t.Fatalf("native stage hid quota: %+v", result)
	}
	host.mu.Lock()
	defer host.mu.Unlock()
	if len(host.emitted) != 0 || host.closes != 0 {
		t.Fatalf("native metadata reached client: %+v", host)
	}
}

// waitFor polls a condition the pump goroutine satisfies asynchronously.
func waitFor(t *testing.T, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if done() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestQuotaEnvelopeBeforeFirstChunkBecomesRotatableFailure(t *testing.T) {
	result, host := runGateStream(t, []hostHTTPStreamChunk{{Payload: []byte(gateQuotaFrame), Done: true}})
	if result.OK || result.Error == nil {
		t.Fatalf("exhausted quota was reported as a stream the client must consume: %+v", result)
	}
	if result.Error.HTTPStatus != http.StatusForbidden || result.Error.Code != "insufficient_quota" {
		t.Fatalf("host cannot rotate on this failure: %+v", result.Error)
	}
	if !strings.Contains(result.Error.Message, "insufficient quota") || !strings.Contains(result.Error.Message, "InferHub.4291.200") {
		t.Fatalf("upstream reason was lost: %q", result.Error.Message)
	}
	host.mu.Lock()
	defer host.mu.Unlock()
	if len(host.emitted) != 0 {
		t.Fatalf("a stream the client never got was already fed: %q", host.emitted)
	}
	if host.closes != 0 {
		t.Fatalf("the host stream was closed although it was never handed over: %d", host.closes)
	}
}

func TestAnsweredStreamKeepsStreamingAndReportsLaterFaultInBand(t *testing.T) {
	result, host := runGateStream(t, []hostHTTPStreamChunk{
		{Payload: []byte(answerFrame)},
		{Payload: []byte(gateQuotaFrame), Done: true},
	})
	if !result.OK {
		t.Fatalf("a stream that already answered must not be turned into a failed request: %+v", result.Error)
	}
	waitFor(t, "the in-band fault", func() bool {
		host.mu.Lock()
		defer host.mu.Unlock()
		return len(host.errs) > 0 && host.closes == 1
	})
	host.mu.Lock()
	defer host.mu.Unlock()
	if len(host.emitted) == 0 || !strings.Contains(string(host.emitted[0]), "answer") {
		t.Fatalf("the partial answer was discarded: %q", host.emitted)
	}
}

func TestAnswerAndFaultInOneReadKeepsThePartialAnswer(t *testing.T) {
	role := `data: {"id":"1","choices":[{"index":0,"delta":{"role":"assistant"}}]}` + "\n\n"
	result, host := runGateStream(t, []hostHTTPStreamChunk{{Payload: []byte(role + answerFrame + gateQuotaFrame), Done: true}})
	if !result.OK {
		t.Fatalf("quota after real answer discarded the partial reply: %+v", result.Error)
	}
	waitFor(t, "partial answer and stream error", func() bool {
		host.mu.Lock()
		defer host.mu.Unlock()
		return len(host.emitted) >= 2 && len(host.errs) == 1 && host.closes == 1
	})
	host.mu.Lock()
	defer host.mu.Unlock()
	if !strings.Contains(string(host.emitted[0]), `"role":"assistant"`) || !strings.Contains(string(host.emitted[1]), "answer") {
		t.Fatalf("same-read answer was reordered: %q", host.emitted)
	}
}

func TestFrameAnswerDetectionRejectsMetadataAndAllowsOutput(t *testing.T) {
	for _, payload := range []string{
		`{"choices":[{"delta":{"role":"assistant"}}]}`,
		`{"choices":[{"delta":{"content":""}}]}`,
		`{"choices":[],"usage":{"total_tokens":2}}`,
		`[DONE]`,
	} {
		if streamFrameHasAnswer([]byte(payload)) {
			t.Fatalf("metadata counted as answer: %s", payload)
		}
	}
	for _, payload := range []string{
		`{"choices":[{"delta":{"content":"hello"}}]}`,
		`{"choices":[{"delta":{"reasoning_content":"thinking"}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"x"}]}}]}`,
	} {
		if !streamFrameHasAnswer([]byte(payload)) {
			t.Fatalf("actual output was not counted: %s", payload)
		}
	}
}

func TestSilentUpstreamIsReportedAsFaultBeforeHandoff(t *testing.T) {
	result, _ := runGateStream(t, []hostHTTPStreamChunk{
		{Error: "connection reset by peer"},
	})
	if result.OK || result.Error == nil {
		t.Fatalf("a stream that died before answering was reported as success: %+v", result)
	}
	if result.Error.HTTPStatus != http.StatusBadGateway {
		t.Fatalf("read failure lost its status: %+v", result.Error)
	}
}

func TestCleanStreamWithoutAnswerFailsWithoutWaiting(t *testing.T) {
	started := time.Now()
	result, host := runGateStream(t, nil)
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("the gate waited %s for a head that never arrived", elapsed)
	}
	if result.OK || result.Error == nil || result.Error.HTTPStatus != http.StatusBadGateway {
		t.Fatalf("an empty stream became a successful completion: %+v", result)
	}
	waitFor(t, "cleanup", func() bool {
		host.mu.Lock()
		defer host.mu.Unlock()
		return host.closes == 0
	})
}

func TestGateHandshakeNeverStrandsThePump(t *testing.T) {
	gate := newStreamGate()
	if fault := gate.await(20 * time.Millisecond); fault == nil || fault.Status != http.StatusGatewayTimeout {
		t.Fatalf("an unanswered stream must fail, got %v", fault)
	}
	// The pump only notices afterwards: its report must not block, and the
	// timed-out executor must tell it not to emit to an unseen stream.
	returned := make(chan struct{})
	go func() {
		gate.reportHead(&upstreamStreamFault{Code: "upstream_error", Status: http.StatusBadGateway, Message: "late"})
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("reportHead blocked after the executor stopped listening")
	}
	select {
	case proceed := <-gate.verdict:
		if proceed {
			t.Fatal("a timed-out gate told the pump to emit")
		}
	case <-time.After(time.Second):
		t.Fatal("the pump is blocked on a verdict that never arrives")
	}
}

func TestGateNeverAcceptsAnswerAfterDeadline(t *testing.T) {
	for i := 0; i < 100; i++ {
		gate := newStreamGate()
		gate.reportHead(nil)
		if fault := gate.await(0); fault == nil || fault.Status != http.StatusGatewayTimeout {
			t.Fatalf("accepted a stream after its deadline: %+v", fault)
		}
		if <-gate.verdict {
			t.Fatal("timed-out stream was allowed to emit")
		}
	}
	gate := newStreamGate()
	gate.reportHead(nil)
	if fault := gate.await(time.Second); fault != nil || !(<-gate.verdict) {
		t.Fatalf("rejected an answer before the deadline: %+v", fault)
	}
}
