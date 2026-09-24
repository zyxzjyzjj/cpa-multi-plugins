package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// managementBasePath must match the prefix the host reserves for plugin
// Management API routes. Paths returned from management.register are joined onto
// this prefix, so they are declared relative to it.
const managementBasePath = "/v0/management"

// managementRoutes returns the plugin-owned Management API surface.
//
// Routes under /v0/management/ are admin-key authenticated and may therefore
// touch credentials. The browser panel itself is served from the
// unauthenticated resource namespace and calls these routes with the admin key
// the operator has already stored in the management UI.
func managementRoutes() []map[string]any {
	return []map[string]any{
		{"Method": http.MethodPost, "Path": "/codearts-provider/login/callback", "Description": "Submit the localhost OAuth callback URL when CPA runs on a different machine."},
		{"Method": http.MethodGet, "Path": "/codearts-provider/login/status", "Description": "Report the stage of one OAuth sign-in flow (waiting for the callback, or exchanging the code)."},
		{"Method": http.MethodPost, "Path": "/codearts-provider/concurrency", "Description": "Persist an account's local concurrent chat limit (body: {auth_index,limit}; 0 inherits the configured default)."},
		{
			"Method":      http.MethodGet,
			"Path":        "/codearts-provider/accounts",
			"Description": "List CodeArts Doer accounts with subscription, quota and credential expiry.",
		},
		{
			"Method":      http.MethodGet,
			"Path":        "/codearts-provider/models",
			"Description": "Read the account model catalog (query: auth_index; add include_benefit=true for a synchronous live benefit diagnostic).",
		},
		{
			"Method":      http.MethodPost,
			"Path":        "/codearts-provider/quota/refresh",
			"Description": "Refresh the cached quota snapshot for one or all accounts (body: {auth_index}).",
		},
		{
			"Method":      http.MethodGet,
			"Path":        "/codearts-provider/quota",
			"Description": "Return the normalised quota/subscription view for one or all accounts.",
		},
		{
			"Method":      http.MethodGet,
			"Path":        "/codearts-provider/benefit-balance",
			"Description": "Refresh one account's optional benefit allowance independently (query: auth_index). Caller cancellation stops the upstream request.",
		},
		{
			"Method":      http.MethodGet,
			"Path":        "/codearts-provider/schedule",
			"Description": "List configured cron tasks with next/last run state.",
		},
		{
			"Method":      http.MethodPost,
			"Path":        "/codearts-provider/schedule/run",
			"Description": "Run one scheduled task immediately (body: {task}).",
		},
		{
			"Method":      http.MethodPost,
			"Path":        "/codearts-provider/checkin",
			"Description": "Claim the daily benefit now (runs the checkin task). Optional body: {task} to pick a specific checkin task.",
		},
		{
			"Method":      http.MethodGet,
			"Path":        "/codearts-provider/usage",
			"Description": "Token usage rollup from host usage records: totals, per-model, per-account and recent requests.",
		},
		{
			"Method":      http.MethodGet,
			"Path":        "/codearts-provider/benefits",
			"Description": "Explain the daily benefit/check-in situation: whether it is configured, the claim URL, and the last outcome.",
		},
		{
			"Method":      http.MethodPost,
			"Path":        "/codearts-provider/schedule/config",
			"Description": "Enable or disable the scheduler and toggle individual tasks (body: {enabled, tasks:[{id,enabled}]}).",
		},
		{
			"Method":      http.MethodPost,
			"Path":        "/codearts-provider/import",
			"Description": "Import a CodeArts Doer credential JSON (body: {access_key_id, secret_access_key, security_token, domain_id, user_name, name}).",
		},
		{
			"Method":      http.MethodGet,
			"Path":        "/codearts-provider/export",
			"Description": "Export all CodeArts Doer credentials as a re-importable JSON document.",
		},
		{
			"Method":      http.MethodPost,
			"Path":        "/codearts-provider/delete",
			"Description": "Delete one CodeArts Doer account auth file (body: {auth_index}).",
		},
	}
}

// managementRegistration declares the plugin's routes and browser resources.
//
// The result is returned as an explicit lowercase map because the host decodes
// it into its own rpcManagementRegistrationResponse, whose fields are tagged
// `routes` and `resources`. Marshalling pluginapi.ManagementRegistrationResponse
// directly would emit "Routes"/"Resources" and be silently ignored.
func managementRegistration() map[string]any {
	return map[string]any{
		"routes": managementRoutes(),
		"resources": []map[string]any{
			{
				"Path":        "/panel",
				"Menu":        "CodeArts",
				"Description": "CodeArts Doer dashboard: subscription, quota, accounts and scheduled tasks.",
			},
			{
				"Path":        "/status",
				"Menu":        "",
				"Description": "Machine-readable provider status as JSON.",
			},
		},
	}
}

