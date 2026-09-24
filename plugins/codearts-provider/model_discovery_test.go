package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func modelFixture(t *testing.T, name string) []byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", "models", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func resetModelCache(t *testing.T) {
	t.Helper()
	clear := func() {
		benefitCatalogMemory.Lock()
		benefitCatalogMemory.entries = map[string]benefitCatalogState{}
		benefitCatalogMemory.retry = map[string]time.Time{}
		benefitCatalogMemory.Unlock()
		discoveredModels.Lock()
		discoveredModels.entries = map[string]modelCacheEntry{}
		discoveredModels.Unlock()
		modelCatalogFlights.Lock()
		modelCatalogFlights.entries = map[string]*modelCatalogFlight{}
		modelCatalogFlights.Unlock()
	}
	clear()
	t.Cleanup(clear)
}

func useModelTestConfig(t *testing.T, cfg *Config) {
	t.Helper()
	previous := currentConfig.Load()
	currentConfig.Store(cfg)
	t.Cleanup(func() {
		if previous == nil {
			previous = defaultConfig()
		}
		currentConfig.Store(previous)
	})
}

func modelTestHost(t *testing.T, callback func(*url.URL, http.Header) (hostHTTPResponse, error)) {
	t.Helper()
	testHost(t, func(method string, request any) (json.RawMessage, error) {
		if method != "host.http.do" {
			t.Fatalf("unexpected host callback: %s", method)
		}
		req := request.(map[string]any)
		if req["method"] != http.MethodGet {
			t.Fatalf("catalogue request method = %v", req["method"])
		}
		u, err := url.Parse(req["url"].(string))
		if err != nil {
			t.Fatal(err)
		}
		response, err := callback(u, http.Header(req["headers"].(map[string][]string)))
		if err != nil {
			return nil, err
		}
		return json.Marshal(response)
	})
}

func modelJSON(body []byte) hostHTTPResponse {
	return hostHTTPResponse{StatusCode: http.StatusOK, Body: body}
}

