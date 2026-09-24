package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestRegistrationMetadataUsesPluginRepository(t *testing.T) {
	metadata, ok := registrationResponse()["metadata"].(pluginapi.Metadata)
	if !ok {
		t.Fatalf("registration metadata has unexpected type %T", registrationResponse()["metadata"])
	}
	if metadata.GitHubRepository != pluginRepositoryURL {
		t.Fatalf("GitHub repository = %q, want %q", metadata.GitHubRepository, pluginRepositoryURL)
	}
}

// TestSchedulerSkipsForeignProvider verifies the plugin scheduler only acts on
// its own provider. Handling another provider's request would silently override
// the host's own scheduling for a service this plugin knows nothing about.
func TestSchedulerSkipsForeignProvider(t *testing.T) {
	body, _ := json.Marshal(pluginapi.SchedulerPickRequest{
		Provider: "codex",
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "a", Provider: "codex"},
		},
	})
	raw, errPick := schedulerPick(body)
	if errPick != nil {
		t.Fatalf("schedulerPick: %v", errPick)
	}
	var env struct {
		Result pluginapi.SchedulerPickResponse `json:"result"`
	}
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
		t.Fatalf("decode: %v", errUnmarshal)
	}
	if env.Result.Handled {
		t.Fatal("a foreign provider must not be handled")
	}
}

// TestSchedulerRotatesAcrossAccounts is the core value of the capability: with
// per-account rate limits upstream, consecutive requests should not keep hitting
// the same credential.
func TestSchedulerRotatesAcrossAccounts(t *testing.T) {
	currentConfig.Store(defaultConfig())
	defer currentConfig.Store(defaultConfig())
	picker.remember("")

	candidates := []pluginapi.SchedulerAuthCandidate{
		{ID: "codearts-provider-a.json", Provider: providerID},
		{ID: "codearts-provider-b.json", Provider: providerID},
		{ID: "codearts-provider-c.json", Provider: providerID},
	}
	body, _ := json.Marshal(pluginapi.SchedulerPickRequest{Provider: providerID, Candidates: candidates})

	seen := map[string]int{}
	for i := 0; i < 6; i++ {
		raw, errPick := schedulerPick(body)
		if errPick != nil {
			t.Fatalf("pick %d: %v", i, errPick)
		}
		var env struct {
			Result pluginapi.SchedulerPickResponse `json:"result"`
		}
		if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
			t.Fatalf("decode: %v", errUnmarshal)
		}
		if !env.Result.Handled || env.Result.AuthID == "" {
			t.Fatalf("pick %d was not handled: %s", i, raw)
		}
		seen[env.Result.AuthID]++
	}
	if len(seen) != 3 {
		t.Fatalf("picks covered %d accounts over 6 requests, want all 3: %v", len(seen), seen)
	}
	for id, count := range seen {
		if count != 2 {
			t.Errorf("account %s was picked %d times, want 2 (even rotation)", id, count)
		}
	}
}

// TestSchedulerSkipsUnavailableCandidates ensures a credential the host already
// marked unusable is never chosen, which would otherwise produce a guaranteed
// failure.
func TestSchedulerSkipsUnavailableCandidates(t *testing.T) {
	currentConfig.Store(defaultConfig())
	defer currentConfig.Store(defaultConfig())
	picker.remember("")

	body, _ := json.Marshal(pluginapi.SchedulerPickRequest{
		Provider: providerID,
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "bad", Provider: providerID, Status: "unavailable"},
			{ID: "good", Provider: providerID},
		},
	})
	for i := 0; i < 4; i++ {
		raw, _ := schedulerPick(body)
		var env struct {
			Result pluginapi.SchedulerPickResponse `json:"result"`
		}
		_ = json.Unmarshal(raw, &env)
		if env.Result.AuthID != "good" {
			t.Fatalf("picked %q, want the available candidate", env.Result.AuthID)
		}
	}
}