// managementHandle dispatches one management or resource request.
//
// req.Path is the full path the host received, so routes are matched on their
// suffix after the plugin's own namespace.
func managementHandle(request []byte) ([]byte, error) {
	var req struct {
		pluginapi.ManagementRequest
		HostCallbackID string `json:"host_callback_id"`
	}
	if len(request) > 0 {
		if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
			return nil, fmt.Errorf("decode management request: %w", errUnmarshal)
		}
	}
	route := managementRouteSuffix(req.Path)
	method := strings.ToUpper(strings.TrimSpace(req.Method))
	if method == "" {
		method = http.MethodGet
	}

	switch {
	case route == "/" || route == "/status":
		return okEnvelope(statusPage(config()))
	case route == "/panel":
		return okEnvelope(htmlResponse(panelHTML()))
	case route == "/accounts" && method == http.MethodGet:
		return okEnvelope(handleAccounts())
	case route == "/concurrency" && method == http.MethodPost:
		return okEnvelope(handleSessionConcurrency(req.ManagementRequest))
	case route == "/models" && method == http.MethodGet:
		return okEnvelope(handleAccountModels(req.Query, req.HostCallbackID))
	case route == "/login/callback" && method == http.MethodPost:
		return okEnvelope(handleLoginCallback(req.ManagementRequest))
	case route == "/login/status" && method == http.MethodGet:
		return okEnvelope(handleLoginStatus(req.Query))
	case route == "/quota" && method == http.MethodGet:
		return okEnvelope(handleQuotaGet(req.Query))
	case route == "/quota/refresh" && method == http.MethodPost:
		return okEnvelope(handleQuotaRefresh(req.ManagementRequest, req.HostCallbackID))
	case route == "/benefit-balance" && method == http.MethodGet:
		return okEnvelope(handleBenefitBalance(req.Query, req.HostCallbackID))
	case route == "/schedule" && method == http.MethodGet:
		return okEnvelope(handleScheduleGet())
	case route == "/schedule/run" && method == http.MethodPost:
		return okEnvelope(handleScheduleRun(req.ManagementRequest))
	case route == "/checkin" && method == http.MethodPost:
		return okEnvelope(handleCheckin(req.ManagementRequest))
	case route == "/benefits" && method == http.MethodGet:
		return okEnvelope(handleBenefits())
	case route == "/usage" && method == http.MethodGet:
		return okEnvelope(handleUsage())
	case route == "/schedule/config" && method == http.MethodPost:
		return okEnvelope(handleScheduleConfig(req.ManagementRequest))
	case route == "/import" && method == http.MethodPost:
		return okEnvelope(handleImport(req.ManagementRequest))
	case route == "/export" && method == http.MethodGet:
		return okEnvelope(handleExport())
	case route == "/delete" && method == http.MethodPost:
		return okEnvelope(handleDelete(req.ManagementRequest))
	}

	body, _ := json.Marshal(map[string]any{
		"error":  "unknown management path",
		"path":   req.Path,
		"method": method,
	})
	return okEnvelope(jsonResponse(http.StatusNotFound, body))
}

// Remote CPA deployments cannot receive a browser's localhost redirect. The
// operator copies the failed localhost /oauth/callback?code=... URL into the
// authenticated panel; the plugin then performs the same STS exchange its
// loopback listener would have performed locally.
func handleLoginCallback(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	var body struct {
		State       string `json:"state"`
		CallbackURL string `json:"callback_url"`
	}
	if json.Unmarshal(req.Body, &body) != nil {
		return errorJSON(400, "invalid callback request")
	}
	loginMu.Lock()
	// Only the state saved when CPA started this login selects the session.
	// Huawei's query-string state belongs to its own authorization flow.
	session := loginSessions[strings.TrimSpace(body.State)]
	loginMu.Unlock()
	if session == nil || time.Now().After(session.expires) {
		return errorJSON(400, "unknown or expired login state")
	}
	u, err := url.Parse(strings.TrimSpace(body.CallbackURL))
	if err != nil || (u.Path != codeArtsOAuthCallback && u.Path != "/authentication") {
		return errorJSON(400, "paste the full localhost callback URL, for example http://127.0.0.1:40000/oauth/callback?code=...&state=...")
	}
	if !callbackAddressMatches(u, session.callbackURL) {
		return errorJSON(400, "that callback URL belongs to a different sign-in attempt")
	}
	code := strings.TrimSpace(u.Query().Get("code"))
	secret := strings.TrimSpace(u.Query().Get("secret"))
	if code == "" && secret == "" {
		return errorJSON(400, "callback URL contains neither an OAuth code nor a legacy secret")
	}
	session.mu.Lock()
	if session.err != "" {
		session.mu.Unlock()
		return errorJSON(409, "login has already failed; start a new authorization")
	}
	if session.received {
		session.mu.Unlock()
		return errorJSON(409, "callback already received")
	}
	session.authorizationCode = code
	session.secret = secret
	session.received = true
	session.callbackAt = time.Now()
	session.mu.Unlock()
	session.closeListener()
	return jsonResponse(200, []byte(`{"success":true}`))
}

// callbackAddressMatches accepts loopback spelling changes without accepting
// a different port, scheme, or remote host. The submitted URL is never fetched;
// token exchange must keep using the original advertised redirect URI.
func callbackAddressMatches(submitted *url.URL, advertised string) bool {
	expected, errParse := url.Parse(advertised)
	if errParse != nil || submitted == nil || submitted.User != nil || submitted.Fragment != "" ||
		(submitted.Scheme != "http" && submitted.Scheme != "https") ||
		submitted.Scheme != expected.Scheme || submitted.Hostname() == "" || submitted.Port() != expected.Port() {
		return false
	}
	if strings.EqualFold(submitted.Hostname(), expected.Hostname()) {
		return true
	}
	isLoopback := func(host string) bool {
		return strings.EqualFold(host, "localhost") || host == "127.0.0.1" || host == "::1"
	}
	return isLoopback(submitted.Hostname()) && isLoopback(expected.Hostname())
}