func TestCapturedAccountCatalogAndBenefitRouting(t *testing.T) {
	resetModelCache(t)
	cfg := defaultConfig()
	cfg.BaseURL = "https://catalog.test"
	cfg.BenefitGatewayURL = "https://gateway.test"
	useModelTestConfig(t, cfg)
	cred := &credential{AccessKeyID: "test-ak", SecretAccessKey: "test-sk", SecurityToken: "test-sts", DomainID: "test-domain"}
	var requests atomic.Int32
	modelTestHost(t, func(u *url.URL, headers http.Header) (hostHTTPResponse, error) {
		requests.Add(1)
		switch u.Path {
		case "/v1/agent-center/agents/useragents":
			q := u.Query()
			if q.Get("offset") != "0" || q.Get("limit") != "100" || q.Get("is_primary_agent") != "true" || q.Get("supported_client") != "VSCODE" || q.Get("min_compatible_plugin_version") != "26.9.101" {
				t.Fatalf("wrong useragents query: %s", u.RawQuery)
			}
			return modelJSON(modelFixture(t, "useragents")), nil
		case "/v1/agent-center/agents/detail":
			if u.Query().Get("agent_id") == "a8bcb36232554267a5142361cc25a393" {
				return modelJSON(modelFixture(t, "agent-detail")), nil
			}
			return modelJSON([]byte(`{"gpts":{"models":[]}}`)), nil
		case "/v1/benefit-gateway-config":
			if headers.Get("Agent-Type") != "PromptCenter" {
				t.Fatalf("benefit gate Agent-Type = %q", headers.Get("Agent-Type"))
			}
			return modelJSON([]byte(`{"enabled":true}`)), nil
		case "/api/v1/gateway/config":
			if u.Host != "gateway.test" {
				t.Fatalf("gateway catalogue used wrong host: %s", u.Host)
			}
			if !strings.Contains(headers.Get("Authorization"), "SignedHeaders=host;x-sdk-date;x-security-token,") || headers.Get("X-Security-Token") != cred.SecurityToken {
				t.Fatal("gateway config did not use the official narrow signature and account security token")
			}
			return modelJSON(modelFixture(t, "gateway-config")), nil
		case "/v1/model/builtin":
			if headers.Get("Agent-Type") != "PromptCenter" {
				t.Fatalf("builtin catalogue Agent-Type = %q", headers.Get("Agent-Type"))
			}
			return modelJSON(modelFixture(t, "builtin")), nil
		default:
			t.Fatalf("unexpected catalogue path %s", u.Path)
			return hostHTTPResponse{}, nil
		}
	})
	catalog := accountModelCatalog(cfg, cred, "callback")
	if len(catalog.Warnings) != 0 || len(catalog.Models) != 9 {
		t.Fatalf("expected nine discovered models without warnings: %+v", catalog)
	}
	want := []ModelConfig{
		{ID: "GLM-5.2", DisplayName: "GLM-5.2", ContextLength: 202752, MaxOutputTokens: 131072, Source: "agent"},
		{ID: "glm-5.2-sft-harmony", DisplayName: "GLM-5.2-ArkTS-SPARK", ContextLength: 202752, MaxOutputTokens: 131072, Source: "agent"},
		{ID: "openpangu-2.0-pro", DisplayName: "OpenPangu-2.0-Pro", ContextLength: 524288, MaxOutputTokens: 131072, Source: "agent"},
		{ID: "openpangu-2.0-flash", DisplayName: "OpenPangu-2.0-Flash", ContextLength: 524288, MaxOutputTokens: 131072, Source: "agent"},
		{ID: "deepseek-v4-flash-0731", DisplayName: "deepseek-v4-flash-0731", ContextLength: 1048576, MaxOutputTokens: 393216, Source: "benefit"},
		{ID: "deepseek-v4-pro-0813", DisplayName: "deepseek-v4-pro-0813", ContextLength: 1048576, MaxOutputTokens: 393216, Source: "benefit"},
		{ID: "glm-5.3-flash", DisplayName: "glm-5.3-flash", ContextLength: 1048576, MaxOutputTokens: 131072, Source: "benefit"},
		{ID: "Qwen3-VL-235B", DisplayName: "Qwen3-VL-235B", ContextLength: 131072, MaxOutputTokens: 8192, SupportsImages: true, Source: "builtin"},
		{ID: "kimi-k2.6-vl", DisplayName: "Kimi-K2.6-VL", ContextLength: 262144, MaxOutputTokens: 32768, SupportsImages: true, Source: "builtin"},
	}
	byID := make(map[string]ModelConfig, len(catalog.Models))
	for _, model := range catalog.Models {
		byID[model.ID] = model
	}
	for _, expected := range want {
		actual, ok := byID[expected.ID]
		if !ok || actual.DisplayName != expected.DisplayName || actual.ContextLength != expected.ContextLength || actual.MaxOutputTokens != expected.MaxOutputTokens || actual.Source != expected.Source {
			t.Fatalf("model metadata = %+v, want %+v", actual, expected)
		}
		req := executorRequest{}
		req.Model = expected.ID
		req.Payload = []byte(`{"messages":[{"role":"user","content":"hello"}]}`)
		body, endpoint, headers, err := buildUpstreamRequest(cfg, req, cred, true)
		if err != nil {
			t.Fatal(err)
		}
		var payload struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatal(err)
		}
		if payload.Model != expected.ID || endpoint != "https://catalog.test/api/v2/chat/completions" {
			t.Fatalf("model routed as %q to %q", payload.Model, endpoint)
		}
		if expected.Source == "benefit" && (headers["maas_type"] != "benefit" || headers["model-id"] != expected.ID || headers["model-name"] != expected.ID) {
			t.Fatalf("benefit model %s missing its routing headers", expected.ID)
		}
		if expected.Source == "agent" && headers["maas_type"] == "benefit" {
			t.Fatalf("agent model %s incorrectly routed as benefit", expected.ID)
		}
	}
	before := requests.Load()
	catalog.Models[0].ID = "caller-mutated-id"
	if again := accountModelCatalog(cfg, cred, "callback"); requests.Load() != before || again.Models[0].ID == "caller-mutated-id" {
		t.Fatal("catalogue cache is not reused or exposes mutable storage")
	}

	// Changing routing configuration intentionally invalidates the live-cache
	// key. Keep the account-confirmed benefit target as explicit LKG metadata so
	// alias routing remains network-free on the executor path.
	cfg.BenefitModels = []ModelConfig{{ID: "glm-5.3-flash", Name: "glm-5.3-flash", DisplayName: "glm-5.3-flash", Source: "benefit"}}
	cfg.ModelMap = map[string]string{"friendly-flash": "glm-5.3-flash"}
	req := executorRequest{}
	req.Model = "friendly-flash"
	req.Payload = []byte(`{"messages":[{"role":"user","content":"hello"}]}`)
	body, _, headers, err := buildUpstreamRequest(cfg, req, cred, false)
	if err != nil {
		t.Fatal(err)
	}
	var aliased struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &aliased); err != nil || aliased.Model != "glm-5.3-flash" || headers["maas_type"] != "benefit" {
		t.Fatalf("configured alias lost discovered benefit routing: model=%q headers=%v err=%v", aliased.Model, headers["maas_type"], err)
	}
	// A configured mapping may intentionally reuse another discovered model's
	// name. Routing must follow the target, not that name's original source.
	cfg.ModelMap = map[string]string{"GLM-5.2": "glm-5.3-flash"}
	req.Model = "GLM-5.2"
	body, _, headers, err = buildUpstreamRequest(cfg, req, cred, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(body, &aliased); err != nil || aliased.Model != "glm-5.3-flash" || headers["maas_type"] != "benefit" {
		t.Fatalf("a colliding alias used the source model's route: model=%q maas_type=%q err=%v", aliased.Model, headers["maas_type"], err)
	}
}

