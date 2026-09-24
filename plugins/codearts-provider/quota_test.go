package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// TestParseQuotaSnapshotMatchesUpstreamShape uses a response built from the
// exact fields the official extension reads, so the mapping stays pinned to the
// real wire format:
//
//	metrics[].name ∈ {usageDataCodeCompletions, usageDataChatMessages}
//	end_date, show.metrics, show.package, package.spec_code,
//	package.package_name_en/cn, package.package_url, package.features[].enable
func TestParseQuotaSnapshotMatchesUpstreamShape(t *testing.T) {
	body := []byte(`{
		"show": {"metrics": true, "package": true},
		"end_date": "2026-10-01",
		"metrics": [
			{"name": "usageDataCodeCompletions", "value": 42.5, "show": true},
			{"name": "usageDataChatMessages", "value": 7, "show": true},
			{"name": "someOtherMetric", "value": 1, "show": true}
		],
		"package": {
			"spec_code": "snap.enterprise",
			"package_name_en": "Enterprise",
			"package_name_cn": "企业版",
			"package_url": "https://example.invalid/upgrade",
			"features": [
				{"name": "RagAgent", "enable": true},
				{"name": "codebase", "enable": false}
			]
		}
	}`)

	snapshot, errParse := parseQuotaSnapshot(body)
	if errParse != nil {
		t.Fatalf("parseQuotaSnapshot: %v", errParse)
	}
	if len(snapshot.Meters) != 3 {
		t.Fatalf("meters = %+v, want the two known rows plus an unknown one", snapshot.Meters)
	}
	byName := map[string]quotaMeter{}
	for _, meter := range snapshot.Meters {
		byName[meter.Name] = meter
	}
	if got := byName["usageDataCodeCompletions"]; got.UsedPercent == nil || *got.UsedPercent != 42.5 {
		t.Errorf("code completions = %+v, want 42.5%%", got)
	}
	if got := byName["usageDataChatMessages"]; got.UsedPercent == nil || *got.UsedPercent != 7 {
		t.Errorf("chat messages = %+v, want 7%%", got)
	}
	// An unrecognised metric is kept under its raw name rather than dropped, so a
	// future upstream rename still reaches the dashboard.
	if got, ok := byName["someOtherMetric"]; !ok || got.Label != "someOtherMetric" {
		t.Errorf("unknown metric was dropped or localised wrongly: %+v (present=%v)", got, ok)
	}
	if snapshot.ResetDate != "2026-10-01" {
		t.Errorf("reset date = %q, want 2026-10-01", snapshot.ResetDate)
	}
	if snapshot.Plan != "snap.enterprise" {
		t.Errorf("plan = %q, want snap.enterprise", snapshot.Plan)
	}
	if snapshot.PlanName != "Enterprise" {
		t.Errorf("plan name = %q, want Enterprise (en preferred)", snapshot.PlanName)
	}
	if snapshot.PlanURL != "https://example.invalid/upgrade" {
		t.Errorf("plan url = %q", snapshot.PlanURL)
	}
	if !snapshot.Features["RagAgent"] {
		t.Error("enabled feature RagAgent was not recorded")
	}
	if snapshot.Features["codebase"] {
		t.Error("disabled feature codebase must be false")
	}
}

// TestParseQuotaSnapshotToleratesPartialResponses makes sure a minimal or empty
// document does not error out; the panel simply shows nothing.
func TestParseQuotaSnapshotToleratesPartialResponses(t *testing.T) {
	for _, body := range []string{`{}`, `{"show":{}}`, `{"metrics":null,"package":null}`} {
		snapshot, errParse := parseQuotaSnapshot([]byte(body))
		if errParse != nil {
			t.Fatalf("parseQuotaSnapshot(%s): %v", body, errParse)
		}
		if len(snapshot.Meters) != 0 {
			t.Errorf("body %s: invented meters: %+v", body, snapshot.Meters)
		}
	}
	if _, errParse := parseQuotaSnapshot([]byte("not json")); errParse == nil {
		t.Error("invalid JSON should be rejected")
	}
}

// TestUsedPercentToRemaining pins the percent-to-fraction inversion the host's
// quota model requires.
func TestUsedPercentToRemaining(t *testing.T) {
	cases := map[float64]float64{
		0:    1,
		25:   0.75,
		100:  0,
		150:  0, // clamped
		-1:   1, // upstream "not reported"
		-42:  1, // upstream "not reported"
		42.5: 0.575,
	}
	for used, want := range cases {
		if got := usedPercentToRemaining(used); got != want {
			t.Errorf("usedPercentToRemaining(%v) = %v, want %v", used, got, want)
		}
	}
}