// handleLoginStatus reports the stage of one sign-in flow. CPA's own
// get-auth-status route only distinguishes pending/success/error, so a pending
// flow would otherwise be silent; this route says whether the plugin is waiting
// for the browser callback or already exchanging the authorization code, and what the
// exchange last answered.
func handleLoginStatus(query url.Values) pluginapi.ManagementResponse {
	state := strings.TrimSpace(query.Get("state"))
	loginMu.Lock()
	session := loginSessions[state]
	loginMu.Unlock()
	if session == nil {
		return jsonResponse(http.StatusNotFound, mustJSON(map[string]any{
			"status":  "unknown",
			"message": "登录会话不存在或已结束，请重新点击「开始授权」。",
		}))
	}
	session.mu.Lock()
	payload := map[string]any{
		"state":        session.state,
		"status":       "pending",
		"message":      session.progressLocked(),
		"callback_url": session.callbackURL,
		"attempts":     session.attempts,
		"expires_at":   session.expires.UTC().Format(time.RFC3339),
	}
	if session.credential != nil {
		payload["status"] = "success"
	}
	if session.err != "" {
		payload["status"] = "error"
	}
	if !session.callbackAt.IsZero() {
		payload["callback_at"] = session.callbackAt.UTC().Format(time.RFC3339)
	}
	if session.lastStatus != 0 {
		payload["last_status"] = session.lastStatus
	}
	session.mu.Unlock()
	return jsonResponse(http.StatusOK, mustJSON(payload))
}

// mustJSON marshals a management payload, falling back to a minimal error body
// so the route can never return an empty response.
func mustJSON(payload any) []byte {
	raw, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		return []byte(`{"status":"error","message":"failed to encode response"}`)
	}
	return raw
}

// managementRouteSuffix strips the management or resource prefix and the plugin
// namespace, yielding a path like "/quota" or "/panel".
//
// Both namespaces are matched against the full path because the host passes the
// path it received in full: authenticated routes arrive as
// /v0/management/<pluginID>/..., browser resources as
// /v0/resource/plugins/<pluginID>/....
func managementRouteSuffix(path string) string {
	trimmed := strings.TrimSpace(path)
	for _, prefix := range []string{
		managementBasePath + "/" + providerID,
		"/v0/resource/plugins/" + providerID,
	} {
		if strings.HasPrefix(trimmed, prefix) {
			trimmed = strings.TrimPrefix(trimmed, prefix)
			break
		}
	}
	if trimmed == "" {
		return "/"
	}
	if !strings.HasPrefix(trimmed, "/") {
		trimmed = "/" + trimmed
	}
	return strings.TrimRight(trimmed, "/")
}

// ---------------------------------------------------------------------------
// account + quota handlers
// ---------------------------------------------------------------------------

// accountView is one account row for the panel and the accounts route.
type accountView struct {
	AuthIndex   string                  `json:"auth_index"`
	AuthID      string                  `json:"auth_id"`
	Name        string                  `json:"name"`
	Label       string                  `json:"label"`
	Status      string                  `json:"status"`
	Disabled    bool                    `json:"disabled"`
	UserName    string                  `json:"user_name"`
	UserID      string                  `json:"user_id"`
	DomainID    string                  `json:"domain_id"`
	LoginType   string                  `json:"login_type"`
	ExpiresAt   string                  `json:"expires_at"`
	Concurrency *sessionConcurrencyView `json:"concurrency,omitempty"`

	Plan      string       `json:"plan,omitempty"`
	PlanName  string       `json:"plan_name,omitempty"`
	PlanURL   string       `json:"plan_url,omitempty"`
	ResetDate string       `json:"reset_date,omitempty"`
	Meters    []quotaMeter `json:"meters,omitempty"`
	Features  any          `json:"features,omitempty"`
	// Benefit is the limited-time daily token pool, reported next to the
	// subscription meters because it is accounted separately upstream.
	Benefit          *benefitBalance `json:"benefit,omitempty"`
	BenefitError     string          `json:"benefit_error,omitempty"`
	BenefitFetchedAt string          `json:"benefit_fetched_at,omitempty"`
	QuotaFetchedAt   string          `json:"quota_fetched_at,omitempty"`
	QuotaError       string          `json:"quota_error,omitempty"`

	LastRefresh string `json:"last_refresh,omitempty"`
	NextRefresh string `json:"next_refresh,omitempty"`
}

