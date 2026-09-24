package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// TestManagementRegistrationWireKeys pins the JSON keys of the management
// registration result. The host decodes this result into its own
// rpcManagementRegistrationResponse, whose fields are tagged `routes` and
// `resources`; emitting "Routes"/"Resources" would be silently ignored and the
// plugin would expose no pages.
func TestManagementRegistrationWireKeys(t *testing.T) {
	raw, errMarshal := json.Marshal(managementRegistration())
	if errMarshal != nil {
		t.Fatalf("marshal management registration: %v", errMarshal)
	}
	var decoded struct {
		Routes    []pluginapi.ManagementRoute `json:"routes"`
		Resources []pluginapi.ResourceRoute   `json:"resources"`
	}
	if errUnmarshal := json.Unmarshal(raw, &decoded); errUnmarshal != nil {
		t.Fatalf("host-shaped decode failed: %v", errUnmarshal)
	}
	// The panel is the browsable menu entry; /status stays available as a
	// machine-readable resource but carries no menu.
	var panel *pluginapi.ResourceRoute
	for index := range decoded.Resources {
		if decoded.Resources[index].Path == "/panel" {
			panel = &decoded.Resources[index]
		}
	}
	if panel == nil {
		t.Fatalf("the /panel resource is missing (raw: %s)", raw)
	}
	if panel.Menu == "" {
		t.Fatal("the panel resource must set Menu so it is browsable")
	}
	if len(decoded.Routes) == 0 {
		t.Fatalf("no management routes were declared (raw: %s)", raw)
	}
	// Every declared route must carry a method and an absolute path.
	for _, route := range decoded.Routes {
		if route.Method == "" || !strings.HasPrefix(route.Path, "/") {
			t.Fatalf("route %+v is malformed", route)
		}
	}
}

// TestStreamResponseWireKeys pins the async streaming reply keys against the
// host's rpcExecutorStreamResponse.
func TestStreamResponseWireKeys(t *testing.T) {
	envelopeRaw, errEnvelope := okEnvelope(map[string]any{
		"headers": http.Header{"Content-Type": []string{"text/event-stream"}},
	})
	if errEnvelope != nil {
		t.Fatalf("okEnvelope: %v", errEnvelope)
	}
	var env struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
	}
	if errUnmarshal := json.Unmarshal(envelopeRaw, &env); errUnmarshal != nil {
		t.Fatalf("decode envelope: %v", errUnmarshal)
	}
	if !env.OK {
		t.Fatal("envelope is not ok")
	}
	var decoded struct {
		Headers http.Header                     `json:"headers"`
		Chunks  []pluginapi.ExecutorStreamChunk `json:"chunks"`
	}
	if errUnmarshal := json.Unmarshal(env.Result, &decoded); errUnmarshal != nil {
		t.Fatalf("host-shaped stream decode failed: %v", errUnmarshal)
	}
	if decoded.Headers.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("content-type = %q, want text/event-stream", decoded.Headers.Get("Content-Type"))
	}
}

// TestModelRegistrationDecodesAsHostExpects verifies the model registration
// payload against pluginapi.ModelRegistrationResponse, which is untagged and
// therefore uses Go field names on the wire.
func TestModelRegistrationDecodesAsHostExpects(t *testing.T) {
	cfg := defaultConfig()
	cfg.Models = []ModelConfig{{ID: "wire-test-model", Name: "wire-test-model"}}
	useModelTestConfig(t, cfg)
	raw, errMarshal := json.Marshal(modelRegistration())
	if errMarshal != nil {
		t.Fatalf("marshal model registration: %v", errMarshal)
	}
	var decoded pluginapi.ModelRegistrationResponse
	if errUnmarshal := json.Unmarshal(raw, &decoded); errUnmarshal != nil {
		t.Fatalf("host-shaped model decode failed: %v", errUnmarshal)
	}
	if decoded.Provider != providerID {
		t.Fatalf("provider = %q, want %q", decoded.Provider, providerID)
	}
	if len(decoded.Models) == 0 {
		t.Fatal("no models were registered")
	}
}