// TestSchedulerPrefersConfiguredAccount covers the preferred strategy and its
// fallback when the preferred account is not a candidate.
func TestSchedulerPrefersConfiguredAccount(t *testing.T) {
	cfg := defaultConfig()
	cfg.Scheduler = SchedulerConfig{Strategy: "preferred", PreferredAuthID: "codearts-provider-main.json"}
	currentConfig.Store(cfg)
	defer currentConfig.Store(defaultConfig())

	body, _ := json.Marshal(pluginapi.SchedulerPickRequest{
		Provider: providerID,
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "codearts-provider-other.json", Provider: providerID},
			{ID: "codearts-provider-main.json", Provider: providerID},
		},
	})
	raw, _ := schedulerPick(body)
	var env struct {
		Result pluginapi.SchedulerPickResponse `json:"result"`
	}
	_ = json.Unmarshal(raw, &env)
	if env.Result.AuthID != "codearts-provider-main.json" {
		t.Fatalf("picked %q, want the preferred account", env.Result.AuthID)
	}

	// With the preferred account absent, rotation takes over instead of failing.
	body2, _ := json.Marshal(pluginapi.SchedulerPickRequest{
		Provider: providerID,
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "codearts-provider-other.json", Provider: providerID},
		},
	})
	raw2, _ := schedulerPick(body2)
	_ = json.Unmarshal(raw2, &env)
	if env.Result.AuthID != "codearts-provider-other.json" {
		t.Fatalf("picked %q, want the fallback candidate", env.Result.AuthID)
	}
}

// TestSchedulerHostStrategyDelegates verifies the explicit opt-out.
func TestSchedulerHostStrategyDelegates(t *testing.T) {
	cfg := defaultConfig()
	cfg.Scheduler = SchedulerConfig{Strategy: "host", Delegate: "round-robin"}
	currentConfig.Store(cfg)
	defer currentConfig.Store(defaultConfig())

	body, _ := json.Marshal(pluginapi.SchedulerPickRequest{
		Provider: providerID,
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "a", Provider: providerID},
		},
	})
	raw, _ := schedulerPick(body)
	var env struct {
		Result pluginapi.SchedulerPickResponse `json:"result"`
	}
	_ = json.Unmarshal(raw, &env)
	if !env.Result.Handled || env.Result.DelegateBuiltin != "round-robin" {
		t.Fatalf("host strategy should delegate, got %+v", env.Result)
	}
	if env.Result.AuthID != "" {
		t.Fatalf("host strategy must not pick an account itself, got %q", env.Result.AuthID)
	}
}

// TestSchedulerNoCandidatesStaysUnhandled ensures the plugin does not claim a
// request it cannot satisfy.
func TestSchedulerNoCandidatesStaysUnhandled(t *testing.T) {
	currentConfig.Store(defaultConfig())
	defer currentConfig.Store(defaultConfig())
	body, _ := json.Marshal(pluginapi.SchedulerPickRequest{Provider: providerID})
	raw, _ := schedulerPick(body)
	var env struct {
		Result pluginapi.SchedulerPickResponse `json:"result"`
	}
	_ = json.Unmarshal(raw, &env)
	if env.Result.Handled {
		t.Fatal("with no usable candidate the pick must stay unhandled")
	}
}

// TestSchedulerConfigNormalize pins the config cleanup, including dropping an
// unknown delegate that the host would ignore anyway.
func TestSchedulerConfigNormalize(t *testing.T) {
	cfg := SchedulerConfig{Strategy: "  ROUND-ROBIN ", PreferredAuthID: " x ", Delegate: "nonsense"}
	cfg.normalize()
	if cfg.Strategy != "round-robin" {
		t.Errorf("strategy = %q, want round-robin", cfg.Strategy)
	}
	if cfg.PreferredAuthID != "x" {
		t.Errorf("preferred = %q, want trimmed", cfg.PreferredAuthID)
	}
	if cfg.Delegate != "" {
		t.Errorf("delegate = %q, want it dropped when unknown", cfg.Delegate)
	}
}