// codeartsAccounts reads every CodeArts Doer credential from the host store and
// merges the cached quota snapshot.
func codeartsAccounts() ([]accountView, error) {
	files, errList := hostAuthList()
	if errList != nil {
		return nil, fmt.Errorf("list auth files: %w", errList)
	}
	cached := quotas.all()

	views := make([]accountView, 0, len(files))
	for _, file := range files {
		if normalizeProvider(file.Provider) != providerID && normalizeProvider(file.Type) != providerID {
			continue
		}
		view := accountView{
			AuthIndex: file.AuthIndex,
			AuthID:    file.ID,
			Name:      file.Name,
			Label:     firstNonEmptyString(file.Label, file.Account, file.Email),
			Status:    file.Status,
			Disabled:  file.Disabled,
		}
		if !file.LastRefresh.IsZero() {
			view.LastRefresh = file.LastRefresh.Format(time.RFC3339)
		}
		if !file.NextRetryAfter.IsZero() {
			view.NextRefresh = file.NextRetryAfter.Format(time.RFC3339)
		}

		// Credential identity comes from the plugin-owned storage blob.
		if storage, errGet := hostAuthGet(file.AuthIndex); errGet == nil {
			if cred, errCred := credentialFromStorage(storage); errCred == nil && cred != nil {
				view.UserName = cred.UserName
				view.UserID = cred.UserID
				view.DomainID = cred.DomainID
				view.LoginType = cred.LoginType
				view.ExpiresAt = cred.ExpiresAt
				v := accountSessionConcurrency(config(), cred, file.Path)
				view.Concurrency = &v
			}
		}

		if snapshot, ok := cached[file.AuthIndex]; ok {
			view.Plan = snapshot.Plan
			view.PlanName = snapshot.PlanName
			view.PlanURL = snapshot.PlanURL
			view.ResetDate = snapshot.ResetDate
			view.Meters = snapshot.Meters
			view.Features = snapshot.Features
			view.Benefit = snapshot.Benefit
			view.BenefitError = snapshot.BenefitError
			if !snapshot.BenefitFetchedAt.IsZero() {
				view.BenefitFetchedAt = snapshot.BenefitFetchedAt.Format(time.RFC3339)
			}
			view.QuotaError = snapshot.Error
			if !snapshot.FetchedAt.IsZero() {
				view.QuotaFetchedAt = snapshot.FetchedAt.Format(time.RFC3339)
			}
		}
		views = append(views, view)
	}
	sort.Slice(views, func(i, j int) bool { return views[i].AuthIndex < views[j].AuthIndex })
	return views, nil
}

func handleAccounts() pluginapi.ManagementResponse {
	views, errAccounts := codeartsAccounts()
	if errAccounts != nil {
		return errorJSON(http.StatusBadGateway, errAccounts.Error())
	}
	body, _ := json.Marshal(map[string]any{
		"provider": providerID,
		"count":    len(views),
		"accounts": views,
	})
	return jsonResponse(http.StatusOK, body)
}

// Optional benefit I/O is never part of quota.fetch, the scheduler or account
// listing. This one-account request can be cancelled independently by the UI.
func handleBenefitBalance(query url.Values, callbackID string) pluginapi.ManagementResponse {
	authIndex := strings.TrimSpace(query.Get("auth_index"))
	if authIndex == "" {
		return errorJSON(http.StatusBadRequest, "auth_index is required")
	}
	if strings.TrimSpace(callbackID) == "" {
		return errorJSON(http.StatusNotImplemented, "host did not provide a cancellable management context")
	}
	if config().APIMode == "native" {
		return errorJSON(http.StatusBadRequest, "benefit allowance is only available in agent mode")
	}
	files, errList := hostAuthList()
	if errList != nil {
		return errorJSON(http.StatusBadGateway, "could not read the account inventory")
	}
	for _, file := range files {
		if file.AuthIndex != authIndex || (normalizeProvider(file.Provider) != providerID && normalizeProvider(file.Type) != providerID) {
			continue
		}
		storage, errGet := hostAuthGet(authIndex)
		if errGet != nil {
			return errorJSON(http.StatusBadGateway, "could not read the account credential")
		}
		cred, errCred := credentialFromStorage(storage)
		if errCred != nil || !cred.valid() {
			return errorJSON(http.StatusBadRequest, "the account credential is incomplete")
		}
		balance, errFetch := fetchBenefitBalance(config(), cred, callbackID)
		if errFetch != nil {
			quotas.putBenefit(authIndex, nil, errFetch.Error())
			return errorJSON(http.StatusBadGateway, errFetch.Error())
		}
		quotas.putBenefit(authIndex, &balance, "")
		body, _ := json.Marshal(map[string]any{"auth_index": authIndex, "benefit": balance})
		return jsonResponse(http.StatusOK, body)
	}
	return errorJSON(http.StatusNotFound, "CodeArts account was not found")
}

func handleAccountModels(query url.Values, callbackID string) pluginapi.ManagementResponse {
	authIndex := strings.TrimSpace(query.Get("auth_index"))
	if authIndex == "" {
		return errorJSON(http.StatusBadRequest, "auth_index is required")
	}
	files, errList := hostAuthList()
	if errList != nil {
		return errorJSON(http.StatusBadGateway, "could not read the account inventory")
	}
	for _, file := range files {
		if file.AuthIndex != authIndex || (normalizeProvider(file.Provider) != providerID && normalizeProvider(file.Type) != providerID) {
			continue
		}
		storage, errGet := hostAuthGet(authIndex)
		if errGet != nil {
			return errorJSON(http.StatusBadGateway, "could not read the account credential")
		}
		cred, errCred := credentialFromStorage(storage)
		if errCred != nil || !cred.valid() {
			return errorJSON(http.StatusBadRequest, "the account credential is incomplete")
		}
		refreshed, errRefresh := prepareCredentialForUse(authIndex, cred)
		if errRefresh != nil {
			return errorJSON(http.StatusServiceUnavailable, "the account credential expired and silent refresh failed")
		}
		cred = refreshed
		includeLiveBenefit := true
		if query.Has("include_benefit") {
			includeLiveBenefit, _ = strconv.ParseBool(strings.TrimSpace(query.Get("include_benefit")))
		}
		catalog := accountAgentModelCatalog(config(), cred, callbackID)
		if includeLiveBenefit {
			// This is an explicit diagnostic refresh. It is synchronous so the
			// management request context cancels host HTTP rather than leaving a
			// detached benefit request behind. It never rewrites the credential:
			// doing so could roll back an OAuth token rotated while this slow
			// diagnostic was running. Account-scoped catalogues are saved separately.
			_ = discoverModelCatalog(config(), cred, callbackID)
			catalog = accountModelCatalog(config(), cred, callbackID)
		}
		return jsonResponse(http.StatusOK, mustJSON(map[string]any{
			"auth_index": authIndex, "models": catalog.Models, "count": len(catalog.Models),
			"source": catalog.Source, "warnings": catalog.Warnings, "fetched_at": catalog.FetchedAt,
		}))
	}
	return errorJSON(http.StatusNotFound, "unknown CodeArts auth_index")
}