// TestToQuotaFetchResponseShape verifies the mapped response satisfies the
// host's expected quota schema, including the subscription and reset time.
func TestToQuotaFetchResponseShape(t *testing.T) {
	eighty, twenty := 80.0, 20.0
	snapshot := quotaSnapshot{
		Plan:      "snap.enterprise",
		PlanName:  "Enterprise",
		ResetDate: "2026-10-01",
		Meters: []quotaMeter{
			{Name: "usageDataCodeCompletions", Label: "代码补全额度", UsedPercent: &eighty},
			{Name: "usageDataChatMessages", Label: "对话消息额度", UsedPercent: &twenty},
		},
	}
	raw, errMarshal := json.Marshal(toQuotaFetchResponse(snapshot))
	if errMarshal != nil {
		t.Fatalf("marshal: %v", errMarshal)
	}
	var decoded pluginapi.QuotaFetchResponse
	if errUnmarshal := json.Unmarshal(raw, &decoded); errUnmarshal != nil {
		t.Fatalf("host-shaped decode failed: %v", errUnmarshal)
	}
	if decoded.Subscription == nil || decoded.Subscription.Plan != "snap.enterprise" {
		t.Fatalf("subscription = %+v, want plan snap.enterprise", decoded.Subscription)
	}
	if len(decoded.Groups) != 1 || len(decoded.Groups[0].Buckets) != 2 {
		t.Fatalf("groups = %+v, want one group with two buckets", decoded.Groups)
	}
	// 80% used means 20% remaining.
	if got := decoded.Groups[0].Buckets[0].RemainingFraction; got != 0.2 {
		t.Errorf("code completions remaining = %v, want 0.2", got)
	}
	if got := decoded.Groups[0].Buckets[0].ResetTime; got != "2026-10-01" {
		t.Errorf("reset time = %q, want 2026-10-01", got)
	}
	if len(decoded.Summary) == 0 {
		t.Error("summary metrics are missing")
	}
}

// TestQuotaDescribeIsHonest verifies the provider reports that it cannot reset
// quota, rather than advertising unsupported capability.
func TestQuotaDescribeIsHonest(t *testing.T) {
	described := quotaDescribe()
	if described.SupportsReset {
		t.Error("the upstream cannot reset quota on demand, so supports_reset must be false")
	}
	if len(described.SupportedProviders) != 1 || described.SupportedProviders[0] != providerID {
		t.Fatalf("supported providers = %v, want [%s]", described.SupportedProviders, providerID)
	}
	if described.DisplayName == "" {
		t.Error("display name should be set for the management UI")
	}
}

// TestQuotaResetReportsUnsupported checks the reset path answers honestly
// instead of pretending success.
func TestQuotaResetReportsUnsupported(t *testing.T) {
	raw, errReset := quotaReset()
	if errReset != nil {
		t.Fatalf("quotaReset: %v", errReset)
	}
	var env struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
	}
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
		t.Fatalf("decode envelope: %v", errUnmarshal)
	}
	if !env.OK {
		t.Fatalf("quotaReset returned an error envelope: %s", raw)
	}
	var reset pluginapi.QuotaResetResponse
	if errUnmarshal := json.Unmarshal(env.Result, &reset); errUnmarshal != nil {
		t.Fatalf("decode reset response: %v", errUnmarshal)
	}
	if reset.Success {
		t.Error("reset must not report success when the upstream cannot reset")
	}
	if reset.Message == "" {
		t.Error("reset should explain why it is unsupported")
	}
}

// TestQuotaFetchRequiresAuthIndex covers the validation path.
func TestQuotaFetchRequiresAuthIndex(t *testing.T) {
	raw, errFetch := quotaFetch([]byte(`{}`))
	if errFetch != nil {
		t.Fatalf("quotaFetch: %v", errFetch)
	}
	var env struct {
		OK    bool `json:"ok"`
		Error *struct {
			Code       string `json:"code"`
			HTTPStatus int    `json:"http_status"`
		} `json:"error"`
	}
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
		t.Fatalf("decode envelope: %v", errUnmarshal)
	}
	if env.OK || env.Error == nil {
		t.Fatalf("expected a failure envelope, got %s", raw)
	}
	if env.Error.Code != "invalid_request" {
		t.Errorf("error code = %q, want invalid_request", env.Error.Code)
	}
	if env.Error.HTTPStatus != http.StatusBadRequest {
		t.Errorf("http_status = %d, want 400", env.Error.HTTPStatus)
	}
}