// TestUsageRollupAggregates covers the usage observer: totals, per-model and
// per-account buckets, and failure counting.
func TestUsageRollupAggregates(t *testing.T) {
	local := &usageRollup{ByModel: map[string]*usageBucket{}, ByAuth: map[string]*usageBucket{}}

	local.observe(pluginapi.UsageRecord{
		Model:   "PanguDev_COM_QC2",
		AuthID:  "codearts-provider-a.json",
		Latency: 1500 * 1e6,
		Detail:  pluginapi.UsageDetail{InputTokens: 100, OutputTokens: 50, TotalTokens: 150},
	})
	local.observe(pluginapi.UsageRecord{
		Model:   "PanguDev_COM_QC2",
		AuthID:  "codearts-provider-b.json",
		Failed:  true,
		Failure: pluginapi.UsageFailure{StatusCode: 429, Body: "rate limited"},
		Detail:  pluginapi.UsageDetail{InputTokens: 10, OutputTokens: 0},
	})

	snapshot := local.snapshot()
	if snapshot["requests"] != int64(2) {
		t.Errorf("requests = %v, want 2", snapshot["requests"])
	}
	if snapshot["failures"] != int64(1) {
		t.Errorf("failures = %v, want 1", snapshot["failures"])
	}
	// Totals fall back to input+output when the host omits total_tokens.
	if snapshot["total_tokens"] != int64(160) {
		t.Errorf("total_tokens = %v, want 160", snapshot["total_tokens"])
	}
	byModel := snapshot["by_model"].(map[string]usageBucket)
	bucket, ok := byModel["PanguDev_COM_QC2"]
	if !ok {
		t.Fatalf("model bucket missing: %v", byModel)
	}
	if bucket.Requests != 2 || bucket.Failures != 1 || bucket.TotalTokens != 160 {
		t.Errorf("model bucket = %+v", bucket)
	}
	byAuth := snapshot["by_auth"].(map[string]usageBucket)
	if len(byAuth) != 2 {
		t.Errorf("auth buckets = %d, want 2", len(byAuth))
	}
	recent := snapshot["recent"].([]usageSample)
	if len(recent) != 2 {
		t.Fatalf("recent = %d, want 2", len(recent))
	}
	// The failure detail must be captured for diagnostics.
	foundFailure := false
	for _, sample := range recent {
		if sample.Failed && strings.Contains(sample.Error, "rate limited") {
			foundFailure = true
		}
	}
	if !foundFailure {
		t.Error("the failure body should be preserved in the recent sample")
	}
}

// TestUsageRollupIsBounded guards the memory ceiling: the plugin lives inside
// the gateway process, so history must not grow without limit.
func TestUsageRollupIsBounded(t *testing.T) {
	local := &usageRollup{ByModel: map[string]*usageBucket{}, ByAuth: map[string]*usageBucket{}}
	for i := 0; i < maxRecentSamples+50; i++ {
		local.observe(pluginapi.UsageRecord{Model: "m", Detail: pluginapi.UsageDetail{InputTokens: 1}})
	}
	recent := local.snapshot()["recent"].([]usageSample)
	if len(recent) != maxRecentSamples {
		t.Fatalf("recent samples = %d, want the cap %d", len(recent), maxRecentSamples)
	}
}

// TestThinkingApplyOnlyInAgentMode documents that the native endpoint is left
// untouched rather than being sent fields it does not document.
func TestThinkingApplyOnlyInAgentMode(t *testing.T) {
	cfg := defaultConfig()
	cfg.APIMode = "native"
	currentConfig.Store(cfg)
	defer currentConfig.Store(defaultConfig())

	original := []byte(`{"messages":[]}`)
	body, _ := json.Marshal(pluginapi.ThinkingApplyRequest{
		Body:   original,
		Config: pluginapi.ThinkingConfig{Mode: "high"},
	})
	raw, errApply := thinkingApply(body)
	if errApply != nil {
		t.Fatalf("thinkingApply: %v", errApply)
	}
	var env struct {
		Result pluginapi.PayloadResponse `json:"result"`
	}
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
		t.Fatalf("decode: %v", errUnmarshal)
	}
	if string(env.Result.Body) != string(original) {
		t.Fatalf("native mode must return the body unchanged, got %s", env.Result.Body)
	}
}