func handleQuotaGet(query map[string][]string) pluginapi.ManagementResponse {
	authIndex := firstQueryValue(query, "auth_index")
	if authIndex != "" {
		views, errAccounts := codeartsAccounts()
		if errAccounts != nil {
			return errorJSON(http.StatusBadGateway, errAccounts.Error())
		}
		for _, view := range views {
			if view.AuthIndex == authIndex {
				body, _ := json.Marshal(view)
				return jsonResponse(http.StatusOK, body)
			}
		}
		return errorJSON(http.StatusNotFound, "unknown auth_index")
	}
	cached := quotas.all()
	body, _ := json.Marshal(map[string]any{
		"provider":  providerID,
		"count":     len(cached),
		"snapshots": cached,
	})
	return jsonResponse(http.StatusOK, body)
}

// handleQuotaRefresh refreshes one or all accounts, forcing an upstream read.
func handleQuotaRefresh(req pluginapi.ManagementRequest, callbackIDs ...string) pluginapi.ManagementResponse {
	authIndex := ""
	if len(req.Body) > 0 {
		var body struct {
			AuthIndex string `json:"auth_index"`
		}
		if errUnmarshal := json.Unmarshal(req.Body, &body); errUnmarshal == nil {
			authIndex = strings.TrimSpace(body.AuthIndex)
		}
	}
	if authIndex == "" {
		authIndex = firstQueryValue(req.Query, "auth_index")
	}

	files, errList := hostAuthList()
	if errList != nil {
		return errorJSON(http.StatusBadGateway, errList.Error())
	}
	refreshed, failed := 0, 0
	var errorsSeen []string
	for _, file := range files {
		if normalizeProvider(file.Provider) != providerID && normalizeProvider(file.Type) != providerID {
			continue
		}
		if authIndex != "" && file.AuthIndex != authIndex {
			continue
		}
		storage, errGet := hostAuthGet(file.AuthIndex)
		if errGet != nil {
			failed++
			errorsSeen = append(errorsSeen, file.AuthIndex+": "+errGet.Error())
			continue
		}
		cred, errCred := credentialFromStorage(storage)
		if errCred != nil || !cred.valid() {
			failed++
			errorsSeen = append(errorsSeen, file.AuthIndex+": credential is incomplete")
			continue
		}
		if _, errFetch := fetchQuotaSnapshot(file.AuthIndex, cred, callbackIDs...); errFetch != nil {
			failed++
			errorsSeen = append(errorsSeen, file.AuthIndex+": "+errFetch.Error())
			continue
		}
		refreshed++
	}
	body, _ := json.Marshal(map[string]any{
		"success":   failed == 0,
		"refreshed": refreshed,
		"failed":    failed,
		"errors":    errorsSeen,
	})
	return jsonResponse(http.StatusOK, body)
}

// ---------------------------------------------------------------------------
// schedule handlers
// ---------------------------------------------------------------------------

func handleScheduleGet() pluginapi.ManagementResponse {
	cfg := config()
	body, _ := json.Marshal(scheduleView(cfg))
	return jsonResponse(http.StatusOK, body)
}

func handleScheduleRun(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	var body struct {
		Task string `json:"task"`
	}
	if len(req.Body) > 0 {
		_ = json.Unmarshal(req.Body, &body)
	}
	taskID := strings.TrimSpace(body.Task)
	if taskID == "" {
		taskID = firstQueryValue(req.Query, "task")
	}
	if taskID == "" {
		return errorJSON(http.StatusBadRequest, "task is required")
	}
	if errTrigger := triggerTask(taskID); errTrigger != nil {
		return errorJSON(http.StatusNotFound, errTrigger.Error())
	}
	out, _ := json.Marshal(map[string]any{
		"success": true,
		"task":    taskID,
		"note":    "the task was started in the background; poll /schedule for the outcome",
	})
	return jsonResponse(http.StatusOK, out)
}

// Persist only plugin-owned switches, never rewrite CPA's config.yaml.
func handleScheduleConfig(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	var body scheduleSwitchChange
	if len(req.Body) > 0 {
		if errUnmarshal := json.Unmarshal(req.Body, &body); errUnmarshal != nil {
			return errorJSON(http.StatusBadRequest, "invalid body: "+errUnmarshal.Error())
		}
	}

	return updateScheduleSwitches(body)
}

