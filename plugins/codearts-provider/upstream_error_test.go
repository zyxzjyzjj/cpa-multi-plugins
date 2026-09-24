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

func executeErrorFixture(t *testing.T, mode string, cred credential) envelope {
	t.Helper()
	req := executorRequest{}
	req.Model = "fixture-model"
	req.Payload = []byte(`{"messages":[{"role":"user","content":"hello"}]}`)
	req.StorageJSON = mustMarshal(t, cred)
	if mode == "async-stream" {
		req.StreamID = "client-fixture"
	}
	var raw []byte
	var err error
	if mode == "completion" {
		raw, err = executorExecute(mustMarshal(t, req))
	} else {
		raw, err = executorExecuteStream(mustMarshal(t, req))
	}
	if err != nil {
		t.Fatal(err)
	}
	var result envelope
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	if result.OK || result.Error == nil {
		t.Fatalf("expected an upstream error envelope: %s", raw)
	}
	return result
}

func TestUpstreamHTTPErrorBodyAndStatusReachAllExecutorModes(t *testing.T) {
	for _, mode := range []string{"completion", "inline-stream", "async-stream"} {
		t.Run(mode, func(t *testing.T) {
			cfg := defaultConfig()
			cfg.DiscoverModels = false
			cfg.ChatSessionHeartbeat = false
			useModelTestConfig(t, cfg)
			cred := credential{
				AccessKeyID: "fixture-private-ak", SecretAccessKey: "fixture-private-sk",
				SecurityToken: "fixture-sts/with+characters&quote\"", RefreshToken: "fixture-private-refresh",
				OAuthContext: &oauthLoginContext{
					PKCEPair:    oauthPKCEPair{CodeVerifier: "fixture-pkce-verifier", CodeChallenge: "fixture-pkce-challenge"},
					DPoPKeyPair: oauthDPoPKeyPair{PrivateKeyJWK: oauthJWK{D: "fixture-dpop-private", X: "fixture-dpop-x", Y: "fixture-dpop-y"}},
				},
			}
			body := mustMarshal(t, map[string]any{
				"error_code": "GATE.042", "error_msg": "Selected model is unavailable", "echo": cred,
				"form_echo": url.QueryEscape(cred.SecurityToken),
			})
			var opens, reads, closes atomic.Int32
			testHost(t, func(method string, request any) (json.RawMessage, error) {
				switch method {
				case "host.http.do_stream":
					opens.Add(1)
					return json.Marshal(hostHTTPStreamOpen{StatusCode: http.StatusBadRequest, StreamID: "upstream-error"})
				case "host.http.stream_read":
					if reads.Add(1) == 1 {
						return json.Marshal(hostHTTPStreamChunk{Payload: body[:len(body)/2]})
					}
					return json.Marshal(hostHTTPStreamChunk{Payload: body[len(body)/2:], Done: true})
				case "host.http.stream_close":
					closes.Add(1)
					return json.RawMessage(`{}`), nil
				default:
					t.Errorf("failed upstream must not retry or emit client chunks: %s", method)
					return nil, fmt.Errorf("unexpected callback")
				}
			})
			result := executeErrorFixture(t, mode, cred)
			if result.Error.HTTPStatus != http.StatusBadRequest || !strings.Contains(result.Error.Message, "GATE.042") || !strings.Contains(result.Error.Message, "Selected model is unavailable") {
				t.Fatalf("upstream status or reason was lost: %+v", result.Error)
			}
			for _, secret := range []string{cred.AccessKeyID, cred.SecretAccessKey, cred.SecurityToken, cred.RefreshToken,
				cred.OAuthContext.PKCEPair.CodeVerifier, cred.OAuthContext.PKCEPair.CodeChallenge,
				cred.OAuthContext.DPoPKeyPair.PrivateKeyJWK.D, cred.OAuthContext.DPoPKeyPair.PrivateKeyJWK.X, cred.OAuthContext.DPoPKeyPair.PrivateKeyJWK.Y} {
				encoded, _ := json.Marshal(secret)
				for _, variant := range []string{secret, url.QueryEscape(secret), string(encoded[1 : len(encoded)-1])} {
					if strings.Contains(result.Error.Message, variant) {
						t.Fatal("upstream diagnostic echoed credential material")
					}
				}
			}
			if opens.Load() != 1 || reads.Load() != 2 || closes.Load() != 1 {
				t.Fatalf("error response was retried, skipped or not closed once: opens=%d reads=%d closes=%d", opens.Load(), reads.Load(), closes.Load())
			}
		})
	}
}