// TestErrorEnvelopeCarriesHTTPStatus ensures upstream failures are classified
// for the client rather than collapsing into a generic 500.
func TestErrorEnvelopeCarriesHTTPStatus(t *testing.T) {
	raw := errorEnvelope("rate_limit_exceeded", "slow down", http.StatusTooManyRequests)

	var env pluginabi.Envelope
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
		t.Fatalf("decode error envelope: %v", errUnmarshal)
	}
	if env.OK {
		t.Fatal("error envelope reported ok=true")
	}
	if env.Error == nil {
		t.Fatal("error envelope has no error object")
	}
	if env.Error.HTTPStatus != http.StatusTooManyRequests {
		t.Fatalf("http_status = %d, want 429", env.Error.HTTPStatus)
	}
	if env.Error.Code != "rate_limit_exceeded" {
		t.Fatalf("code = %q, want rate_limit_exceeded", env.Error.Code)
	}
}

// TestRegistrationDeclaresImplementedCapabilitiesOnly guards against
// over-declaring capabilities: every enabled flag must have a handler.
func TestRegistrationDeclaresImplementedCapabilitiesOnly(t *testing.T) {
	raw, errMarshal := json.Marshal(registrationResponse())
	if errMarshal != nil {
		t.Fatalf("marshal registration: %v", errMarshal)
	}
	var decoded struct {
		SchemaVersion uint32 `json:"schema_version"`
		Capabilities  struct {
			ModelProvider         bool     `json:"model_provider"`
			AuthProvider          bool     `json:"auth_provider"`
			Executor              bool     `json:"executor"`
			ManagementAPI         bool     `json:"management_api"`
			QuotaProvider         bool     `json:"quota_provider"`
			UsagePlugin           bool     `json:"usage_plugin"`
			ThinkingApplier       bool     `json:"thinking_applier"`
			Scheduler             bool     `json:"scheduler"`
			ExecutorModelScope    string   `json:"executor_model_scope"`
			ExecutorInputFormats  []string `json:"executor_input_formats"`
			ExecutorOutputFormats []string `json:"executor_output_formats"`
		} `json:"capabilities"`
	}
	if errUnmarshal := json.Unmarshal(raw, &decoded); errUnmarshal != nil {
		t.Fatalf("decode registration: %v", errUnmarshal)
	}
	if decoded.SchemaVersion != pluginabi.SchemaVersion {
		t.Fatalf("schema_version = %d, want %d", decoded.SchemaVersion, pluginabi.SchemaVersion)
	}
	// Every capability the plugin claims must have a handler in handleMethod;
	// over-declaring would make the host call methods we do not implement.
	for name, declared := range map[string]bool{
		"model_provider":   decoded.Capabilities.ModelProvider,
		"auth_provider":    decoded.Capabilities.AuthProvider,
		"executor":         decoded.Capabilities.Executor,
		"management_api":   decoded.Capabilities.ManagementAPI,
		"quota_provider":   decoded.Capabilities.QuotaProvider,
		"usage_plugin":     decoded.Capabilities.UsagePlugin,
		"thinking_applier": decoded.Capabilities.ThinkingApplier,
		"scheduler":        decoded.Capabilities.Scheduler,
	} {
		if !declared {
			t.Errorf("capability %s should be declared: %s", name, raw)
		}
	}
	// An executor must declare at least one input and output format.
	if len(decoded.Capabilities.ExecutorInputFormats) == 0 || len(decoded.Capabilities.ExecutorOutputFormats) == 0 {
		t.Fatal("executor must declare input and output formats")
	}
	if decoded.Capabilities.ExecutorModelScope == "" {
		t.Fatal("executor_model_scope must be set")
	}
}

// TestAuthParseUnhandledForForeignProvider ensures the parser defers to other
// providers instead of claiming every auth file.
func TestAuthParseUnhandledForForeignProvider(t *testing.T) {
	request, errMarshal := json.Marshal(pluginapi.AuthParseRequest{
		Provider: "openai",
		FileName: "openai.json",
		RawJSON:  []byte(`{"type":"openai","api_key":"sk-x"}`),
	})
	if errMarshal != nil {
		t.Fatalf("marshal parse request: %v", errMarshal)
	}
	raw, errParse := authParse(request)
	if errParse != nil {
		t.Fatalf("authParse: %v", errParse)
	}
	var env struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
	}
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
		t.Fatalf("decode envelope: %v", errUnmarshal)
	}
	var parsed pluginapi.AuthParseResponse
	if errUnmarshal := json.Unmarshal(env.Result, &parsed); errUnmarshal != nil {
		t.Fatalf("decode parse response: %v", errUnmarshal)
	}
	if parsed.Handled {
		t.Fatal("a foreign provider must not be handled")
	}
}

