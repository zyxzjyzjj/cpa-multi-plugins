package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func chatTestHeader(headers map[string][]string, name string) string {
	for key, values := range headers {
		if strings.EqualFold(key, name) && len(values) > 0 {
			return values[0]
		}
	}
	return ""
}

func chatHeartbeatFixtureRequest(t *testing.T, request any) (string, string) {
	t.Helper()
	req := request.(map[string]any)
	u, err := url.Parse(req["url"].(string))
	if err != nil {
		t.Error(err)
		return "", ""
	}
	if req["method"] != http.MethodPut || u.Path != "/snap-manager/v1/chat-session/heartbeat" || string(req["body"].([]byte)) != "{}" {
		t.Error("heartbeat did not use the official PUT endpoint and empty JSON object")
	}
	headers := req["headers"].(map[string][]string)
	if !strings.Contains(chatTestHeader(headers, "Authorization"), "user-session-id") {
		t.Error("heartbeat did not sign its user-session-id header")
	}
	sessionID := chatTestHeader(headers, "user-session-id")
	if sessionID == "" {
		t.Error("heartbeat has no user-session-id")
	}
	return u.Query().Get("status"), sessionID
}

func TestChatSessionRefreshesBusyAndStopsIdleOnce(t *testing.T) {
	const interval = 20 * time.Millisecond
	cfg := defaultConfig()
	var busy, idle atomic.Int32
	type event struct{ status, id string }
	events := make(chan event, 32)
	testHost(t, func(method string, request any) (json.RawMessage, error) {
		if method != "host.http.do" {
			return nil, fmt.Errorf("unexpected callback %s", method)
		}
		status, id := chatHeartbeatFixtureRequest(t, request)
		if status == "busy" {
			busy.Add(1)
		} else if status == "idle" {
			idle.Add(1)
		} else {
			t.Errorf("unknown heartbeat status %q", status)
		}
		select {
		case events <- event{status, id}:
		default:
		}
		return json.Marshal(modelJSON([]byte(`{"status":"ok"}`)))
	})
	t.Cleanup(stopAllChatSessions)
	session, response, err := beginChatSessionWithInterval(cfg, executorRequest{HostCallbackID: "session-callback", ChatSessionID: "foreign-session"}, &credential{AccessKeyID: "fixture-ak", SecretAccessKey: "fixture-sk"}, interval)
	if err != nil || response != nil || session == nil || session.ID() == "" || session.ID() == "foreign-session" {
		t.Fatalf("could not open chat session: response=%+v err=%v", response, err)
	}
	for sequence := 0; sequence < 2; sequence++ {
		select {
		case event := <-events:
			if event.status != "busy" || event.id != session.ID() {
				t.Fatalf("heartbeat lost the shared session ID: %+v", event)
			}
		case <-time.After(time.Second):
			t.Fatal("initial or periodic busy heartbeat was not sent")
		}
	}
	session.Stop()
	stoppedBusy := busy.Load()
	session.Stop()
	stopAllChatSessions()
	if idle.Load() != 1 {
		t.Fatalf("session stopped with %d idle heartbeats, want one", idle.Load())
	}
	time.Sleep(2 * interval)
	if busy.Load() != stoppedBusy {
		t.Fatal("busy heartbeat continued after idle")
	}
	for len(events) > 0 {
		event := <-events
		if event.id != session.ID() {
			t.Fatal("idle heartbeat changed the session ID")
		}
	}
}

func TestChatSessionDisabledOrNativeSkipsHeartbeats(t *testing.T) {
	for _, mode := range []string{"disabled", "native", "no-credential"} {
		t.Run(mode, func(t *testing.T) {
			cfg := defaultConfig()
			cred := &credential{AccessKeyID: "fixture-ak", SecretAccessKey: "fixture-sk"}
			switch mode {
			case "disabled":
				cfg.ChatSessionHeartbeat = false
			case "native":
				cfg.APIMode = "native"
			default:
				cred = nil
			}
			testHost(t, func(method string, request any) (json.RawMessage, error) {
				t.Errorf("heartbeat should be skipped for %s", mode)
				return nil, fmt.Errorf("unexpected heartbeat")
			})
			session, response, err := beginChatSession(cfg, executorRequest{}, cred)
			if session != nil || response != nil || err != nil {
				t.Fatalf("disabled heartbeat unexpectedly changed the request: session=%v response=%+v err=%v", session, response, err)
			}
		})
	}
}