// TestQuotaFetchRejectsForeignProvider ensures the quota provider only answers
// for its own credentials.
func TestQuotaFetchRejectsForeignProvider(t *testing.T) {
	body, _ := json.Marshal(pluginapi.QuotaFetchRequest{Provider: "openai", AuthIndex: "x"})
	raw, errFetch := quotaFetch(body)
	if errFetch != nil {
		t.Fatalf("quotaFetch: %v", errFetch)
	}
	var env struct {
		OK    bool `json:"ok"`
		Error *struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
		t.Fatalf("decode envelope: %v", errUnmarshal)
	}
	if env.OK {
		t.Fatal("a foreign provider must not be handled")
	}
	if env.Error == nil || env.Error.Code != "unsupported_provider" {
		t.Fatalf("error = %+v, want code unsupported_provider", env.Error)
	}
}

// TestQuotaCacheRoundTrip covers the cache used to keep the panel cheap.
func TestQuotaCacheRoundTrip(t *testing.T) {
	index := "test-cache-index"
	quotas.put(quotaSnapshot{AuthIndex: index, Plan: "p"})
	got, ok := quotas.get(index)
	if !ok || got.Plan != "p" {
		t.Fatalf("cache lookup = %+v, %v", got, ok)
	}
	if _, ok := quotas.all()[index]; !ok {
		t.Error("the snapshot is missing from the full view")
	}
	quotas.forget(index)
	if _, ok := quotas.get(index); ok {
		t.Error("forget did not remove the snapshot")
	}
}

// TestBuildAuthFileDocumentKeepsPluginKey verifies the persisted document uses
// the plugin-owned storage key so host-managed keys survive a round trip.
func TestBuildAuthFileDocumentKeepsPluginKey(t *testing.T) {
	cred := credential{
		AccessKeyID:     "AK",
		SecretAccessKey: "SK",
		SecurityToken:   "TOK",
		DomainID:        "DOM",
		UserName:        "tester",
		LoginType:       "AKSK",
	}
	raw, errBuild := buildAuthFileDocument(cred)
	if errBuild != nil {
		t.Fatalf("buildAuthFileDocument: %v", errBuild)
	}
	var doc map[string]any
	if errUnmarshal := json.Unmarshal(raw, &doc); errUnmarshal != nil {
		t.Fatalf("document is not valid JSON: %v", errUnmarshal)
	}
	if doc["type"] != providerID {
		t.Errorf("type = %v, want %s", doc["type"], providerID)
	}
	if _, ok := doc[storageKey]; !ok {
		t.Fatalf("the document must nest the credential under %q (keys: %v)", storageKey, doc)
	}
	// And it must round-trip back into a usable credential.
	back, errCred := credentialFromStorage(raw)
	if errCred != nil {
		t.Fatalf("credentialFromStorage: %v", errCred)
	}
	if back == nil || back.AccessKeyID != "AK" || back.SecurityToken != "TOK" {
		t.Fatalf("round trip produced %+v", back)
	}
}

// TestMergeCredentialFilePreservesHostKeys documents that a scheduled renewal
// must not discard host-owned keys such as priority or note.
func TestMergeCredentialFilePreservesHostKeys(t *testing.T) {
	existing := []byte(`{"type":"codearts-provider","priority":7,"note":"keep me","disabled":false}`)
	storage := []byte(`{"codearts_provider_credential":{"access_key_id":"AK2","secret_access_key":"SK2","security_token":"TOK2"}}`)
	merged, errMerge := mergeCredentialFile(existing, storage)
	if errMerge != nil {
		t.Fatalf("mergeCredentialFile: %v", errMerge)
	}
	var doc map[string]any
	if errUnmarshal := json.Unmarshal(merged, &doc); errUnmarshal != nil {
		t.Fatalf("merged document is not valid JSON: %v", errUnmarshal)
	}
	if doc["priority"] != float64(7) {
		t.Errorf("priority = %v, want it preserved as 7", doc["priority"])
	}
	if doc["note"] != "keep me" {
		t.Errorf("note = %v, want it preserved", doc["note"])
	}
	if _, ok := doc[storageKey]; !ok {
		t.Error("the refreshed credential was not written")
	}
	cred, errCred := credentialFromStorage(merged)
	if errCred != nil || cred.AccessKeyID != "AK2" {
		t.Fatalf("refreshed credential = %+v, err %v", cred, errCred)
	}
}