// TestThinkingApplyAgentModeSetsEffort verifies the mapping onto the field the
// upstream actually accepts.
func TestThinkingApplyAgentModeSetsEffort(t *testing.T) {
	cfg := defaultConfig()
	cfg.APIMode = "agent"
	currentConfig.Store(cfg)
	defer currentConfig.Store(defaultConfig())

	body, _ := json.Marshal(pluginapi.ThinkingApplyRequest{
		Body:   []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`),
		Config: pluginapi.ThinkingConfig{Mode: "high", Budget: 32000},
	})
	raw, errApply := thinkingApply(body)
	if errApply != nil {
		t.Fatalf("thinkingApply: %v", errApply)
	}
	var env struct {
		Result pluginapi.PayloadResponse `json:"result"`
	}
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
		t.Fatalf("decode: %v", errUnmarshal)
	}
	var payload map[string]any
	if errUnmarshal := json.Unmarshal(env.Result.Body, &payload); errUnmarshal != nil {
		t.Fatalf("body is not JSON: %v", errUnmarshal)
	}
	if payload["reasoning_effort"] != "high" {
		t.Errorf("reasoning_effort = %v, want high", payload["reasoning_effort"])
	}
	if payload["max_tokens"] != float64(32000) {
		t.Errorf("max_tokens = %v, want 32000 from the budget", payload["max_tokens"])
	}
	// Client fields must survive the rewrite.
	if _, ok := payload["messages"]; !ok {
		t.Error("the messages array was dropped")
	}
}

// TestNormalizeReasoningEffort covers the level/mode/budget mapping table.
func TestNormalizeReasoningEffort(t *testing.T) {
	cases := []struct {
		config pluginapi.ThinkingConfig
		want   string
	}{
		{pluginapi.ThinkingConfig{Level: "high"}, "high"},
		{pluginapi.ThinkingConfig{Level: "MINIMAL"}, "low"},
		{pluginapi.ThinkingConfig{Mode: "off"}, ""},
		{pluginapi.ThinkingConfig{Mode: "dynamic"}, "medium"},
		{pluginapi.ThinkingConfig{Budget: 20000}, "high"},
		{pluginapi.ThinkingConfig{Budget: 2000}, "medium"},
		{pluginapi.ThinkingConfig{}, ""},
	}
	for _, tc := range cases {
		if got := normalizeReasoningEffort(tc.config); got != tc.want {
			t.Errorf("normalizeReasoningEffort(%+v) = %q, want %q", tc.config, got, tc.want)
		}
	}
}

// TestThinkingApplyToleratesUnparsableBody ensures a body we cannot parse is
// passed through instead of being replaced with an empty object.
func TestThinkingApplyToleratesUnparsableBody(t *testing.T) {
	cfg := defaultConfig()
	cfg.APIMode = "agent"
	currentConfig.Store(cfg)
	defer currentConfig.Store(defaultConfig())

	body, _ := json.Marshal(pluginapi.ThinkingApplyRequest{
		Body:   []byte("not json"),
		Config: pluginapi.ThinkingConfig{Mode: "high"},
	})
	raw, errApply := thinkingApply(body)
	if errApply != nil {
		t.Fatalf("thinkingApply: %v", errApply)
	}
	var env struct {
		Result pluginapi.PayloadResponse `json:"result"`
	}
	_ = json.Unmarshal(raw, &env)
	if string(env.Result.Body) != "not json" {
		t.Fatalf("body = %q, want it passed through unchanged", env.Result.Body)
	}
}