func TestChatSessionTransientHeartbeatFailureDoesNotReleaseActiveChat(t *testing.T) {
	for _, failure := range []string{"transport", "http"} {
		t.Run(failure, func(t *testing.T) {
			var busy, idle atomic.Int32
			recovered := make(chan struct{})
			testHost(t, func(method string, request any) (json.RawMessage, error) {
				if method != "host.http.do" {
					return nil, fmt.Errorf("unexpected callback %s", method)
				}
				status, _ := chatHeartbeatFixtureRequest(t, request)
				if status == "idle" {
					idle.Add(1)
					return json.Marshal(modelJSON([]byte(`{"status":"ok"}`)))
				}
				switch busy.Add(1) {
				case 2:
					if failure == "transport" {
						return nil, fmt.Errorf("temporary network failure")
					}
					return json.Marshal(hostHTTPResponse{StatusCode: http.StatusServiceUnavailable})
				case 3:
					close(recovered)
				}
				return json.Marshal(modelJSON([]byte(`{"status":"ok"}`)))
			})
			t.Cleanup(stopAllChatSessions)
			session, response, err := beginChatSessionWithInterval(defaultConfig(), executorRequest{}, &credential{AccessKeyID: "fixture-ak", SecretAccessKey: "fixture-sk"}, 10*time.Millisecond)
			if err != nil || response != nil || session == nil {
				t.Fatalf("could not start session: response=%+v err=%v", response, err)
			}
			select {
			case <-recovered:
			case <-time.After(time.Second):
				t.Fatal("a transient heartbeat failure stopped periodic busy renewal")
			}
			if idle.Load() != 0 {
				t.Fatal("heartbeat failure released a chat that is still generating")
			}
			session.Stop()
			if idle.Load() != 1 {
				t.Fatal("completed chat did not release its session exactly once")
			}
		})
	}
}

func TestChatSessionFailedBusyStillReleasesOwnedSession(t *testing.T) {
	for _, mode := range []string{"status", "transport", "unacknowledged"} {
		t.Run(mode, func(t *testing.T) {
			var busy, idle atomic.Int32
			var ownedID string
			testHost(t, func(method string, request any) (json.RawMessage, error) {
				if method != "host.http.do" {
					return nil, fmt.Errorf("unexpected callback %s", method)
				}
				status, id := chatHeartbeatFixtureRequest(t, request)
				if status == "idle" {
					idle.Add(1)
					if id != ownedID || request.(map[string]any)["host_callback_id"] != "" {
						t.Error("failed busy cleanup must release only its own session using a detached callback")
					}
					return json.Marshal(modelJSON([]byte(`{"status":"ok"}`)))
				}
				busy.Add(1)
				ownedID = id
				switch mode {
				case "status":
					return json.Marshal(hostHTTPResponse{StatusCode: http.StatusBadRequest, Body: []byte(`{"error_code":"TM.00001041"}`)})
				case "transport":
					return nil, fmt.Errorf("fixture transport failed after accepting busy")
				default:
					return json.Marshal(modelJSON([]byte(`{"status":"error"}`)))
				}
			})
			t.Cleanup(stopAllChatSessions)
			session, response, err := beginChatSession(defaultConfig(), executorRequest{HostCallbackID: "request-callback"}, &credential{AccessKeyID: "fixture-ak", SecretAccessKey: "fixture-sk"})
			if session != nil || busy.Load() != 1 || idle.Load() != 1 {
				t.Fatalf("failed busy leaked its session: busy=%d idle=%d session=%v", busy.Load(), idle.Load(), session)
			}
			if mode == "status" {
				if err != nil || response == nil || response.StatusCode != http.StatusBadRequest || !strings.Contains(string(response.Body), "TM.00001041") {
					t.Fatalf("busy rejection lost its upstream diagnostic: response=%+v err=%v", response, err)
				}
			} else if err == nil {
				t.Fatal("failed busy did not report an error")
			}
			stopAllChatSessions()
			if idle.Load() != 1 {
				t.Fatal("failed acquisition was retained in the active session registry")
			}
		})
	}
}