// TestAuthParseAcceptsOwnCredentialFile verifies the documented credential file
// shape is recognised and mapped onto an auth record.
func TestAuthParseAcceptsOwnCredentialFile(t *testing.T) {
	request, errMarshal := json.Marshal(pluginapi.AuthParseRequest{
		Provider: providerID,
		FileName: "codearts-provider-me.json",
		RawJSON: []byte(`{
			"type": "codearts-provider",
			"access_key_id": "AKTEST",
			"secret_access_key": "SKTEST",
			"security_token": "TOK",
			"domain_id": "DOM",
			"user_name": "tester"
		}`),
	})
	if errMarshal != nil {
		t.Fatalf("marshal parse request: %v", errMarshal)
	}
	raw, errParse := authParse(request)
	if errParse != nil {
		t.Fatalf("authParse: %v", errParse)
	}
	var env struct {
		Result json.RawMessage `json:"result"`
	}
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
		t.Fatalf("decode envelope: %v", errUnmarshal)
	}
	var parsed pluginapi.AuthParseResponse
	if errUnmarshal := json.Unmarshal(env.Result, &parsed); errUnmarshal != nil {
		t.Fatalf("decode parse response: %v", errUnmarshal)
	}
	if !parsed.Handled {
		t.Fatal("the plugin's own credential file must be handled")
	}
	if parsed.Auth.Provider != providerID {
		t.Fatalf("provider = %q, want %q", parsed.Auth.Provider, providerID)
	}
	if parsed.Auth.Label != "tester" {
		t.Fatalf("label = %q, want tester", parsed.Auth.Label)
	}
	// The credential must round-trip through the opaque storage blob.
	cred, errCred := credentialFromStorage(parsed.Auth.StorageJSON)
	if errCred != nil {
		t.Fatalf("credentialFromStorage: %v", errCred)
	}
	if cred == nil || !cred.valid() {
		t.Fatalf("stored credential is not usable: %+v", cred)
	}
	if cred.AccessKeyID != "AKTEST" || cred.SecurityToken != "TOK" || cred.DomainID != "DOM" {
		t.Fatalf("credential round-trip mismatch: %+v", cred)
	}
}

// TestAuthParseRejectsIncompleteCredential makes sure a file that looks like
// ours but lacks keys is not claimed.
func TestAuthParseRejectsIncompleteCredential(t *testing.T) {
	request, _ := json.Marshal(pluginapi.AuthParseRequest{
		Provider: providerID,
		FileName: "codearts-provider-partial.json",
		RawJSON:  []byte(`{"type":"codearts-provider","access_key_id":"AKONLY"}`),
	})
	raw, errParse := authParse(request)
	if errParse != nil {
		t.Fatalf("authParse: %v", errParse)
	}
	var env struct {
		Result json.RawMessage `json:"result"`
	}
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
		t.Fatalf("decode envelope: %v", errUnmarshal)
	}
	var parsed pluginapi.AuthParseResponse
	if errUnmarshal := json.Unmarshal(env.Result, &parsed); errUnmarshal != nil {
		t.Fatalf("decode parse response: %v", errUnmarshal)
	}
	if parsed.Handled {
		t.Fatal("an incomplete credential must not be claimed")
	}
}

// TestRefreshDeadlineForPermanentKeyPair ensures a credential without a security
// token is not scheduled for a renewal that would fail every hour.
func TestRefreshDeadlineForPermanentKeyPair(t *testing.T) {
	permanent := &credential{AccessKeyID: "AK", SecretAccessKey: "SK"}
	if got := refreshDeadline(permanent); time.Until(got) < 20*24*time.Hour {
		t.Fatalf("permanent key pair refresh deadline = %v, want far in the future", got)
	}

	expiring := &credential{
		AccessKeyID:     "AK",
		SecretAccessKey: "SK",
		SecurityToken:   "TOK",
		ExpiresAt:       time.Now().Add(6 * time.Hour).UTC().Format(time.RFC3339),
	}
	deadline := refreshDeadline(expiring)
	if delta := time.Until(deadline); delta > 6*time.Hour || delta < 4*time.Hour {
		t.Fatalf("temporary credential refresh deadline is %v away, want about 5h", delta)
	}
}

