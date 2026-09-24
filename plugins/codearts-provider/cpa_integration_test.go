package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// This test runs an actual, CGO-enabled CPA binary with the built DLL and a
// local upstream fixture. No real subscription or credentials are used.
// CODEARTS_CPA_EXE and CODEARTS_PLUGIN_DLL opt in to the process-level test.
func TestCPAIntegration(t *testing.T) {
	executable := os.Getenv("CODEARTS_CPA_EXE")
	dll := os.Getenv("CODEARTS_PLUGIN_DLL")
	if executable == "" || dll == "" {
		t.Skip("set CODEARTS_CPA_EXE and CODEARTS_PLUGIN_DLL for real-host integration")
	}
	var chatCalls, loginCalls, renewCalls, benefitCatalogCalls atomic.Int32
	var nextStatus atomic.Int32
	var nextStreamFault atomic.Bool
	var nextStreamMetadata atomic.Bool
	var blockBenefit atomic.Bool
	benefitEntered, benefitCancelled := make(chan struct{}, 1), make(chan struct{}, 1)
	var benefitCalls atomic.Int32
	var dailyClaimCalls atomic.Int32
	var dailyClaimFail atomic.Bool
	var welfareMu sync.Mutex
	welfareStatus := map[string]string{}
	var sessionMu sync.Mutex
	var heldChat atomic.Pointer[integrationChatGate]
	var upstreamSessionLimit atomic.Int32
	upstreamSessionLimit.Store(3)
	activeSessions := map[string]bool{}
	sessionStarts := map[string]int{}
	sessionFinishes := map[string]int{}
	agentModels := modelFixture(t, "agent-detail")
	gatewayModels := modelFixture(t, "gateway-config")
	agentList := modelFixture(t, "useragents")
	builtinModels := modelFixture(t, "builtin")
	profile, _ := json.Marshal(oauthIdentity{AccountID: "tenant", PrincipalID: "browser-id", PrincipalURN: "iam::tenant:browser-user"})
	claims, _ := json.Marshal(map[string]string{"user_profile": base64.RawURLEncoding.EncodeToString(profile)})
	refreshToken := "e30." + base64.RawURLEncoding.EncodeToString(claims) + ".signature"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/oauth2/tokens" {
			loginCalls.Add(1)
			if r.Header.Get("DPoP") == "" || r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
				http.Error(w, "missing OAuth proof", 400)
				return
			}
			_ = r.ParseForm()
			if r.Form.Get("client_id") != codeArtsOAuthClientID || r.Form.Get("code_verifier") == "" || (r.Form.Get("grant_type") != "authorization_code" && r.Form.Get("grant_type") != "refresh_token") {
				http.Error(w, "wrong OAuth exchange", 400)
				return
			}
			if r.Form.Get("grant_type") == "authorization_code" {
				redirect, parseErr := url.Parse(r.Form.Get("redirect_uri"))
				if parseErr != nil || redirect.Hostname() != "127.0.0.1" || redirect.Path != codeArtsOAuthCallback || redirect.RawQuery != "" {
					http.Error(w, "token exchange changed the original redirect URI", 400)
					return
				}
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"credentials":{"access_key_id":"login-ak","secret_access_key":"login-sk","security_token":"login-sts","expiration":"2030-01-01T00:00:00Z"},"refresh_token":%q}`, refreshToken)
			return
		}
		if !strings.HasPrefix(r.Header.Get("Authorization"), "SDK-HMAC-SHA256 Access=") {
			http.Error(w, "unsigned", 401)
			return
		}
		switch r.URL.Path {
		case "/v1/ops/delivery", epWelfareClaim, epWelfareConfirm:
			welfareMu.Lock()
			defer welfareMu.Unlock()
			account := strings.Split(r.Header.Get("Authorization"), ",")[0]
			state := welfareStatus[account]
			if state == "" {
				state = "ELIGIBLE"
			}
			if r.Header.Get("Agent-Type") != "PromptCenter" {
				http.Error(w, "wrong activity headers", 400)
				return
			}
			if r.URL.Path == "/v1/ops/delivery" {
				if r.Method != "GET" || r.URL.Query().Get("channel") != "IDE" {
					http.Error(w, "invalid delivery", 400)
					return
				}
				fmt.Fprintf(w, `{"code":0,"data":{"items":[{"campaignId":1,"type":"USER_LOGIN","benefitUnit":"CREDIT","benefitAmount":1000,"claimable":%t,"status":%q}]}}`, state == "ELIGIBLE", state)
				return
			}
			var b map[string]any
			_ = json.NewDecoder(r.Body).Decode(&b)
			if r.Method != "POST" || b["campaignId"] != float64(1) {
				http.Error(w, "incorrect campaign", 400)
				return
			}
			if r.URL.Path == epWelfareClaim {
				dailyClaimCalls.Add(1)
				if dailyClaimFail.Load() {
					http.Error(w, "claim unavailable", 503)
					return
				}
				if b["channel"] != "IDE" || b["idempotentKey"] == nil {
					http.Error(w, "incorrect claim", 400)
					return
				}
				welfareStatus[account] = "CLAIMED"
			} else {
				welfareStatus[account] = "CONFIRMED"
			}
			fmt.Fprint(w, `{"code":0,"data":{"campaignId":1}}`)
		case "/snap-manager/v1/statistics/plugin":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"metrics":[{"name":"usageTokenChatMessages","value":20,"show":true}],"package":{"spec_code":"fixture-plan"}}`)
		case epBenefitBalance:
			benefitCalls.Add(1)
			if blockBenefit.Load() {
				benefitEntered <- struct{}{}
				select {
				case <-r.Context().Done():
					benefitCancelled <- struct{}{}
				case <-time.After(10 * time.Second):
					http.Error(w, "fixture cancellation was not propagated", http.StatusGatewayTimeout)
				}
				return
			}
			fmt.Fprint(w, `{"error_code":"0000","result":{"daily_token_limit":100,"daily_tokens_used":30}}`)
		case "/snap-manager/v1/token/renew":
			renewCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"credential":{"access":"renewed-ak","secret":"renewed-sk","securitytoken":"renewed-sts","expires_at":"2030-01-01T00:00:00Z"}}`)
		case "/snap-manager/v1/chat-session/heartbeat":
			sessionID := strings.TrimSpace(r.Header.Get("user-session-id"))
			body, _ := io.ReadAll(r.Body)
			if r.Method != http.MethodPut || sessionID == "" || string(body) != "{}" {
				http.Error(w, "invalid chat session heartbeat", http.StatusBadRequest)
				return
			}
			sessionMu.Lock()
			defer sessionMu.Unlock()
			switch r.URL.Query().Get("status") {
			case "busy":
				if !activeSessions[sessionID] {
					if len(activeSessions) >= int(upstreamSessionLimit.Load()) {
						http.Error(w, "TM.00001041: fixture session limit reached", http.StatusBadRequest)
						return
					}
					activeSessions[sessionID] = true
					sessionStarts[sessionID]++
				}
			case "idle":
				if activeSessions[sessionID] {
					delete(activeSessions, sessionID)
					sessionFinishes[sessionID]++
				}
			default:
				http.Error(w, "invalid chat session status", http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"status":"ok"}`)
		case "/v1/agent-center/agents/useragents":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(agentList)
		case "/v1/agent-center/agents/detail":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(agentModels)
		case "/v1/model/builtin":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(builtinModels)
		case "/v1/benefit-gateway-config":
			benefitCatalogCalls.Add(1)
			fmt.Fprint(w, `{"enabled":true}`)
		case "/api/v1/gateway/config":
			if !strings.Contains(r.Header.Get("Authorization"), "SignedHeaders=host;x-sdk-date;x-security-token") {
				http.Error(w, "incorrect benefit gateway signature", 401)
				return
			}
			_, _ = w.Write(gatewayModels)
		case agentModePath:
			chatCalls.Add(1)
			sessionMu.Lock()
			busy := activeSessions[r.Header.Get("user-session-id")]
			sessionMu.Unlock()
			if !busy {
				http.Error(w, "chat did not use an active busy session", http.StatusBadRequest)
				return
			}
			if gate := heldChat.Load(); gate != nil {
				gate.entered <- struct{}{}
				select {
				case <-gate.release:
				case <-r.Context().Done():
					return
				}
			}
			if status := nextStatus.Load(); status != 0 {
				http.Error(w, "fixture rate limit", int(status))
				return
			}
			body, _ := io.ReadAll(r.Body)
			var request map[string]json.RawMessage
			if json.Unmarshal(body, &request) != nil {
				http.Error(w, "wrong model", 400)
				return
			}
			var requested string
			_ = json.Unmarshal(request["model"], &requested)
			if requested != "GLM-5.2" && requested != "glm-5.3-flash" {
				http.Error(w, "model alias was not resolved", 400)
				return
			}
			benefit := requested == "glm-5.3-flash"
			if (r.Header.Get("maas_type") == "benefit") != benefit || r.Header.Get("model-id") != requested || r.Header.Get("model-name") != requested {
				http.Error(w, "wrong model routing headers", 400)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			if nextStreamFault.Load() {
				if nextStreamMetadata.Load() {
					fmt.Fprint(w, "data: {\"id\":\"chatcmpl-test\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"\"},\"finish_reason\":null}]}\r\n\r\n")
					w.(http.Flusher).Flush()
				}
				// HTTP 200 with an error inside the final, unterminated SSE frame.
				fmt.Fprint(w, "data: "+quotaFaultFrame)
				return
			}
			if bytes.Contains(body, []byte("lookup")) {
				fmt.Fprint(w, "data: {\"id\":\"chatcmpl-test\",\"object\":\"chat.completion.chunk\",\"model\":\"audit-model\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"call1\",\"type\":\"function\",\"function\":{\"name\":\"lookup\",\"arguments\":\"{}\"}}]},\"finish_reason\":null}]}\r\n\r\n")
				fmt.Fprint(w, "data: {\"id\":\"chatcmpl-test\",\"object\":\"chat.completion.chunk\",\"model\":\"audit-model\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\r\n\r\n")
			} else {
				fmt.Fprint(w, "data: {\"id\":\"chatcmpl-test\",\"object\":\"chat.completion.chunk\",\"model\":\"audit-model\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hello from fixture\"},\"finish_reason\":null}]}\r\n\r\n")
				w.(http.Flusher).Flush()
				fmt.Fprint(w, "data: {\"id\":\"chatcmpl-test\",\"object\":\"chat.completion.chunk\",\"model\":\"audit-model\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\r\n\r\n")
			}
			fmt.Fprint(w, "data: {\"id\":\"chatcmpl-test\",\"object\":\"chat.completion.chunk\",\"model\":\"audit-model\",\"choices\":[],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":7,\"total_tokens\":18}}\r\n\r\ndata: [DONE]\r\n\r\n")
		default:
			http.Error(w, "unknown fixture endpoint", 404)
		}
	}))
	defer upstream.Close()
	dir := t.TempDir()
	authDir := filepath.Join(dir, "auth")
	pluginDir := filepath.Join(dir, "plugins")
	for _, d := range []string{authDir, pluginDir} {
		if err := os.MkdirAll(d, 0700); err != nil {
			t.Fatal(err)
		}
	}
	library, err := os.ReadFile(dll)
	if err != nil {
		t.Fatal(err)
	}
	libraryExt := strings.ToLower(filepath.Ext(dll))
	if libraryExt == "" {
		if runtime.GOOS == "windows" {
			libraryExt = ".dll"
		} else if runtime.GOOS == "darwin" {
			libraryExt = ".dylib"
		} else {
			libraryExt = ".so"
		}
	}
	if err = os.WriteFile(filepath.Join(pluginDir, "codearts-provider"+libraryExt), library, 0600); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	configPath := filepath.Join(dir, "config.yaml")
	const staticModelConfig = "      model_map: {audit-model: GLM-5.2}\n      models: [{id: audit-model, display_name: Audit alias}]\n"
	const benefitModelConfig = "      benefit_models:\n        - {id: deepseek-v4-flash-0731, display_name: deepseek-v4-flash-0731, context_length: 1048576, max_output_tokens: 393216}\n        - {id: deepseek-v4-pro-0813, display_name: deepseek-v4-pro-0813, context_length: 1048576, max_output_tokens: 393216}\n        - {id: glm-5.3-flash, display_name: glm-5.3-flash, context_length: 1048576, max_output_tokens: 131072}\n"
	configYAML := fmt.Sprintf("host: 127.0.0.1\nport: %d\nauth-dir: %q\napi-keys: [audit-client]\nremote-management:\n  allow-remote: false\n  secret-key: audit-admin\n  disable-control-panel: true\nrequest-retry: 0\nplugins:\n  enabled: true\n  dir: %q\n  configs:\n    codearts-provider:\n      enabled: true\n      base_url: %q\n      benefit_gateway_url: %q\n      oauth_token_url: %q\n      discover_models: true\n", port, filepath.ToSlash(authDir), filepath.ToSlash(pluginDir), upstream.URL, upstream.URL, upstream.URL+"/v1/oauth2/tokens") + benefitModelConfig + staticModelConfig
	stateDir := filepath.Join(dir, "persistent-state")
	configYAML = strings.Replace(configYAML, benefitModelConfig, "", 1) // No configured benefit fallback may mask discovery.
	configYAML += fmt.Sprintf("      state_dir: %q\n", filepath.ToSlash(stateDir))
	if err = os.WriteFile(configPath, []byte(configYAML), 0600); err != nil {
		t.Fatal(err)
	}
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	client := &http.Client{Timeout: 15 * time.Second}
	assertSessionsIdle := func() {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			sessionMu.Lock()
			remaining := len(activeSessions)
			sessionMu.Unlock()
			if remaining == 0 {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatal("chat terminated without releasing its busy session")
	}
	request := func(method, path, body string) (int, []byte) {
		t.Helper()
		req, err := http.NewRequest(method, base+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		key := "audit-client"
		if strings.HasPrefix(path, "/v0/management/") {
			key = "audit-admin"
		}
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		if path == "/v1/chat/completions" || path == "/v1/messages" {
			assertSessionsIdle()
		}
		return resp.StatusCode, b
	}
	var process *exec.Cmd
	stop := func() {
		if process != nil {
			process.Process.Kill()
			process.Wait()
			process = nil
		}
	}
	defer stop()
	start := func() {
		t.Helper()
		logPath := filepath.Join(dir, "cpa.log")
		var logOffset int64
		if info, err := os.Stat(logPath); err == nil {
			logOffset = info.Size()
		}
		process = exec.Command(executable, "-config", configPath)
		process.Dir = dir
		log, err := os.OpenFile(filepath.Join(dir, "cpa.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal(err)
		}
		process.Stdout = log
		process.Stderr = log
		if err = process.Start(); err != nil {
			log.Close()
			t.Fatal(err)
		}
		log.Close()
		deadline := time.Now().Add(25 * time.Second)
		for time.Now().Before(deadline) {
			resp, err := client.Get(base + "/")
			if err == nil {
				resp.Body.Close()
				// The listener becomes reachable before host account bootstrap is
				// finished. Importing then races its stale initial file snapshot.
				logs, _ := os.ReadFile(logPath)
				if int64(len(logs)) >= logOffset && bytes.Contains(logs[logOffset:], []byte("file watcher started")) {
					return
				}
			}
			time.Sleep(100 * time.Millisecond)
		}
		logBody, _ := os.ReadFile(filepath.Join(dir, "cpa.log"))
		t.Fatalf("CPA did not start: %s", logBody)
	}
	defer func() {
		if t.Failed() {
			stop()
			logBody, _ := os.ReadFile(filepath.Join(dir, "cpa.log"))
			t.Logf("CPA log:\n%s", logBody)
		}
	}()
	start()
	status, body := request("GET", "/v0/management/plugins", "")
	if status != 200 || !bytes.Contains(body, []byte(`"registered":true`)) || !bytes.Contains(body, []byte(`"effective_enabled":true`)) {
		t.Fatalf("plugin not enabled: %d %s", status, body)
	}
	t.Log("actual CPA loaded and enabled the DLL")
	status, body = request("POST", "/v0/management/codearts-provider/import", `{"access_key_id":"import-ak","secret_access_key":"import-sk","security_token":"import-sts","expires_at":"2000-01-01T00:00:00Z","user_name":"imported","name":"codearts-provider-integration.json"}`)
	if status != 200 {
		t.Fatalf("import failed: %d %s", status, body)
	}
	assertModels := func(expectedIDs ...string) {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			status, body = request("GET", "/v1/models", "")
			complete := status == http.StatusOK
			for _, modelID := range expectedIDs {
				complete = complete && bytes.Contains(body, []byte(`"`+modelID+`"`))
			}
			if complete {
				return
			}
			time.Sleep(200 * time.Millisecond)
		}
		_, accountsBody := request("GET", "/v0/management/codearts-provider/accounts", "")
		var accounts struct {
			Accounts []struct {
				AuthIndex string `json:"auth_index"`
			} `json:"accounts"`
		}
		if json.Unmarshal(accountsBody, &accounts) == nil {
			for _, account := range accounts.Accounts {
				catalogStatus, catalogBody := request("GET", "/v0/management/codearts-provider/models?auth_index="+url.QueryEscape(account.AuthIndex), "")
				var catalog modelCatalogResult
				if json.Unmarshal(catalogBody, &catalog) == nil {
					t.Logf("account catalog diagnostic: status=%d source=%s warnings=%v", catalogStatus, catalog.Source, catalog.Warnings)
				}
			}
		}
		t.Fatalf("discovered model missing: %d %s", status, body)
	}
	// Only the alias is static. GLM-5.2 and the benefit ID require account
	// refresh plus live regional and benefit discovery.
	assertModels("audit-model", "glm-5.3-flash", "GLM-5.2")
	if renewCalls.Load() != 1 {
		t.Fatalf("expired credential was renewed %d times, want exactly once before model discovery", renewCalls.Load())
	}
	t.Log("expired stored credential was silently renewed and persisted before model discovery")
	capturedModelIDs := []string{"GLM-5.2", "glm-5.2-sft-harmony", "openpangu-2.0-pro", "openpangu-2.0-flash", "Qwen3-VL-235B", "kimi-k2.6-vl", "deepseek-v4-flash-0731", "deepseek-v4-pro-0813", "glm-5.3-flash"}
	for _, modelID := range capturedModelIDs {
		if !bytes.Contains(body, []byte(`"`+modelID+`"`)) {
			t.Fatalf("captured model %s is missing from CPA catalog: %s", modelID, body)
		}
	}
	if bytes.Contains(body, []byte("PanguDev_COM_QC2")) {
		t.Fatal("CPA advertised the obsolete default model")
	}
	t.Log("imported nested credential and discovered account models")
	status, body = request("POST", "/v1/chat/completions", `{"model":"glm-5.3-flash","messages":[{"role":"user","content":"hello"}]}`)
	if status != 200 || !bytes.Contains(body, []byte("hello from fixture")) {
		t.Fatalf("benefit model routing failed: %d %s", status, body)
	}
	t.Log("four Agent, two built-in and three discovered benefit models appeared; benefit routing passed through CPA")
	status, body = request("GET", "/v0/management/codearts-provider/accounts", "")
	var accountList struct {
		Accounts []struct {
			AuthIndex string `json:"auth_index"`
		} `json:"accounts"`
	}
	if json.Unmarshal(body, &accountList) != nil || status != 200 || len(accountList.Accounts) != 1 {
		t.Fatalf("account inventory failed: %d %s", status, body)
	}
	testCPAConcurrency(t, base, 3, &heldChat, &chatCalls)
	status, body = request("POST", "/v0/management/codearts-provider/concurrency", fmt.Sprintf(`{"auth_index":%q,"limit":5}`, accountList.Accounts[0].AuthIndex))
	if status != 200 || !bytes.Contains(body, []byte(`"persistent":true`)) {
		t.Fatalf("concurrency save failed: %d %s", status, body)
	}
	stop()
	start()
	assertModels("audit-model", "glm-5.3-flash", "GLM-5.2")
	status, body = request("GET", "/v0/management/codearts-provider/accounts", "")
	if status != 200 || !bytes.Contains(body, []byte(`"override":5`)) {
		t.Fatalf("restart lost concurrency override: %d %s", status, body)
	}
	upstreamSessionLimit.Store(5)
	testCPAConcurrency(t, base, 5, &heldChat, &chatCalls)
	status, body = request("POST", "/v0/management/codearts-provider/concurrency", fmt.Sprintf(`{"auth_index":%q,"limit":0}`, accountList.Accounts[0].AuthIndex))
	if status != 200 {
		t.Fatalf("concurrency reset failed: %d %s", status, body)
	}
	upstreamSessionLimit.Store(3)
	assertSessionsIdle()
	t.Log("per-account concurrency: 3 and 5 enforced, persisted across restart, no busy cooldown or leaked sessions")
	benefitPath := "/v0/management/codearts-provider/benefit-balance?auth_index=" + url.QueryEscape(accountList.Accounts[0].AuthIndex)
	status, body = request("GET", benefitPath, "")
	if status != http.StatusOK || !bytes.Contains(body, []byte(`"daily_tokens_used":30`)) {
		t.Fatalf("independent benefit lookup failed: %d %s", status, body)
	}
	blockBenefit.Store(true)
	ctxBenefit, cancelBenefit := context.WithCancel(context.Background())
	defer cancelBenefit()
	benefitReq, _ := http.NewRequestWithContext(ctxBenefit, http.MethodGet, base+benefitPath, nil)
	benefitReq.Header.Set("Authorization", "Bearer audit-admin")
	benefitDone := make(chan error, 1)
	go func() {
		response, err := client.Do(benefitReq)
		if response != nil {
			response.Body.Close()
		}
		benefitDone <- err
	}()
	select {
	case <-benefitEntered:
	case <-time.After(3 * time.Second):
		t.Fatal("benefit request did not reach fixture")
	}
	// Keep the optional gateway blocked while the normal quota path completes.
	quotaStarted := time.Now()
	status, body = request("POST", "/v0/management/codearts-provider/quota/refresh", `{}`)
	if status != http.StatusOK || !bytes.Contains(body, []byte(`"refreshed":1`)) || time.Since(quotaStarted) > 2*time.Second {
		t.Fatalf("optional request blocked quota refresh: %d %s", status, body)
	}
	cancelBenefit()
	select {
	case <-benefitCancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("browser cancellation did not stop optional upstream request")
	}
	if err := <-benefitDone; err == nil {
		t.Fatal("cancelled benefit request succeeded")
	}
	blockBenefit.Store(false)
	if benefitCalls.Load() != 2 {
		t.Fatalf("quota refresh unexpectedly fetched benefits: %d calls", benefitCalls.Load())
	}
	t.Log("independent benefit refresh is cancellable through CPA and cannot block package refresh")
	status, body = request("GET", "/v0/management/codearts-provider/models?auth_index="+url.QueryEscape(accountList.Accounts[0].AuthIndex), "")
	var visibleCatalog modelCatalogResult
	if json.Unmarshal(body, &visibleCatalog) != nil || status != 200 || len(visibleCatalog.Models) != 10 || visibleCatalog.Source != "discovered" || len(visibleCatalog.Warnings) != 0 {
		t.Fatalf("panel account catalog differs from registered models: %d %s", status, body)
	}
	for _, secret := range []string{"import-ak", "import-sk", "import-sts", "access_key_id", "oauth_context"} {
		if bytes.Contains(body, []byte(secret)) {
			t.Fatal("model diagnostics exposed credential material")
		}
	}
	for _, stream := range []bool{false, true} {
		status, body = request("POST", "/v1/chat/completions", fmt.Sprintf(`{"model":"audit-model","messages":[{"role":"user","content":"hello"}],"stream":%t,"stream_options":{"include_usage":true}}`, stream))
		if status != 200 || !bytes.Contains(body, []byte("hello from fixture")) || !bytes.Contains(body, []byte(`"total_tokens":18`)) {
			t.Fatalf("chat stream=%v failed: %d %s", stream, status, body)
		}
		if stream && bytes.Count(body, []byte("[DONE]")) != 1 {
			t.Fatalf("incorrect stream termination: %s", body)
		}
	}
	status, body = request("POST", "/v1/chat/completions", `{"model":"audit-model","messages":[{"role":"user","content":"lookup"}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}]}`)
	if status != 200 || !bytes.Contains(body, []byte(`"tool_calls"`)) || !bytes.Contains(body, []byte(`"call1"`)) {
		t.Fatalf("tool call failed: %d %s", status, body)
	}
	t.Log("streaming, non-streaming, usage and tool calls passed through CPA")
	for _, route := range []string{"/v1/messages", "/v1/responses"} {
		for _, stream := range []bool{false, true} {
			payload := fmt.Sprintf(`{"model":"audit-model","max_tokens":64,"messages":[{"role":"user","content":"hello"}],"stream":%t}`, stream)
			if route == "/v1/responses" {
				payload = fmt.Sprintf(`{"model":"audit-model","input":"hello","stream":%t}`, stream)
			}
			status, body = request("POST", route, payload)
			if status != 200 || !bytes.Contains(body, []byte("hello from fixture")) {
				t.Fatalf("CPA format conversion %s stream=%v failed: %d %s", route, stream, status, body)
			}
		}
	}
	t.Log("CPA translated Anthropic Messages and OpenAI Responses in both modes")
	// The Anthropic route must receive complete Anthropic events, not bare JSON
	// chunks: CPA writes plugin frames for this protocol unchanged, so a missing
	// event frame silently truncates the client stream.
	status, body = request("POST", "/v1/messages", `{"model":"audit-model","max_tokens":64,"messages":[{"role":"user","content":"hello"}],"stream":true}`)
	if status != 200 {
		t.Fatalf("anthropic stream failed: %d %s", status, body)
	}
	for _, want := range []string{
		"event: message_start",
		"event: content_block_start",
		`"type":"text_delta"`,
		"event: content_block_stop",
		"event: message_delta",
		`"stop_reason":"end_turn"`,
		"event: message_stop",
	} {
		if !bytes.Contains(body, []byte(want)) {
			t.Fatalf("anthropic stream missing %q: %s", want, body)
		}
	}
	if bytes.Count(body, []byte("event: message_start")) != 1 || bytes.Count(body, []byte("event: message_stop")) != 1 {
		t.Fatalf("anthropic stream duplicated its envelope: %s", body)
	}
	status, body = request("POST", "/v1/messages", `{"model":"audit-model","max_tokens":64,"messages":[{"role":"user","content":"hello"}]}`)
	if status != 200 || !bytes.Contains(body, []byte(`"type":"message"`)) || !bytes.Contains(body, []byte(`"stop_reason":"end_turn"`)) {
		t.Fatalf("anthropic non-stream envelope is not Anthropic shaped: %d %s", status, body)
	}
	// Tool calls must survive the Anthropic route as tool_use blocks.
	status, body = request("POST", "/v1/messages", `{"model":"audit-model","max_tokens":64,"messages":[{"role":"user","content":"lookup"}],"tools":[{"name":"lookup","description":"fixture tool","input_schema":{"type":"object"}}],"stream":true}`)
	if status != 200 || !bytes.Contains(body, []byte(`"type":"tool_use"`)) || !bytes.Contains(body, []byte(`"partial_json"`)) || !bytes.Contains(body, []byte(`"stop_reason":"tool_use"`)) {
		t.Fatalf("anthropic tool stream failed: %d %s", status, body)
	}
	beforeCountCalls := chatCalls.Load()
	countThroughCPA := func(payload string) int {
		t.Helper()
		countStatus, countBody := request("POST", "/v1/messages/count_tokens", payload)
		var result struct {
			InputTokens int  `json:"input_tokens"`
			Estimated   bool `json:"estimated"`
		}
		if countStatus != 200 || json.Unmarshal(countBody, &result) != nil || result.InputTokens <= 0 || !result.Estimated {
			t.Fatalf("anthropic count_tokens lost its estimate: %d %s", countStatus, countBody)
		}
		return result.InputTokens
	}
	baseCount := countThroughCPA(`{"model":"audit-model","messages":[{"role":"user","content":"hello"}]}`)
	if actual := countThroughCPA(`{"model":"audit-model","max_tokens":100000,"temperature":0.2,"metadata":{"user_id":"` + strings.Repeat("tracking", 200) + `"},"messages":[{"role":"user","content":"hello"}]}`); actual != baseCount {
		t.Fatalf("CPA counted transport metadata or output options: got %d want %d", actual, baseCount)
	}
	if actual := countThroughCPA(`{"model":"audit-model","messages":[{"role":"user","content":"` + strings.Repeat("中文", 200) + `"}]}`); actual <= baseCount {
		t.Fatal("CPA count did not grow with actual input")
	}
	if chatCalls.Load() != beforeCountCalls {
		t.Fatal("token counting sent a generation request upstream")
	}
	t.Log("Anthropic streams carry the full event sequence, tool blocks and count_tokens")
	stop()
	start()
	assertModels("audit-model", "glm-5.3-flash")
	status, body = request("POST", "/v1/chat/completions", `{"model":"audit-model","messages":[{"role":"user","content":"after restart"}]}`)
	if status != 200 || !bytes.Contains(body, []byte("hello from fixture")) {
		t.Fatalf("restart lost credential: %d %s", status, body)
	}
	t.Log("saved account remains callable after CPA restart")
	status, body = request("GET", "/v0/management/codearts-provider-auth-url", "")
	if status != 200 {
		t.Fatalf("login start failed: %d %s", status, body)
	}
	var login struct {
		URL   string `json:"url"`
		State string `json:"state"`
	}
	json.Unmarshal(body, &login)
	loginURL, err := url.Parse(login.URL)
	if err != nil {
		t.Fatal(err)
	}
	callback, err := url.Parse(loginURL.Query().Get("auth_callback_url"))
	if err != nil {
		t.Fatal(err)
	}
	query := callback.Query()
	if callback.RawQuery != "" {
		t.Fatal("redirect URI must not embed the CPA state")
	}
	// Huawei generates an independent state; browsers may spell 127.0.0.1 as
	// localhost. Neither must change the CPA session or original token redirect.
	callback.Host = "localhost:" + callback.Port()
	query.Set("code", "browser-authorization-code")
	query.Set("state", "huawei-generated-state")
	callback.RawQuery = query.Encode()
	callbackBody, _ := json.Marshal(map[string]string{"provider": providerID, "redirect_url": callback.String()})
	status, body = request("POST", "/v0/management/oauth-callback", string(callbackBody))
	if status != http.StatusNotFound || !bytes.Contains(body, []byte("unknown or expired state")) {
		t.Fatalf("generic callback should reject Huawei's unrelated state: %d %s", status, body)
	}
	callbackBody, _ = json.Marshal(map[string]string{"state": login.State, "callback_url": callback.String()})
	status, body = request("POST", "/v0/management/codearts-provider/login/callback", string(callbackBody))
	if status != 200 {
		t.Fatalf("callback failed: %d %s", status, body)
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		status, body = request("GET", "/v0/management/get-auth-status?state="+url.QueryEscape(login.State), "")
		if bytes.Contains(body, []byte(`"status":"ok"`)) {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if status != 200 || !bytes.Contains(body, []byte(`"status":"ok"`)) || loginCalls.Load() == 0 {
		t.Fatalf("login did not complete: %d %s", status, body)
	}
	status, body = request("GET", "/v0/management/codearts-provider/accounts", "")
	if status != 200 || !bytes.Contains(body, []byte("browser-user")) {
		t.Fatalf("CPA did not persist browser account: %d %s", status, body)
	}
	t.Log("CPA browser login/poll persisted the subscription credential")

	// Second flow: a local browser reaches the plugin's own OAuth callback
	// listener directly instead of copying the URL through the management panel.
	status, body = request("GET", "/v0/management/codearts-provider-auth-url", "")
	if status != 200 {
		t.Fatalf("second login start failed: %d %s", status, body)
	}
	var secondLogin struct {
		URL   string `json:"url"`
		State string `json:"state"`
	}
	json.Unmarshal(body, &secondLogin)
	secondURL, err := url.Parse(secondLogin.URL)
	if err != nil {
		t.Fatal(err)
	}
	secondCallback, err := url.Parse(secondURL.Query().Get("auth_callback_url"))
	if err != nil {
		t.Fatal(err)
	}
	// The pending flow must explain itself rather than stay silent.
	status, body = request("GET", "/v0/management/codearts-provider/login/status?state="+url.QueryEscape(secondLogin.State), "")
	if status != 200 || !bytes.Contains(body, []byte("浏览器")) {
		t.Fatalf("pending login did not report its stage: %d %s", status, body)
	}
	// Snapshot the exchange counter before triggering the one-time code.
	beforeCalls := loginCalls.Load()
	secondQuery := secondCallback.Query()
	secondQuery.Set("code", "second-authorization-code")
	secondQuery.Set("state", "second-huawei-state")
	secondCallback.RawQuery = secondQuery.Encode()
	callbackResp, err := client.Get(secondCallback.String())
	if err != nil {
		t.Fatalf("browser-style callback could not reach the plugin listener: %v", err)
	}
	callbackPayload, _ := io.ReadAll(callbackResp.Body)
	callbackResp.Body.Close()
	if callbackResp.StatusCode != 200 || !bytes.Contains(callbackPayload, []byte("授权信息已收到")) {
		t.Fatalf("plugin rejected the OAuth browser callback: %d %s", callbackResp.StatusCode, callbackPayload)
	}
	deadline = time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		status, body = request("GET", "/v0/management/get-auth-status?state="+url.QueryEscape(secondLogin.State), "")
		if bytes.Contains(body, []byte(`"status":"ok"`)) {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if status != 200 || !bytes.Contains(body, []byte(`"status":"ok"`)) || loginCalls.Load() == beforeCalls {
		t.Fatalf("direct OAuth browser login did not complete: %d %s", status, body)
	}
	status, body = request("GET", "/v0/management/codearts-provider/accounts", "")
	if status != 200 || !bytes.Contains(body, []byte(`"login_type":"WEB"`)) {
		t.Fatalf("browser login was not persisted as a WEB credential: %d %s", status, body)
	}
	t.Log("OAuth browser callback completed the login through the plugin listener")

	// The dashboard is the operator-facing surface: it must be the Chinese,
	// single-authorization page the extension's own flow implies, and it must not
	// grow a second sign-in path.
	status, body = request("GET", "/v0/resource/plugins/codearts-provider/panel", "")
	if status != 200 {
		t.Fatalf("panel resource unavailable: %d", status)
	}
	for _, want := range []string{`lang="zh-CN"`, "charset=\"utf-8\"", "开始授权", "提交回调地址，完成授权", "账号", "定时任务"} {
		if !bytes.Contains(body, []byte(want)) {
			t.Fatalf("panel is missing %q", want)
		}
	}
	for _, unwanted := range []string{"Sign in to Huawei Cloud", "Import credential", "Access key ID", "Scheduled tasks"} {
		if bytes.Contains(body, []byte(unwanted)) {
			t.Fatalf("panel still exposes the old UI string %q", unwanted)
		}
	}
	t.Log("panel serves the Chinese single-authorization dashboard")
	for _, want := range []string{"领取每日活动积分", "定时任务总开关", "data-task-toggle"} {
		if !bytes.Contains(body, []byte(want)) {
			t.Fatalf("panel lacks %s", want)
		}
	}

	nextStatus.Store(429)
	status, body = request("POST", "/v1/chat/completions", `{"model":"audit-model","messages":[{"role":"user","content":"rate limit"}],"stream":true}`)
	if status != 429 {
		t.Fatalf("CPA lost upstream 429: %d %s", status, body)
	}
	if !bytes.Contains(body, []byte("fixture rate limit")) {
		t.Fatalf("CPA lost the upstream error reason: %s", body)
	}
	t.Log("CPA preserved upstream rate-limit status for streaming clients")
	if chatCalls.Load() < 4 {
		t.Fatal("chat requests did not reach the upstream")
	}

	// An empty static model list is the shipped default. Restart the actual
	// host without the test alias so static registration cannot mask failures
	// to register or execute models discovered from saved accounts.
	stop()
	configYAML = strings.Replace(configYAML, staticModelConfig, "", 1)
	if err = os.WriteFile(configPath, []byte(configYAML), 0600); err != nil {
		t.Fatal(err)
	}
	nextStatus.Store(0)
	start()
	assertModels(capturedModelIDs...)
	for _, unexpected := range []string{"audit-model", "PanguDev_COM_QC2"} {
		if bytes.Contains(body, []byte(`"`+unexpected+`"`)) {
			t.Fatalf("empty static configuration advertised %s: %s", unexpected, body)
		}
	}
	for _, modelID := range []string{"GLM-5.2", "glm-5.3-flash", "GLM-5.2", "glm-5.3-flash"} {
		status, body = request("POST", "/v1/chat/completions", fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"without static aliases"}]}`, modelID))
		if status != http.StatusOK || !bytes.Contains(body, []byte("hello from fixture")) {
			t.Fatalf("discovered model %s was not callable without static configuration: %d %s", modelID, status, body)
		}
	}
	for _, stream := range []bool{true, false} {
		// Reset host cooldown state between failure cases.
		stop()
		start()
		assertModels(capturedModelIDs...)
		nextStreamFault.Store(true)
		nextStreamMetadata.Store(stream)
		status, body = request("POST", "/v1/chat/completions", fmt.Sprintf(`{"model":"GLM-5.2","messages":[{"role":"user","content":"quota error"}],"stream":%t}`, stream))
		nextStreamFault.Store(false)
		nextStreamMetadata.Store(false)
		if !bytes.Contains(body, []byte("insufficient quota")) || bytes.Contains(body, []byte(`"finish_reason":"stop"`)) {
			t.Fatalf("in-stream quota error became empty success (stream=%t): %d %s", stream, status, body)
		}
		if status != http.StatusForbidden {
			t.Fatalf("quota status lost after pre-answer metadata (stream=%t): %d %s", stream, status, body)
		}
		assertSessionsIdle()
	}
	t.Log("unterminated in-stream quota faults reach clients instead of empty success")

	// Test management switches in the actual ABI host, including auth loading
	// after registration. Do not rely on the host's config watcher to persist UI.
	const scheduleBase = "/v0/management/codearts-provider"
	status, body = request("POST", scheduleBase+"/schedule/config", `{"enabled":false,"tasks":[{"id":"token-renew","enabled":false},{"id":"daily-benefit-claim","enabled":true}]}`)
	if status != 200 || !bytes.Contains(body, []byte(`"persistent":true`)) {
		t.Fatalf("switch save failed: %d %s", status, body)
	}
	stop()
	start()
	deadline = time.Now().Add(12 * time.Second)
	var schedule struct {
		Enabled bool `json:"enabled"`
		Pending bool `json:"pending"`
		Tasks   []struct {
			ID      string `json:"id"`
			Enabled bool   `json:"enabled"`
			Running bool   `json:"running"`
			Count   int    `json:"run_count"`
			Error   string `json:"last_error"`
			Result  string `json:"last_result"`
		} `json:"tasks"`
	}
	for time.Now().Before(deadline) {
		status, body = request("GET", scheduleBase+"/schedule", "")
		schedule.Tasks = nil
		if json.Unmarshal(body, &schedule) == nil && !schedule.Pending {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if status != 200 || schedule.Pending || schedule.Enabled {
		t.Fatalf("restart lost total switch: %s", body)
	}
	for _, task := range schedule.Tasks {
		if task.ID == "token-renew" && task.Enabled {
			t.Fatal("restart lost individual switch")
		}
		if task.ID == dailyClaimTaskID && !task.Enabled {
			t.Fatal("restart lost daily opt-in")
		}
	}
	if dailyClaimCalls.Load() != 0 {
		t.Fatal("disabled scheduler claimed automatically")
	}

	// A failed claim must not block an ordinary chat, and its task result must
	// be reported as failure. All credentials and upstreams are test fixtures.
	dailyClaimFail.Store(true)
	status, body = request("POST", scheduleBase+"/checkin", `{"task":"daily-benefit-claim"}`)
	if status != 200 {
		t.Fatalf("manual trigger failed: %s", body)
	}
	waitClaim := func(minCount int) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			_, payload := request("GET", scheduleBase+"/schedule", "")
			schedule.Tasks = nil // omitted fields must not reuse an earlier failure.
			_ = json.Unmarshal(payload, &schedule)
			for _, task := range schedule.Tasks {
				if task.ID == dailyClaimTaskID && task.Count >= minCount && !task.Running {
					return
				}
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatal("claim did not complete")
	}
	waitClaim(1)
	for _, task := range schedule.Tasks {
		if task.ID == dailyClaimTaskID && task.Error == "" {
			t.Fatal("failed claim was reported successful")
		}
	}
	status, body = request("POST", "/v1/chat/completions", `{"model":"glm-5.3-flash","messages":[{"role":"user","content":"claim failure isolation"}]}`)
	if status != 200 || !bytes.Contains(body, []byte("hello from fixture")) {
		t.Fatalf("claim failure affected chat: %d %s", status, body)
	}
	dailyClaimFail.Store(false)
	// Age ONLY fixture attempt records to make a retry eligible without a real
	// ten-minute delay. Production code never exposes a force/retry bypass.
	statePaths, _ := filepath.Glob(filepath.Join(stateDir, "daily-welfare-*.state"))
	if len(statePaths) == 0 {
		t.Fatal("claim attempt was not persisted")
	}
	for _, path := range statePaths {
		var state dailyClaimState
		if err := readPluginState(path, &state); err != nil {
			t.Fatal(err)
		}
		state.LastAttempt = time.Now().Add(-11 * time.Minute)
		if err := savePluginState(path, state); err != nil {
			t.Fatal(err)
		}
	}
	_, _ = request("POST", scheduleBase+"/schedule/config", `{"tasks":[{"id":"daily-benefit-claim","enabled":false}]}`)
	_, _ = request("POST", scheduleBase+"/checkin", `{"task":"daily-benefit-claim"}`)
	waitClaim(2)
	for _, task := range schedule.Tasks {
		if task.ID == dailyClaimTaskID && (task.Error != "" || !strings.Contains(task.Result, "官方确认")) {
			t.Fatalf("manual claim with both flags off failed: %+v", task)
		}
	}
	claimed := dailyClaimCalls.Load()
	_, _ = request("POST", scheduleBase+"/schedule/config", `{"enabled":true,"tasks":[{"id":"daily-benefit-claim","enabled":true}]}`)
	waitClaim(3) // startup catch-up, but today's successes are already on disk.
	stop()
	start()
	waitClaim(1)
	if dailyClaimCalls.Load() != claimed {
		t.Fatal("restart or catch-up repeated today's accepted claim")
	}
	t.Log("daily claim exact protocol, failure isolation, manual trigger, persistent switches and restart dedup passed")
	sessionMu.Lock()
	defer sessionMu.Unlock()
	for sessionID, starts := range sessionStarts {
		if sessionFinishes[sessionID] != starts {
			t.Fatalf("chat session lifecycle is unbalanced: busy=%d idle=%d", starts, sessionFinishes[sessionID])
		}
	}
	if benefitCatalogCalls.Load() == 0 {
		t.Fatal("CPA never discovered benefit models without a configured fallback")
	}
	t.Log("empty static configuration registered Agent, built-in and discovered benefit models and executed both routes")
}