func TestExecutorReleasesChatSessionOnRejectedChat(t *testing.T) {
	for _, mode := range []string{"completion", "inline-stream", "async-stream"} {
		t.Run(mode, func(t *testing.T) {
			cfg := defaultConfig()
			cfg.DiscoverModels = false
			cfg.ExtraHeaders = map[string]string{"user-session-id": "foreign-session"}
			useModelTestConfig(t, cfg)
			var busy, idle, closed atomic.Int32
			var sessionID string
			testHost(t, func(method string, request any) (json.RawMessage, error) {
				switch method {
				case "host.http.do":
					status, id := chatHeartbeatFixtureRequest(t, request)
					if status == "busy" {
						sessionID = id
						busy.Add(1)
					} else if status == "idle" {
						if id != sessionID {
							t.Error("idle did not release the chat's session")
						}
						idle.Add(1)
					}
					return json.Marshal(modelJSON([]byte(`{"status":"ok"}`)))
				case "host.http.do_stream":
					headers := request.(map[string]any)["headers"].(map[string][]string)
					if busy.Load() != 1 || chatTestHeader(headers, "user-session-id") != sessionID {
						t.Error("chat opened without its successfully established busy session")
					}
					return json.Marshal(hostHTTPStreamOpen{StatusCode: http.StatusBadRequest, StreamID: "rejected-chat"})
				case "host.http.stream_read":
					return json.Marshal(hostHTTPStreamChunk{Payload: []byte(`{"error_code":"FIXTURE.400"}`), Done: true})
				case "host.http.stream_close":
					closed.Add(1)
					return json.RawMessage(`{}`), nil
				default:
					t.Errorf("unexpected callback %s", method)
					return nil, fmt.Errorf("unexpected callback")
				}
			})
			t.Cleanup(stopAllChatSessions)
			result := executeErrorFixture(t, mode, credential{AccessKeyID: "fixture-ak", SecretAccessKey: "fixture-sk"})
			if result.Error.HTTPStatus != http.StatusBadRequest || busy.Load() != 1 || idle.Load() != 1 || closed.Load() != 1 {
				t.Fatalf("rejected chat did not release both resources: status=%d busy=%d idle=%d closed=%d", result.Error.HTTPStatus, busy.Load(), idle.Load(), closed.Load())
			}
			if v := accountSessionConcurrency(cfg, &credential{AccessKeyID: "fixture-ak", SecretAccessKey: "fixture-sk"}, ""); v.Active != 0 {
				t.Fatal("rejected chat leaked its local concurrency permit")
			}
		})
	}
}

func TestExecutorTimeoutReleasesChatSession(t *testing.T) {
	cfg := defaultConfig()
	cfg.DiscoverModels = false
	cfg.RequestTimeoutSeconds = 1
	useModelTestConfig(t, cfg)
	var busy, idle, upstreamCloses atomic.Int32
	upstreamClosed := make(chan struct{})
	var clientCalls atomic.Int32
	testHost(t, func(method string, request any) (json.RawMessage, error) {
		switch method {
		case "host.http.do":
			status, _ := chatHeartbeatFixtureRequest(t, request)
			if status == "busy" {
				busy.Add(1)
			} else if status == "idle" {
				idle.Add(1)
			}
			return json.Marshal(modelJSON([]byte(`{"status":"ok"}`)))
		case "host.http.do_stream":
			return json.Marshal(hostHTTPStreamOpen{StatusCode: http.StatusOK, StreamID: "timed-out-chat"})
		case "host.http.stream_read":
			<-upstreamClosed
			return nil, fmt.Errorf("upstream closed")
		case "host.http.stream_close":
			if upstreamCloses.Add(1) == 1 {
				close(upstreamClosed)
			}
			return json.RawMessage(`{}`), nil
		case "host.stream.emit":
			clientCalls.Add(1)
			return json.RawMessage(`{}`), nil
		case "host.stream.close":
			clientCalls.Add(1)
			return json.RawMessage(`{}`), nil
		default:
			t.Errorf("unexpected callback %s", method)
			return nil, fmt.Errorf("unexpected callback")
		}
	})
	t.Cleanup(stopAllChatSessions)
	req := executorRequest{StreamID: "timeout-client"}
	req.Model = "fixture-model"
	req.Payload = []byte(`{"messages":[{"role":"user","content":"hello"}]}`)
	req.StorageJSON = []byte(`{"access_key_id":"fixture-ak","secret_access_key":"fixture-sk"}`)
	raw, err := executorExecuteStream(mustMarshal(t, req))
	if err != nil {
		t.Fatal(err)
	}
	var result envelope
	if json.Unmarshal(raw, &result) != nil || result.OK || result.Error == nil || result.Error.HTTPStatus != http.StatusGatewayTimeout {
		t.Fatalf("silent stream did not fail before handoff: %s", raw)
	}
	if clientCalls.Load() != 0 {
		t.Fatal("timeout emitted or closed a stream the host never received")
	}
	if busy.Load() != 1 || idle.Load() != 1 || upstreamCloses.Load() != 1 {
		t.Fatalf("timeout leaked a session: busy=%d idle=%d upstream closes=%d", busy.Load(), idle.Load(), upstreamCloses.Load())
	}
	if v := accountSessionConcurrency(cfg, &credential{AccessKeyID: "fixture-ak", SecretAccessKey: "fixture-sk"}, ""); v.Active != 0 {
		t.Fatal("timeout leaked its local concurrency permit")
	}
}