// ---------------------------------------------------------------------------
// credential import / export / delete
// ---------------------------------------------------------------------------

// handleImport accepts a credential JSON and writes it to the host auth store.
func handleImport(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	var body struct {
		AccessKeyID     string `json:"access_key_id"`
		SecretAccessKey string `json:"secret_access_key"`
		SecurityToken   string `json:"security_token"`
		DomainID        string `json:"domain_id"`
		UserName        string `json:"user_name"`
		UserID          string `json:"user_id"`
		ExpiresAt       string `json:"expires_at"`
		LoginType       string `json:"login_type"`
		Name            string `json:"name"`
	}
	if len(req.Body) == 0 {
		return errorJSON(http.StatusBadRequest, "request body is required")
	}
	if errUnmarshal := json.Unmarshal(req.Body, &body); errUnmarshal != nil {
		return errorJSON(http.StatusBadRequest, "invalid body: "+errUnmarshal.Error())
	}
	parsed, errParse := credentialFromStorage(req.Body)
	if errParse != nil || !parsed.valid() {
		return errorJSON(400, "access_key_id and secret_access_key are both required")
	}
	cred := *parsed
	if cred.LoginType == "" {
		cred.LoginType = "AKSK"
		if cred.SecurityToken != "" {
			cred.LoginType = "WEB"
		}
	}
	if !cred.valid() {
		return errorJSON(http.StatusBadRequest, "access_key_id and secret_access_key are both required")
	}

	name := strings.TrimSpace(body.Name)
	if name == "" {
		name = credentialFileName(&cred)
	}
	if !strings.HasSuffix(strings.ToLower(name), ".json") {
		name += ".json"
	}

	payload, errBuild := buildAuthFileDocument(cred)
	if errBuild != nil {
		return errorJSON(http.StatusInternalServerError, errBuild.Error())
	}
	if errSave := hostAuthSave(name, payload); errSave != nil {
		return errorJSON(http.StatusBadGateway, "host.auth.save: "+errSave.Error())
	}
	logInfo("imported credential through the management API", map[string]any{"file": name})

	out, _ := json.Marshal(map[string]any{
		"success":    true,
		"name":       name,
		"user":       firstNonEmptyString(cred.UserName, cred.UserID),
		"login_type": cred.LoginType,
	})
	return jsonResponse(http.StatusOK, out)
}

// buildAuthFileDocument renders the auth file the host persists. The credential
// lives under the plugin-owned key so host-managed keys survive a round trip.
func buildAuthFileDocument(cred credential) ([]byte, error) {
	doc := map[string]any{
		"type":      providerID,
		storageKey:  cred,
		"user_name": cred.UserName,
		"user_id":   cred.UserID,
	}
	if cred.DomainID != "" {
		doc["domain_id"] = cred.DomainID
	}
	if cred.LoginType != "" {
		doc["login_type"] = cred.LoginType
	}
	if cred.ExpiresAt != "" {
		doc["expires_at"] = cred.ExpiresAt
	}
	return json.MarshalIndent(doc, "", "  ")
}

// handleExport dumps every credential in a re-importable form. The response is
// served from an admin-authenticated route, so returning key material is
// acceptable here; the unauthenticated panel never calls it without the key.
func handleExport() pluginapi.ManagementResponse {
	views, errAccounts := codeartsAccounts()
	if errAccounts != nil {
		return errorJSON(http.StatusBadGateway, errAccounts.Error())
	}
	type exported struct {
		Name string     `json:"name"`
		Cred credential `json:"credential"`
	}
	items := make([]exported, 0, len(views))
	for _, view := range views {
		storage, errGet := hostAuthGet(view.AuthIndex)
		if errGet != nil {
			continue
		}
		cred, errCred := credentialFromStorage(storage)
		if errCred != nil || cred == nil || !cred.valid() {
			continue
		}
		items = append(items, exported{Name: view.Name, Cred: *cred})
	}
	body, _ := json.Marshal(map[string]any{
		"provider":    providerID,
		"exported_at": time.Now().UTC().Format(time.RFC3339),
		"count":       len(items),
		"accounts":    items,
		"warning":     "this document contains live credentials; store it accordingly",
	})
	return jsonResponse(http.StatusOK, body)
}

