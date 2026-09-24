package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestQuotaFaultAcrossModesAndEOF(t *testing.T) {
	for _, mode := range []string{"agent", "native"} {
		for _, suffix := range []string{"", "\n", "\n\ndata: [DONE]\n\n"} {
			t.Run(fmt.Sprintf("%s/suffix=%q", mode, suffix), func(t *testing.T) {
				cfg := defaultConfig()
				cfg.APIMode = mode
				body := []byte("data: " + quotaFaultFrame + suffix)
				if completion, err := aggregateUpstream(cfg, "probe", body); err == nil {
					t.Fatalf("quota error became a success: %s", completion)
				}
				for _, protocol := range []string{protocolOpenAI, protocolClaude} {
					raw, err := bufferedStreamResponse(cfg, "probe", protocol, body)
					if err != nil {
						t.Fatal(err)
					}
					var response envelope
					if err := json.Unmarshal(raw, &response); err != nil {
						t.Fatal(err)
					}
					if response.OK || response.Error == nil || response.Error.HTTPStatus != http.StatusForbidden {
						t.Fatalf("%s lost quota status: %s", protocol, raw)
					}
				}
			})
		}
	}
}

func TestEmptyContainersDoNotHideStreamFault(t *testing.T) {
	for _, field := range []string{
		`"choices":[]`, `"choices":null`, `"delta":null`, `"delta":{}`,
		`"choices":[{}]`, `"choices":[{"delta":{"role":"assistant"}}]`,
		`"choices":[{"delta":{"content":""}}]`, `"text":"[DONE]"`,
	} {
		payload := []byte(`{"error_code":"InferHub.4291.200","error_msg":"insufficient quota",` + field + `}`)
		if streamFrameFault(payload) == nil {
			t.Errorf("metadata hid error: %s", field)
		}
	}
	for _, field := range []string{
		`"delta":{"content":"partial"}`, `"delta":{"reasoning_content":"thinking"}`,
		`"choices":[{"delta":{"tool_calls":[{"function":{"arguments":"{"}}]}}]`,
		`"choices":[{"message":{"content":"answer"}}]`,
	} {
		if streamFrameFault([]byte(`{"error_code":"Gate.500",`+field+`}`)) != nil {
			t.Errorf("partial answer discarded: %s", field)
		}
	}
}

func TestAsyncFaultTerminatesOnceWithoutSuccess(t *testing.T) {
	for _, mode := range []string{"agent", "native"} {
		for _, protocol := range []string{protocolOpenAI, protocolClaude} {
			for _, tail := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/tail=%t", mode, protocol, tail), func(t *testing.T) {
					cfg := defaultConfig()
					cfg.APIMode = mode
					cred := &credential{SecurityToken: "fixture-sensitive-token"}
					frame := `data: {"error_code":"Gate.429","error_msg":"rejected fixture-sensitive-token"}`
					if !tail {
						frame += "\n\n"
					}
					reads, failures, closes, stops := 0, 0, 0, 0
					testHost(t, func(method string, request any) (json.RawMessage, error) {
						switch method {
						case "host.http.stream_read":
							reads++
							if reads > 1 {
								return nil, fmt.Errorf("read after terminal fault")
							}
							return json.Marshal(hostHTTPStreamChunk{Payload: []byte(frame), Done: tail})
						case "host.stream.emit":
							r := request.(map[string]any)
							if message, ok := r["error"].(string); ok {
								failures++
								if strings.Contains(message, cred.SecurityToken) || !strings.Contains(message, "[REDACTED]") {
									t.Errorf("stream error not redacted: %q", message)
								}
							} else {
								t.Errorf("fault emitted as successful content: %v", r)
							}
						case "host.stream.close":
							closes++
						default:
							return nil, fmt.Errorf("unexpected callback %s", method)
						}
						return json.RawMessage(`{}`), nil
					})
					req := executorRequest{}
					req.Model, req.Format = "probe", protocol
					session := &executorStreamSession{streamID: "client", stop: func() { stops++ }}
					runUpstreamStream(cfg, req, &hostHTTPStreamOpen{StreamID: "upstream"}, session, nil, cred)
					if reads != 1 || failures != 1 || closes != 1 || stops != 1 {
						t.Fatalf("reads=%d failures=%d closes=%d stops=%d", reads, failures, closes, stops)
					}
				})
			}
		}
	}
}

func TestNativeClaudeRetainsContentAndTrailingUsage(t *testing.T) {
	cfg := defaultConfig()
	cfg.APIMode = "native"
	renderer := newStreamRenderer(cfg, "probe", protocolClaude)
	var out [][]byte
	for _, chunk := range []string{
		`data: {"delta":{"content":"hello"}}` + "\n\n",
		`data: {"prompt_tokens":8,"completion_tokens":2,"total_tokens":10}`,
	} {
		// Exercise arbitrary byte fragmentation, not just whole SSE events.
		for _, b := range []byte(chunk) {
			out = append(out, renderer.feed([]byte{b})...)
		}
	}
	out = append(out, renderer.finish()...)
	var output strings.Builder
	for _, frame := range out {
		output.Write(frame)
	}
	text := output.String()
	if !strings.Contains(text, `"text":"hello"`) || !strings.Contains(text, `"output_tokens":2`) || strings.Count(text, "event: message_stop") != 1 {
		t.Fatalf("native -> Claude lost text/usage/termination: %s", text)
	}
	if len(renderer.finish()) != 0 {
		t.Fatal("finish emitted a duplicate terminal event")
	}
}

