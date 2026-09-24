package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mmqz/cpa-multi-plugins/plugins/trae/pool"
	"github.com/mmqz/cpa-multi-plugins/plugins/trae/upstream"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestExecutorStreamAndNonStreamUseResolvedModel(t *testing.T) {
	var bodies [][]byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodies = append(bodies, body)
		var obj map[string]any
		_ = json.Unmarshal(body, &obj)
		w.Header().Set("Content-Type", "text/event-stream")
		if obj["config_name"] != "kimi-k3" || obj["stream"] != true {
			_, _ = io.WriteString(w, "event: error\ndata: {\"code\":4001,\"message\":\"param is invalid\"}\n\n")
			return
		}
		_, _ = io.WriteString(w, soloAnswerSSE)
	}))
	defer server.Close()
	oldClient := upstreamClient
	oldPool := accountPool
	accountPool = pool.New("")
	upstreamClient = &upstream.Client{HTTP: server.Client(), AgentHost: server.URL}
	t.Cleanup(func() { upstreamClient = oldClient; accountPool = oldPool })
	storage, _ := json.Marshal(map[string]any{
		"account": map[string]any{"uid": "issue13-fixture"},
		"auth":    map[string]any{"accessToken": "fixture", "variant": "solo", "expiresAt": time.Now().Add(72 * time.Hour).Unix()},
	})
	for _, stream := range []bool{false, true} {
		payload, _ := json.Marshal(map[string]any{"model": "client-alias", "stream": stream, "max_tokens": 16,
			"messages": []any{map[string]any{"role": "user", "content": "收到"}}})
		req := pluginapi.ExecutorRequest{Model: "kimi-k3-solo", Payload: payload, StorageJSON: storage, Stream: stream}
		raw, _ := json.Marshal(req)
		var out []byte
		var err error
		if stream {
			out, err = handleExecStream(raw)
		} else {
			out, err = handleExecExecute(raw)
		}
		if err != nil {
			t.Fatalf("stream=%v: %v", stream, err)
		}
		var env envelope
		if json.Unmarshal(out, &env) != nil || !env.OK {
			t.Fatalf("stream=%v: invalid response %s", stream, out)
		}
	}
	if len(bodies) != 2 || !bytes.Equal(bodies[0], bodies[1]) {
		t.Fatalf("stream and non-stream sent different upstream payloads: %q", bodies)
	}
}

func TestNormalizeExecutorPayloadMissingModelAndInvalidBody(t *testing.T) {
	req := pluginapi.ExecutorRequest{Model: "glm-5.3-solo", Payload: []byte(`{"messages":[]}`)}
	if err := normalizeExecutorPayload(&req); err != nil || !bytes.Contains(req.Payload, []byte(`"model":"glm-5.3-solo"`)) {
		t.Fatalf("resolved model was lost: %s, %v", req.Payload, err)
	}
	for _, payload := range []string{"null", "[]", "broken"} {
		req.Payload = []byte(payload)
		err := normalizeExecutorPayload(&req)
		if err == nil {
			t.Fatalf("accepted invalid request %s", payload)
		}
		var env envelope
		_ = json.Unmarshal(errorEnvelopeFor(err), &env)
		if env.Error.HTTPStatus != 400 {
			t.Fatalf("invalid request must be request-scoped: %+v", env.Error)
		}
	}
}

func TestSoloRequestFaultKeepsStatusAcrossRPC(t *testing.T) {
	for _, tc := range []struct {
		message string
		status  int
	}{
		{"param is invalid", 422}, {"prompt is too long", 413},
	} {
		fault := &upstream.SOLOStreamError{Code: 4001, Msg: tc.message}
		var env envelope
		_ = json.Unmarshal(errorEnvelopeFor(upstreamStatusError(soloFaultStatus(fault), soloStreamErrorCopy(fault))), &env)
		if env.Error == nil || env.Error.HTTPStatus != tc.status {
			t.Fatalf("%q lost request-scoped status: %+v", tc.message, env.Error)
		}
	}
}