// handleDelete removes one credential from the host store.
//
// The strict ownership check matters: deleting an auth file is destructive, so
// the target must be a credential this plugin owns, identified by auth_index.
func handleDelete(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	var body struct {
		AuthIndex string `json:"auth_index"`
	}
	if len(req.Body) > 0 {
		_ = json.Unmarshal(req.Body, &body)
	}
	authIndex := strings.TrimSpace(body.AuthIndex)
	if authIndex == "" {
		authIndex = firstQueryValue(req.Query, "auth_index")
	}
	if authIndex == "" {
		return errorJSON(http.StatusBadRequest, "auth_index is required")
	}

	files, errList := hostAuthList()
	if errList != nil {
		return errorJSON(http.StatusBadGateway, errList.Error())
	}
	for _, file := range files {
		if file.AuthIndex != authIndex {
			continue
		}
		// Refuse to delete anything that is not a CodeArts Doer credential.
		if normalizeProvider(file.Provider) != providerID && normalizeProvider(file.Type) != providerID {
			return errorJSON(http.StatusForbidden, "auth_index does not belong to the "+providerID+" provider")
		}
		path := strings.TrimSpace(file.Path)
		if path == "" {
			return errorJSON(http.StatusConflict, "credential has no backing auth file to delete")
		}
		// The plugin ABI has no host.auth.delete, so the file is removed
		// directly. That is destructive, so the path is constrained to a plain
		// .json file inside the auth directory before anything is touched.
		if errDelete := deleteAuthFileSafely(path); errDelete != nil {
			return errorJSON(http.StatusBadGateway, errDelete.Error())
		}
		quotas.forget(authIndex)
		logInfo("deleted credential through the management API", map[string]any{
			"account": authIndex,
			"file":    file.Name,
		})
		out, _ := json.Marshal(map[string]any{"success": true, "auth_index": authIndex, "name": file.Name})
		return jsonResponse(http.StatusOK, out)
	}
	return errorJSON(http.StatusNotFound, "unknown auth_index")
}

// handleUsage serves the token usage rollup collected from host usage records.
func handleUsage() pluginapi.ManagementResponse {
	body, errMarshal := json.Marshal(usage.snapshot())
	if errMarshal != nil {
		return errorJSON(http.StatusInternalServerError, errMarshal.Error())
	}
	return jsonResponse(http.StatusOK, body)
}

// ---------------------------------------------------------------------------
// daily benefit / check-in handlers
// ---------------------------------------------------------------------------

// handleCheckin claims the daily benefit by running a checkin task now.
//
// Claims are driven by explicit checkin task configuration. Reading a model
// catalog must never trigger an entitlement claim as a side effect.
func handleCheckin(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	var body struct {
		Task string `json:"task"`
	}
	if len(req.Body) > 0 {
		_ = json.Unmarshal(req.Body, &body)
	}
	taskID := strings.TrimSpace(body.Task)
	if taskID == "" {
		taskID = firstQueryValue(req.Query, "task")
	}

	cfg := config()
	var candidates []ScheduleTask
	for _, task := range cfg.visibleScheduleTasks() {
		if task.Type == TaskCheckin || task.Type == TaskDailyClaim {
			candidates = append(candidates, task)
		}
	}
	if len(candidates) == 0 {
		out, _ := json.Marshal(map[string]any{
			"success":    false,
			"configured": false,
			"error":      "no checkin task is configured",
			"how_to_enable": []string{
				"Model discovery does not claim benefits. Claim eligible benefits in the official client or configure an activity-specific checkin task.",
				"Open the activity page in your browser, use the browser devtools Network tab, click the daily claim, and copy the request URL, method, body and headers.",
				"Add a schedule task of type \"checkin\" with checkin_url (and checkin_body/checkin_headers if the request needs them).",
				"Then call this route again, or let cron claim it automatically.",
			},
		})
		return jsonResponse(http.StatusOK, out)
	}

	selected := candidates[0]
	if taskID != "" {
		found := false
		for _, task := range candidates {
			if task.ID == taskID {
				selected, found = task, true
				break
			}
		}
		if !found {
			return errorJSON(http.StatusNotFound, "unknown checkin task "+taskID)
		}
	}
	if errTrigger := triggerTask(selected.ID); errTrigger != nil {
		return errorJSON(http.StatusNotFound, errTrigger.Error())
	}
	out, _ := json.Marshal(map[string]any{
		"success":    true,
		"configured": true,
		"task":       selected.ID,
		"note":       "claim started in the background; GET /benefits for the outcome",
	})
	return jsonResponse(http.StatusOK, out)
}

// handleBenefits explains the daily check-in situation for the panel.
func handleBenefits() pluginapi.ManagementResponse {
	cfg := config()
	states := scheduler.snapshot()

	type taskView struct {
		ID         string `json:"id"`
		Cron       string `json:"cron"`
		Enabled    bool   `json:"enabled"`
		URL        string `json:"checkin_url,omitempty"`
		Method     string `json:"checkin_method,omitempty"`
		NextRun    string `json:"next_run,omitempty"`
		LastRun    string `json:"last_run,omitempty"`
		LastResult string `json:"last_result,omitempty"`
		LastError  string `json:"last_error,omitempty"`
	}
	next := scheduler.nextRuns()
	var tasks []taskView
	for _, task := range cfg.visibleScheduleTasks() {
		if task.Type != TaskCheckin && task.Type != TaskDailyClaim {
			continue
		}
		view := taskView{
			ID:      task.ID,
			Cron:    task.Cron,
			Enabled: task.isEnabled(),
			URL:     task.CheckinURL,
			Method:  task.CheckinMethod,
		}
		if value, ok := next[task.ID]; ok {
			view.NextRun = value
		}
		if state, ok := states[task.ID]; ok {
			if !state.LastRunAt.IsZero() {
				view.LastRun = state.LastRunAt.Format(time.RFC3339)
			}
			view.LastResult = state.LastResult
			view.LastError = state.LastError
		}
		tasks = append(tasks, view)
	}

	scheduleEnabled := cfg.Schedule.Enabled
	out, _ := json.Marshal(map[string]any{
		"configured":       len(tasks) > 0,
		"schedule_enabled": scheduleEnabled,
		"tasks":            tasks,
		"explanation": "daily-benefit-claim now uses ops delivery/claim/confirm for USER_LOGIN CREDIT activities only. " +
			"Success requires an upstream CONFIRMED/CONSUMED state. Activity credits are not the developer gateway's benefit token pool.",
		"how_to_capture": []string{
			"Open the daily benefit page in a browser and sign in.",
			"Open devtools -> Network, clear it, then click the daily claim button.",
			"Copy the request as cURL: URL, method, body and any custom headers.",
			"Put them into a checkin task (checkin_url / checkin_method / checkin_body / checkin_headers).",
			"Set checkin_success_marker to text that appears only on a real claim, so a no-op is not reported as success.",
		},
	})
	return jsonResponse(http.StatusOK, out)
}