// TestAuthRefreshKeepsPermanentKeyPair verifies the refresh handler short-circuits
// for a credential with no security token instead of calling upstream.
func TestAuthRefreshKeepsPermanentKeyPair(t *testing.T) {
	storage, errMarshal := json.Marshal(map[string]any{
		storageKey: credential{
			AccessKeyID:     "AKPERM",
			SecretAccessKey: "SKPERM",
			UserName:        "tester",
			LoginType:       "AKSK",
		},
	})
	if errMarshal != nil {
		t.Fatalf("marshal storage: %v", errMarshal)
	}
	request, _ := json.Marshal(pluginapi.AuthRefreshRequest{
		AuthID:       "codearts-provider/me.json",
		AuthProvider: providerID,
		StorageJSON:  storage,
	})

	raw, errRefresh := authRefresh(request)
	if errRefresh != nil {
		t.Fatalf("authRefresh: %v", errRefresh)
	}
	var env struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
	}
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
		t.Fatalf("decode envelope: %v", errUnmarshal)
	}
	if !env.OK {
		t.Fatalf("refresh returned an error envelope: %s", raw)
	}
	var refreshed pluginapi.AuthRefreshResponse
	if errUnmarshal := json.Unmarshal(env.Result, &refreshed); errUnmarshal != nil {
		t.Fatalf("decode refresh response: %v", errUnmarshal)
	}
	cred, errCred := credentialFromStorage(refreshed.Auth.StorageJSON)
	if errCred != nil {
		t.Fatalf("credentialFromStorage: %v", errCred)
	}
	if cred == nil || cred.AccessKeyID != "AKPERM" || cred.SecretAccessKey != "SKPERM" {
		t.Fatalf("credential was not preserved: %+v", cred)
	}
	if refreshed.Auth.Metadata["login_type"] != "AKSK" {
		t.Fatalf("login_type = %v, want AKSK", refreshed.Auth.Metadata["login_type"])
	}
}

// TestStatusPageAndRegistrationAgreeOnCapabilities guards the invariant that bit
// us once: the status page used to hard-code its own capability list, which
// silently drifted from what plugin.register declared. Both must render
// declaredCapabilities().
func TestStatusPageAndRegistrationAgreeOnCapabilities(t *testing.T) {
	// What registration reports.
	regRaw, errReg := json.Marshal(registrationResponse())
	if errReg != nil {
		t.Fatalf("marshal registration: %v", errReg)
	}
	var reg struct {
		Capabilities map[string]any `json:"capabilities"`
	}
	if errUnmarshal := json.Unmarshal(regRaw, &reg); errUnmarshal != nil {
		t.Fatalf("decode registration: %v", errUnmarshal)
	}

	// What the status page reports.
	status := statusPage(config())
	var page struct {
		Capabilities map[string]any `json:"capabilities"`
	}
	if errUnmarshal := json.Unmarshal(status.Body, &page); errUnmarshal != nil {
		t.Fatalf("decode status page: %v", errUnmarshal)
	}

	if len(reg.Capabilities) == 0 {
		t.Fatal("registration declared no capabilities")
	}
	for name, declared := range reg.Capabilities {
		fromPage, present := page.Capabilities[name]
		if !present {
			t.Errorf("capability %q is declared by registration but missing from the status page", name)
			continue
		}
		if fmt.Sprint(fromPage) != fmt.Sprint(declared) {
			t.Errorf("capability %q differs: registration=%v status page=%v", name, declared, fromPage)
		}
	}
	for name := range page.Capabilities {
		if _, present := reg.Capabilities[name]; !present {
			t.Errorf("status page advertises %q which registration does not declare", name)
		}
	}

	// Every declared-true capability must have a dispatch handler behind it.
	for name, value := range reg.Capabilities {
		if enabled, isBool := value.(bool); !isBool || !enabled {
			continue
		}
		if _, known := capabilityMethods[name]; !known {
			t.Errorf("capability %q is declared true but has no known RPC method", name)
		}
	}
}

// capabilityMethods maps each capability to the RPC method that services it, so
// a capability cannot be declared without an implementation behind it.
var capabilityMethods = map[string]string{
	"model_provider":   pluginabi.MethodModelStatic,
	"auth_provider":    pluginabi.MethodAuthIdentifier,
	"executor":         pluginabi.MethodExecutorExecute,
	"management_api":   pluginabi.MethodManagementRegister,
	"quota_provider":   pluginabi.MethodQuotaIdentifier,
	"usage_plugin":     pluginabi.MethodUsageHandle,
	"thinking_applier": pluginabi.MethodThinkingIdentifier,
	"scheduler":        pluginabi.MethodSchedulerPick,
}