func TestBufferedFaultRedactsCredential(t *testing.T) {
	cred := &credential{SecretAccessKey: "fixture-sensitive-secret"}
	raw, err := bufferedStreamResponse(defaultConfig(), "probe", protocolOpenAI,
		[]byte(`data: {"error_code":"Gate.500","error_msg":"fixture-sensitive-secret"}`), cred)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), cred.SecretAccessKey) || !strings.Contains(string(raw), "[REDACTED]") {
		t.Fatalf("buffered fault leaked a credential: %s", raw)
	}
}

func TestQuotaMissingNullAndNegativePercentStayUnknown(t *testing.T) {
	for _, value := range []string{"", `,"value":null`, `,"value":-1`} {
		snapshot, err := parseQuotaSnapshot([]byte(`{"metrics":[{"name":"new-meter","show":true,"usage_token_num":30,"package_token_amount":100` + value + `}]}`))
		if err != nil || len(snapshot.Meters) != 1 {
			t.Fatalf("counters were lost: %+v, %v", snapshot, err)
		}
		if fraction, ok := snapshot.Meters[0].remainingFraction(); ok {
			t.Fatalf("unknown percent fabricated a ratio %v for %s", fraction, value)
		}
		if len(toQuotaFetchResponse(snapshot).Groups) != 0 {
			t.Fatal("unknown ratio advertised to CPA")
		}
	}
}

func TestQuotaRefreshDoesNotCallOptionalGateway(t *testing.T) {
	cfg := defaultConfig()
	useModelTestConfig(t, cfg)
	defer quotas.forget("package-only")
	calls := 0
	testHost(t, func(method string, request any) (json.RawMessage, error) {
		calls++
		r := request.(map[string]any)
		if method != "host.http.do" || !strings.HasSuffix(r["url"].(string), "/snap-manager/v1/statistics/plugin") {
			t.Fatalf("package refresh touched optional I/O: %s", method)
		}
		if r["host_callback_id"] != "quota-context" {
			t.Fatal("quota request lost cancellation context")
		}
		return json.Marshal(hostHTTPResponse{StatusCode: 200, Body: []byte(`{"metrics":[{"name":"quota","show":true,"value":20}]}`)})
	})
	snapshot, err := fetchQuotaSnapshot("package-only", &credential{AccessKeyID: "fixture-ak", SecretAccessKey: "fixture-sk"}, "quota-context")
	if err != nil || calls != 1 || len(snapshot.Meters) != 1 {
		t.Fatalf("package refresh failed: %+v, %v, calls=%d", snapshot, err, calls)
	}
	if cached, ok := quotas.get("package-only"); !ok || len(cached.Meters) != 1 {
		t.Fatal("successful package result was not cached")
	}
}

func TestBenefitCacheIndependentMergeAndExpiry(t *testing.T) {
	cache := &quotaCache{}
	cache.putBenefit("account", &benefitBalance{DailyTokenLimit: 100, DailyTokensUsed: 30}, "")
	cache.put(quotaSnapshot{AuthIndex: "account", Plan: "paid", FetchedAt: time.Now()})
	snapshot, _ := cache.get("account")
	if snapshot.Plan != "paid" || snapshot.Benefit == nil || snapshot.Benefit.DailyTokensUsed != 30 {
		t.Fatalf("package write erased benefit: %+v", snapshot)
	}
	snapshot.BenefitFetchedAt = time.Now().Add(-6 * time.Minute)
	cache.put(snapshot)
	for _, got := range []quotaSnapshot{cache.all()["account"], func() quotaSnapshot { s, _ := cache.get("account"); return s }()} {
		if got.Benefit != nil || got.BenefitError == "" || got.Plan != "paid" {
			t.Fatalf("stale benefit advertised as current: %+v", got)
		}
	}
	cache.putBenefit("account", nil, "upstream failed")
	if got, _ := cache.get("account"); got.Plan != "paid" || got.BenefitError != "upstream failed" {
		t.Fatalf("benefit error damaged package: %+v", got)
	}
}

func TestBenefitRequestCarriesCancellationAndRefusesBackground(t *testing.T) {
	cred := &credential{AccessKeyID: "fixture-ak", SecretAccessKey: "fixture-sk"}
	cfg := defaultConfig()
	calls := 0
	testHost(t, func(method string, request any) (json.RawMessage, error) {
		calls++
		r := request.(map[string]any)
		if method != "host.http.do" || r["host_callback_id"] != "benefit-context" {
			t.Fatal("optional I/O lost cancellation")
		}
		return json.Marshal(hostHTTPResponse{StatusCode: 200, Body: []byte(`{"error_code":"0000","result":{"daily_token_limit":100}}`)})
	})
	if _, err := fetchBenefitBalance(cfg, cred); err == nil || calls != 0 {
		t.Fatal("detached optional request was allowed")
	}
	if b, err := fetchBenefitBalance(cfg, cred, "benefit-context"); err != nil || b.DailyTokenLimit != 100 || calls != 1 {
		t.Fatalf("scoped request failed: %+v, %v", b, err)
	}
}