func TestAgentModelsRespectVisibilityAndAliases(t *testing.T) {
	models, err := parseAgentModels([]byte(`{"gpts":{"models":[
		{"model_alias":"visible-alias","model_id":"internal-id","model_name":"Visible Name","model_parameters":{"enabled":null,"display_enabled":true}},
		{"model_alias":"disabled","model_parameters":{"enabled":false,"display_enabled":true}},
		{"model_alias":"hidden","model_parameters":{"enabled":true,"display_enabled":false}},
		{"model_id":"id-only","model_parameters":{"display_enabled":true}},
		{"model_alias":"missing-display-flag","model_parameters":{"enabled":true}},
		{"model_alias":"null-display-flag","model_parameters":{"enabled":true,"display_enabled":null}},
		{"model_alias":"display-alias","model_id":"top-level-id","model_parameters":{"model_id":"effective-call-id","display_enabled":true}},
		{"model_alias":"","model_id":"","model_parameters":{"display_enabled":true}}
	]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 3 || models[0].ID != "visible-alias" || models[0].DisplayName != "Visible Name" || models[1].ID != "id-only" || models[2].ID != "effective-call-id" {
		t.Fatalf("wrong visible models/aliases: %+v", models)
	}
}

func TestConfiguredBenefitModelsNormalizeAsBenefitRoutes(t *testing.T) {
	request := mustMarshal(t, map[string]any{
		"config_yaml": []byte("benefit_models:\n  - id: glm-benefit\n"),
	})
	cfg, err := parseConfig(request)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.BenefitModels) != 1 {
		t.Fatalf("benefit models = %+v", cfg.BenefitModels)
	}
	model := cfg.BenefitModels[0]
	if model.ID != "glm-benefit" || model.Name != "glm-benefit" || model.DisplayName != "glm-benefit" || model.Source != "benefit" || model.ContextLength != 128000 || model.MaxOutputTokens != 8192 {
		t.Fatalf("benefit model was not normalized for safe routing: %+v", model)
	}
}

func TestBuiltinDiscoveryCanBeDisabled(t *testing.T) {
	resetModelCache(t)
	cfg := defaultConfig()
	cfg.ModelAgentIDs = []string{"agent"}
	cfg.DiscoverBuiltinModels = false
	var builtinCalls atomic.Int32
	agentFixture := modelFixture(t, "agent-detail")
	modelTestHost(t, func(u *url.URL, _ http.Header) (hostHTTPResponse, error) {
		switch u.Path {
		case "/v1/agent-center/agents/detail":
			return modelJSON(agentFixture), nil
		case "/v1/model/builtin":
			builtinCalls.Add(1)
			return modelJSON(modelFixture(t, "builtin")), nil
		default:
			return hostHTTPResponse{}, fmt.Errorf("unexpected path %s", u.Path)
		}
	})
	catalog := accountAgentModelCatalog(cfg, &credential{AccessKeyID: "no-builtin-ak", SecretAccessKey: "sk"}, "")
	if len(catalog.Models) != 4 || builtinCalls.Load() != 0 {
		t.Fatalf("disabled builtin discovery changed Agent catalogue: calls=%d catalogue=%+v", builtinCalls.Load(), catalog)
	}
}

func TestConfiguredBenefitModelRoutesWithoutLiveDiscovery(t *testing.T) {
	resetModelCache(t)
	cfg := defaultConfig()
	cfg.ModelAgentIDs = []string{"code-agent"}
	cfg.BenefitModels = []ModelConfig{{ID: "glm-5.3-flash", Name: "glm-5.3-flash", DisplayName: "glm-5.3-flash", Source: "benefit"}}
	var liveCalls atomic.Int32
	modelTestHost(t, func(u *url.URL, _ http.Header) (hostHTTPResponse, error) {
		liveCalls.Add(1)
		return hostHTTPResponse{}, fmt.Errorf("configured benefit routing queried live path %s", u.Path)
	})
	req := executorRequest{}
	req.Model = "glm-5.3-flash"
	req.Payload = []byte(`{"messages":[{"role":"user","content":"hello"}]}`)
	cred := &credential{AccessKeyID: "configured-benefit", SecretAccessKey: "sk"}
	_, _, headers, err := buildUpstreamRequest(cfg, req, cred, true)
	if err != nil {
		t.Fatal(err)
	}
	if headers["maas_type"] != "benefit" {
		t.Fatalf("configured benefit model lost its route: %+v", headers)
	}
	cfg.DiscoverModels = false
	_, _, headers, err = buildUpstreamRequest(cfg, req, cred, true)
	if err != nil || headers["maas_type"] != "benefit" {
		t.Fatalf("static-mode benefit model lost its route: headers=%+v err=%v", headers, err)
	}
	cfg.ModelMap = map[string]string{"friendly-benefit": "glm-5.3-flash"}
	req.Model = "friendly-benefit"
	_, _, headers, err = buildUpstreamRequest(cfg, req, cred, true)
	if err != nil || headers["maas_type"] != "benefit" {
		t.Fatalf("configured benefit alias lost its route: headers=%+v err=%v", headers, err)
	}
	if liveCalls.Load() != 0 {
		t.Fatalf("configured benefit routing issued %d live catalogue requests", liveCalls.Load())
	}
}

func TestAgentCatalogPaginatesAndDeduplicates(t *testing.T) {
	resetModelCache(t)
	cfg := defaultConfig()
	cfg.BaseURL = "https://pages.test"
	var offsets []string
	details := map[string]int{}
	modelTestHost(t, func(u *url.URL, _ http.Header) (hostHTTPResponse, error) {
		switch u.Path {
		case "/v1/agent-center/agents/useragents":
			offset := u.Query().Get("offset")
			offsets = append(offsets, offset)
			if offset == "0" {
				agents := make([]map[string]any, 100)
				for index := range agents {
					agents[index] = map[string]any{"agent_id": "duplicate-agent", "agent_name": "ActModeAgent"}
				}
				return modelJSON(mustMarshal(t, map[string]any{"total": 101, "agents": agents})), nil
			}
			if offset != "100" {
				t.Fatalf("unexpected pagination offset %q", offset)
			}
			return modelJSON([]byte(`{"total":101,"agents":[{"agent_id":"new-code-agent","agent_name":"CodeAgent"}]}`)), nil
		case "/v1/agent-center/agents/detail":
			id := u.Query().Get("agent_id")
			details[id]++
			return modelJSON(modelFixture(t, "agent-detail")), nil
		case "/v1/benefit-gateway-config":
			return modelJSON([]byte(`{"enabled":false}`)), nil
		case "/v1/model/builtin":
			return modelJSON([]byte(`{"builtinModels":[]}`)), nil
		default:
			t.Fatalf("gateway should not be called when disabled: %s", u.Path)
			return hostHTTPResponse{}, nil
		}
	})
	catalog := accountModelCatalog(cfg, &credential{AccessKeyID: "pages-ak", SecretAccessKey: "sk"}, "")
	if !reflect.DeepEqual(offsets, []string{"0", "100"}) || !reflect.DeepEqual(details, map[string]int{"duplicate-agent": 1, "new-code-agent": 1}) || len(catalog.Models) != 4 || len(catalog.Warnings) != 0 {
		t.Fatalf("pagination/deduplication failed: offsets=%v details=%v catalogue=%+v", offsets, details, catalog)
	}
}

func TestPartialModelCatalogPreservesModelsAndWarnings(t *testing.T) {
	for _, failedSource := range []string{"agent", "gateway", "gate", "builtin"} {
		t.Run(failedSource, func(t *testing.T) {
			resetModelCache(t)
			cfg := defaultConfig()
			cfg.ModelAgentIDs = []string{"explicit-agent"}
			modelTestHost(t, func(u *url.URL, _ http.Header) (hostHTTPResponse, error) {
				switch u.Path {
				case "/v1/agent-center/agents/detail":
					if failedSource == "agent" {
						return hostHTTPResponse{StatusCode: http.StatusBadGateway}, nil
					}
					return modelJSON(modelFixture(t, "agent-detail")), nil
				case "/v1/benefit-gateway-config":
					if failedSource == "gate" {
						return hostHTTPResponse{}, fmt.Errorf("gate unavailable")
					}
					return modelJSON([]byte(`{"enabled":true}`)), nil
				case "/api/v1/gateway/config":
					if failedSource == "gateway" {
						return hostHTTPResponse{StatusCode: http.StatusServiceUnavailable}, nil
					}
					return modelJSON(modelFixture(t, "gateway-config")), nil
				case "/v1/model/builtin":
					if failedSource == "builtin" {
						return hostHTTPResponse{StatusCode: http.StatusBadGateway}, nil
					}
					return modelJSON([]byte(`{"builtinModels":[{"model_id":"builtin-only","model_name":"Builtin Only","enable":true}]}`)), nil
				default:
					t.Fatalf("explicit agent IDs should bypass catalogue listing: %s", u.Path)
					return hostHTTPResponse{}, nil
				}
			})
			catalog := accountModelCatalog(cfg, &credential{AccessKeyID: "partial-ak", SecretAccessKey: "sk"}, "")
			// agent-detail contributes 4, gateway-config 3, the built-in stub 1.
			wantCount := map[string]int{"agent": 4, "gateway": 5, "gate": 5, "builtin": 7}[failedSource]
			if len(catalog.Models) != wantCount || len(catalog.Warnings) == 0 {
				t.Fatalf("partial source failure lost models or warning: %+v", catalog)
			}
		})
	}
}

func TestCriticalAgentCatalogNeverStartsLiveBenefitDiscovery(t *testing.T) {
	resetModelCache(t)
	cfg := defaultConfig()
	cfg.ModelAgentIDs = []string{"agent"}
	var benefitCalls atomic.Int32
	modelTestHost(t, func(u *url.URL, _ http.Header) (hostHTTPResponse, error) {
		switch u.Path {
		case "/v1/agent-center/agents/detail":
			return modelJSON(modelFixture(t, "agent-detail")), nil
		case "/v1/benefit-gateway-config":
			benefitCalls.Add(1)
			time.Sleep(100 * time.Millisecond)
			return hostHTTPResponse{}, fmt.Errorf("slow optional catalogue")
		default:
			return hostHTTPResponse{}, fmt.Errorf("unexpected path %s", u.Path)
		}
	})
	started := time.Now()
	catalog := accountAgentModelCatalog(cfg, &credential{AccessKeyID: "slow-benefit-ak", SecretAccessKey: "sk"}, "")
	elapsed := time.Since(started)
	if len(catalog.Models) != 4 || elapsed >= 80*time.Millisecond || benefitCalls.Load() != 0 {
		t.Fatalf("critical Agent catalogue touched live benefit discovery: elapsed=%s benefit_calls=%d catalogue=%+v", elapsed, benefitCalls.Load(), catalog)
	}
}

func TestConcurrentAgentDiscoveryCoalescesCacheMiss(t *testing.T) {
	resetModelCache(t)
	cfg := defaultConfig()
	cfg.ModelAgentIDs = []string{"agent"}
	var requests atomic.Int32
	agentFixture := modelFixture(t, "agent-detail")
	modelTestHost(t, func(u *url.URL, _ http.Header) (hostHTTPResponse, error) {
		if u.Path != "/v1/agent-center/agents/detail" {
			return hostHTTPResponse{}, fmt.Errorf("unexpected path %s", u.Path)
		}
		requests.Add(1)
		time.Sleep(20 * time.Millisecond)
		return modelJSON(agentFixture), nil
	})
	const callers = 20
	results := make(chan modelCatalogResult, callers)
	var wait sync.WaitGroup
	wait.Add(callers)
	for index := 0; index < callers; index++ {
		go func() {
			defer wait.Done()
			results <- accountAgentModelCatalog(cfg, &credential{AccessKeyID: "singleflight-ak", SecretAccessKey: "sk"}, "")
		}()
	}
	wait.Wait()
	close(results)
	for catalog := range results {
		if len(catalog.Models) != 4 {
			t.Fatalf("coalesced caller lost Agent models: %+v", catalog)
		}
	}
	if requests.Load() != 1 {
		t.Fatalf("concurrent cache miss issued %d Agent requests, want 1", requests.Load())
	}
	modelCatalogFlights.Lock()
	remainingFlights := len(modelCatalogFlights.entries)
	modelCatalogFlights.Unlock()
	if remainingFlights != 0 {
		t.Fatalf("completed discovery retained %d singleflight locks", remainingFlights)
	}
}

func TestConcurrentFailedAgentDiscoveryUsesShortNegativeCache(t *testing.T) {
	resetModelCache(t)
	cfg := defaultConfig()
	cfg.ModelAgentIDs = []string{"agent"}
	var requests atomic.Int32
	modelTestHost(t, func(u *url.URL, _ http.Header) (hostHTTPResponse, error) {
		if u.Path != "/v1/agent-center/agents/detail" {
			return hostHTTPResponse{}, fmt.Errorf("unexpected path %s", u.Path)
		}
		requests.Add(1)
		time.Sleep(20 * time.Millisecond)
		return hostHTTPResponse{StatusCode: http.StatusBadGateway}, nil
	})
	const callers = 20
	results := make(chan modelCatalogResult, callers)
	var wait sync.WaitGroup
	wait.Add(callers)
	for index := 0; index < callers; index++ {
		go func() {
			defer wait.Done()
			results <- accountAgentModelCatalog(cfg, &credential{AccessKeyID: "negative-cache-ak", SecretAccessKey: "sk"}, "")
		}()
	}
	wait.Wait()
	close(results)
	for catalog := range results {
		if len(catalog.Models) != 0 || len(catalog.Warnings) == 0 {
			t.Fatalf("failed discovery lost its negative-cache result: %+v", catalog)
		}
	}
	if requests.Load() != 1 {
		t.Fatalf("concurrent failed cache miss issued %d requests, want 1", requests.Load())
	}
	modelCatalogFlights.Lock()
	remainingFlights := len(modelCatalogFlights.entries)
	modelCatalogFlights.Unlock()
	if remainingFlights != 0 {
		t.Fatalf("failed discovery retained %d singleflight locks", remainingFlights)
	}
}

func TestExplicitFullCatalogWaitsForBenefitWithoutDetachedWork(t *testing.T) {
	resetModelCache(t)
	cfg := defaultConfig()
	cfg.ModelAgentIDs = []string{"agent"}
	benefitEntered := make(chan struct{})
	releaseBenefit := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseBenefit) }) }
	t.Cleanup(release)
	agentFixture := modelFixture(t, "agent-detail")
	modelTestHost(t, func(u *url.URL, _ http.Header) (hostHTTPResponse, error) {
		switch u.Path {
		case "/v1/agent-center/agents/detail":
			return modelJSON(agentFixture), nil
		case "/v1/model/builtin":
			return modelJSON([]byte(`{"builtinModels":[]}`)), nil
		case "/v1/benefit-gateway-config":
			close(benefitEntered)
			<-releaseBenefit
			return modelJSON([]byte(`{"enabled":false}`)), nil
		default:
			return hostHTTPResponse{}, fmt.Errorf("unexpected path %s", u.Path)
		}
	})
	done := make(chan modelCatalogResult, 1)
	go func() {
		done <- accountModelCatalog(cfg, &credential{AccessKeyID: "explicit-full-ak", SecretAccessKey: "sk"}, "")
	}()
	select {
	case <-benefitEntered:
	case <-time.After(time.Second):
		release()
		select {
		case <-done:
		case <-time.After(time.Second):
		}
		t.Fatal("explicit full refresh never reached benefit discovery")
	}
	select {
	case catalog := <-done:
		release()
		t.Fatalf("explicit full refresh returned while its synchronous benefit request was active: %+v", catalog)
	case <-time.After(20 * time.Millisecond):
	}
	release()
	var catalog modelCatalogResult
	select {
	case catalog = <-done:
	case <-time.After(time.Second):
		t.Fatal("explicit full refresh did not finish after benefit request was released")
	}
	if len(catalog.Models) != 4 || len(catalog.Warnings) != 0 {
		t.Fatalf("explicit full refresh lost Agent models: %+v", catalog)
	}
}

func TestGatewayCatalogRejectsUnsuccessfulResponses(t *testing.T) {
	for _, body := range []string{
		`{"error_code":"DENIED","error_msg":"not entitled","result":{"models":[{"model_id":"unavailable"}]}}`,
		`{"error_code":"0000","result":null}`,
		`{"error_code":"0000","result":{"models":[]}}`,
		`{"result":{"models":[{"model_id":"missing-success-code"}]}}`,
		`invalid-json`,
	} {
		models, err := parseBenefitModels([]byte(body))
		if err == nil || len(models) != 0 {
			t.Fatalf("unsuccessful gateway response advertised models: models=%+v err=%v", models, err)
		}
	}
}

func TestNoFabricatedModelWhenDiscoveryUnavailable(t *testing.T) {
	for _, mode := range []string{"failure", "empty", "malformed", "missing-credentials"} {
		t.Run(mode, func(t *testing.T) {
			resetModelCache(t)
			cfg := defaultConfig()
			cfg.ModelAgentIDs = []string{"empty-agent"}
			useModelTestConfig(t, cfg)
			modelTestHost(t, func(u *url.URL, _ http.Header) (hostHTTPResponse, error) {
				if mode == "missing-credentials" {
					t.Fatal("attempted discovery without credentials")
				}
				if mode == "failure" {
					return hostHTTPResponse{}, fmt.Errorf("unavailable")
				}
				if u.Path == "/v1/benefit-gateway-config" {
					return modelJSON([]byte(`{"enabled":false}`)), nil
				}
				if mode == "malformed" {
					return modelJSON([]byte(`not-json`)), nil
				}
				return modelJSON([]byte(`{"gpts":{"models":[]}}`)), nil
			})
			storage := mustMarshal(t, map[string]any{storageKey: credential{AccessKeyID: "empty-ak", SecretAccessKey: "sk"}})
			if mode == "missing-credentials" {
				storage = nil
			}
			raw, err := modelsForAuth(mustMarshal(t, pluginapi.AuthModelRequest{AuthProvider: providerID, StorageJSON: storage}))
			response := testEnvelopeResult[pluginapi.ModelResponse](t, raw, err)
			if len(response.Models) != 0 {
				t.Fatalf("fabricated models for %s: %+v", mode, response.Models)
			}
		})
	}
}

func TestDefaultModelsAndLegacyExampleMigration(t *testing.T) {
	for _, cfg := range []*Config{defaultConfig(), {}} {
		cfg.normalize()
		if len(cfg.Models) != 0 || cfg.DefaultModelID != "" {
			t.Fatalf("default configuration fabricates a model: %+v", cfg.Models)
		}
	}
	for _, discover := range []bool{true, false} {
		cfg := defaultConfig()
		cfg.DiscoverModels = discover
		cfg.DefaultModelID = "PanguDev_COM_QC2"
		cfg.Models = []ModelConfig{{ID: "PanguDev_COM_QC2"}}
		cfg.ModelMap = map[string]string{"pangu-dev": "PanguDev_COM_QC2"}
		cfg.normalize()
		if discover && (len(cfg.Models) != 0 || cfg.DefaultModelID != "") {
			t.Fatal("legacy automatic-discovery example still advertises the fake fallback")
		}
		if !discover && (len(cfg.Models) != 1 || cfg.DefaultModelID != "PanguDev_COM_QC2") {
			t.Fatal("explicit static configuration was removed")
		}
	}
	cfg := defaultConfig()
	cfg.Models = []ModelConfig{{ID: "user-configured-model"}}
	cfg.normalize()
	if len(cfg.Models) != 1 || cfg.Models[0].ID != "user-configured-model" {
		t.Fatal("explicit custom fallback model was removed")
	}
}