func TestUpstreamErrorReadIsBoundedAndKeepsStatus(t *testing.T) {
	for _, scenario := range []string{"oversized", "endless-empty", "partial-read-error"} {
		t.Run(scenario, func(t *testing.T) {
			var opens, reads, closes atomic.Int32
			testHost(t, func(method string, request any) (json.RawMessage, error) {
				switch method {
				case "host.http.do_stream":
					opens.Add(1)
					return json.Marshal(hostHTTPStreamOpen{StatusCode: http.StatusTooManyRequests, StreamID: "limited-error"})
				case "host.http.stream_read":
					reads.Add(1)
					switch scenario {
					case "oversized":
						return json.Marshal(hostHTTPStreamChunk{Payload: []byte(strings.Repeat("x", upstreamErrorBodyLimit+2048))})
					case "endless-empty":
						return json.Marshal(hostHTTPStreamChunk{})
					default:
						return json.Marshal(hostHTTPStreamChunk{Payload: []byte("quota exhausted"), Error: "transport detail including private data", Done: true})
					}
				case "host.http.stream_close":
					closes.Add(1)
					return json.RawMessage(`{}`), nil
				default:
					t.Errorf("unexpected callback %s", method)
					return nil, fmt.Errorf("unexpected callback")
				}
			})
			response, err := readUpstreamResponse(defaultConfig(), "", "https://fixture.test/chat", nil, nil)
			if err != nil || response.StatusCode != http.StatusTooManyRequests || response.ErrorReadNote == "" || len(response.Body) > upstreamErrorBodyLimit {
				t.Fatalf("bounded error reader lost status/body limit: response=%+v err=%v", response, err)
			}
			if opens.Load() != 1 || closes.Load() != 1 || reads.Load() > upstreamErrorChunkLimit {
				t.Fatalf("unbounded error reads or repeated request: opens=%d reads=%d closes=%d", opens.Load(), reads.Load(), closes.Load())
			}
			if scenario == "oversized" && (len(response.Body) != upstreamErrorBodyLimit || reads.Load() != 1) {
				t.Fatal("oversized error was not stopped at the byte limit")
			}
			if scenario == "partial-read-error" && string(response.Body) != "quota exhausted" {
				t.Fatal("read error discarded the useful partial upstream response")
			}
		})
	}
}

func TestUpstreamErrorTimeoutClosesAndPreservesHTTPStatus(t *testing.T) {
	for _, mode := range []string{"completion", "async-stream"} {
		t.Run(mode, func(t *testing.T) {
			cfg := defaultConfig()
			cfg.DiscoverModels = false
			cfg.ChatSessionHeartbeat = false
			cfg.RequestTimeoutSeconds = 1
			useModelTestConfig(t, cfg)
			closed := make(chan struct{})
			readerExited := make(chan struct{})
			var reads, closes atomic.Int32
			testHost(t, func(method string, request any) (json.RawMessage, error) {
				switch method {
				case "host.http.do_stream":
					return json.Marshal(hostHTTPStreamOpen{StatusCode: http.StatusForbidden, StreamID: "slow-error"})
				case "host.http.stream_read":
					if reads.Add(1) == 1 {
						return json.Marshal(hostHTTPStreamChunk{Payload: []byte("subscription expired")})
					}
					defer close(readerExited)
					<-closed
					return nil, fmt.Errorf("read canceled by stream close")
				case "host.http.stream_close":
					if closes.Add(1) == 1 {
						close(closed)
					}
					return json.RawMessage(`{}`), nil
				default:
					t.Errorf("unexpected callback %s", method)
					return nil, fmt.Errorf("unexpected callback")
				}
			})
			start := time.Now()
			result := executeErrorFixture(t, mode, credential{AccessKeyID: "fixture-ak", SecretAccessKey: "fixture-sk"})
			if time.Since(start) > 3*time.Second {
				t.Fatal("error body read ignored the configured short timeout")
			}
			if result.Error.HTTPStatus != http.StatusForbidden || !strings.Contains(result.Error.Message, "subscription expired") || !strings.Contains(result.Error.Message, "read timed out") || closes.Load() != 1 {
				t.Fatalf("timeout masked upstream status, discarded partial reason or failed to close: %+v closes=%d", result.Error, closes.Load())
			}
			select {
			case <-readerExited:
			case <-time.After(time.Second):
				t.Fatal("stream closure did not release the pending host read")
			}
		})
	}
}

func TestTruncatedUpstreamErrorDoesNotExposeCredentialPrefix(t *testing.T) {
	cred := &credential{SecurityToken: "fixture-sensitive-security-token"}
	prefix := cred.SecurityToken[:16]
	response := &hostHTTPResponse{StatusCode: http.StatusBadRequest, Body: []byte("upstream echo: " + prefix), ErrorReadNote: "error response truncated at 8 KiB"}
	raw, err := upstreamHTTPErrorEnvelope(response, cred)
	if err != nil {
		t.Fatal(err)
	}
	var result envelope
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	if result.Error == nil || strings.Contains(result.Error.Message, prefix) || !strings.Contains(result.Error.Message, "[REDACTED]") {
		t.Fatal("truncation exposed a partial credential")
	}
}