// ---------------------------------------------------------------------------
// response helpers
// ---------------------------------------------------------------------------

func jsonResponse(status int, body []byte) pluginapi.ManagementResponse {
	return pluginapi.ManagementResponse{
		StatusCode: status,
		Headers:    http.Header{"Content-Type": []string{"application/json; charset=utf-8"}},
		Body:       body,
	}
}

func htmlResponse(html string) pluginapi.ManagementResponse {
	return pluginapi.ManagementResponse{
		StatusCode: http.StatusOK,
		Headers:    http.Header{"Content-Type": []string{"text/html; charset=utf-8"}},
		Body:       []byte(html),
	}
}

func errorJSON(status int, message string) pluginapi.ManagementResponse {
	body, _ := json.Marshal(map[string]any{"success": false, "error": message})
	return jsonResponse(status, body)
}

func firstQueryValue(query map[string][]string, key string) string {
	values, ok := query[key]
	if !ok || len(values) == 0 {
		return ""
	}
	return strings.TrimSpace(values[0])
}

// statusPage renders the machine-readable provider status. It deliberately
// contains no credential material because this route also backs the
// unauthenticated browser resource.
func statusPage(cfg *Config) pluginapi.ManagementResponse {
	type modelView struct {
		ID            string `json:"id"`
		DisplayName   string `json:"display_name"`
		UpstreamModel string `json:"upstream_model"`
		Source        string `json:"source"`
	}
	models := make([]modelView, 0, len(cfg.Models)+len(cfg.BenefitModels))
	seenModels := make(map[string]bool, cap(models))
	for _, source := range [][]ModelConfig{cfg.Models, cfg.BenefitModels} {
		for _, model := range source {
			if model.ID == "" || seenModels[model.ID] {
				continue
			}
			seenModels[model.ID] = true
			models = append(models, modelView{
				ID:            model.ID,
				DisplayName:   model.DisplayName,
				UpstreamModel: cfg.upstreamModel(model.ID),
				Source:        firstNonEmptyString(model.Source, "configured"),
			})
		}
	}

	payload := map[string]any{
		"plugin": map[string]any{
			"id":      providerID,
			"name":    "CodeArts",
			"version": version,
		},
		"endpoint": map[string]any{
			"base_url":          cfg.BaseURL,
			"protocol_mode":     cfg.APIMode,
			"chat_path":         chatPathFor(cfg),
			"quota_path":        "/snap-manager/v1/statistics/plugin",
			"web_login_base":    cfg.WebLoginBase,
			"agent_id":          cfg.AgentID,
			"default_model_id":  cfg.DefaultModelID,
			"plugin_name":       cfg.PluginName,
			"plugin_version":    cfg.PluginVersion,
			"language":          cfg.Language,
			"heartbeat":         cfg.Heartbeat,
			"request_timeout_s": cfg.RequestTimeoutSeconds,
		},
		"models": models,
		"model_catalog": map[string]any{
			"scope": "configured_only", "discovery_enabled": cfg.DiscoverModels,
			"account_catalog_path":      managementBasePath + "/" + providerID + "/models?auth_index=...",
			"live_benefit_refresh_path": managementBasePath + "/" + providerID + "/models?auth_index=...&include_benefit=true",
		},
		"schedule": map[string]any{
			"enabled":  cfg.Schedule.Enabled,
			"timezone": firstNonEmptyString(cfg.Schedule.Timezone, time.Local.String()),
			"tasks":    describeTasks(cfg),
		},
		// Derived from the same source as plugin.register so the status page can
		// never advertise a different capability set than the plugin declares.
		"capabilities": declaredCapabilities(),
		"notes": []string{
			"Model discovery does not claim benefits; claims require the official client or an explicitly configured checkin task.",
			"Recurring work is driven by the plugin's own cron scheduler because the plugin ABI provides no timer.",
			"Resource pages are not admin-authenticated, so no credential material is shown here.",
			"Sign in with GET /v0/management/" + providerID + "-auth-url (admin key required) and open the returned url.",
		},
		"generated_at": time.Now().UTC().Format(time.RFC3339),
	}

	body, errMarshal := json.MarshalIndent(payload, "", "  ")
	if errMarshal != nil {
		body = []byte(`{"error":"failed to encode status"}`)
	}
	return pluginapi.ManagementResponse{
		StatusCode: http.StatusOK,
		Headers:    http.Header{"Content-Type": []string{"application/json; charset=utf-8"}},
		Body:       body,
	}
}

func chatPathFor(cfg *Config) string {
	if cfg.APIMode == "native" {
		return nativeChatPath
	}
	return agentModePath
}