// TestManagementRouteSuffix covers the path matching used by the dispatcher for
// both authenticated routes and unauthenticated resources.
func TestManagementRouteSuffix(t *testing.T) {
	cases := map[string]string{
		"/v0/management/codearts-provider/quota":        "/quota",
		"/v0/management/codearts-provider/schedule/run": "/schedule/run",
		"/v0/resource/plugins/codearts-provider/panel":  "/panel",
		"/v0/resource/plugins/codearts-provider/status": "/status",
		"/v0/management/codearts-provider/quota/":       "/quota",
		"/v0/management/codearts-provider":              "/",
	}
	for input, want := range cases {
		if got := managementRouteSuffix(input); got != want {
			t.Errorf("managementRouteSuffix(%q) = %q, want %q", input, got, want)
		}
	}
}

// TestParseQuotaSnapshotFollowsTheMetricMigration uses the live document captured
// on 2026-09-21, where the message-count row was retired in favour of a token
// row. The old code read only the two retired names, so the dashboard showed no
// chat allowance at all while upstream was reporting one.
func TestParseQuotaSnapshotFollowsTheMetricMigration(t *testing.T) {
	body := []byte(`{"metrics":[` +
		`{"name":"usageDataChatMessages","value":-1,"usage_token_num":0,"package_token_amount":0,"show":false},` +
		`{"name":"usageTokenChatMessages","value":0,"usage_token_num":2167,"package_token_amount":5000000,"show":true}` +
		`],"start_date":"2026-09-02","end_date":"2026-10-03","package":{"spec_code":"codearts.agent.trial"}}`)

	snapshot, errParse := parseQuotaSnapshot(body)
	if errParse != nil {
		t.Fatal(errParse)
	}
	if len(snapshot.Meters) != 1 {
		t.Fatalf("meters = %+v, want only the displayed token row", snapshot.Meters)
	}
	meter := snapshot.Meters[0]
	if meter.Name != "usageTokenChatMessages" || meter.Label != "对话 token 额度" {
		t.Errorf("meter = %+v", meter)
	}
	if meter.UsedTokens != 2167 || meter.AllowanceTokens != 5000000 {
		t.Errorf("token counters were lost: %+v", meter)
	}
	if meter.UsedPercent == nil || *meter.UsedPercent != 0 {
		t.Errorf("used percent = %v, want a reported 0", meter.UsedPercent)
	}
}

// TestParseQuotaSnapshotNeverInventsZero verifies the failure mode the migration
// caused: an absent metric used to be reported as "0% used", which is a claim of
// an untouched quota rather than the truth ("nothing was reported").
func TestParseQuotaSnapshotNeverInventsZero(t *testing.T) {
	// Only the retired row exists, and upstream marks it hidden.
	snapshot, errParse := parseQuotaSnapshot([]byte(`{"metrics":[` +
		`{"name":"usageDataChatMessages","value":-1,"show":false}]}`))
	if errParse != nil {
		t.Fatal(errParse)
	}
	if len(snapshot.Meters) != 0 {
		t.Fatalf("hidden metric was displayed: %+v", snapshot.Meters)
	}
	response := toQuotaFetchResponse(snapshot)
	for _, bucket := range response.Groups {
		t.Fatalf("an unknown quota was advertised to the host: %+v", bucket)
	}
	for _, metric := range response.Summary {
		if strings.Contains(strings.ToLower(metric.Key), "chat") {
			t.Fatalf("summary invented a value: %+v", metric)
		}
	}
}

// TestQuotaMeterTokenCountersWithoutPercentage covers a metric that reports only
// counters: it must reach the panel and the host as counters, and must not be
// turned into a percentage the upstream never published.
func TestQuotaMeterTokenCountersWithoutPercentage(t *testing.T) {
	snapshot, errParse := parseQuotaSnapshot([]byte(`{"metrics":[` +
		`{"name":"usageTokenSomethingNew","value":-1,"usage_token_num":30,"package_token_amount":100,"show":true}]}`))
	if errParse != nil {
		t.Fatal(errParse)
	}
	if len(snapshot.Meters) != 1 {
		t.Fatalf("meters = %+v", snapshot.Meters)
	}
	meter := snapshot.Meters[0]
	if meter.UsedPercent != nil {
		t.Errorf("a negative value became a percentage: %v", *meter.UsedPercent)
	}
	if _, ok := meter.remainingFraction(); ok {
		t.Error("a meter without a ratio must not be advertised as a bucket")
	}
	if !strings.Contains(meter.Describe(), "30 of 100 tokens") {
		t.Errorf("Describe() = %q", meter.Describe())
	}
	if meter.Label != "usageTokenSomethingNew" {
		t.Errorf("unknown metric lost its raw name: %q", meter.Label)
	}
}
