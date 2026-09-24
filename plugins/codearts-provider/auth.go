package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// storageKey is the field name under which the credential is persisted inside
// the auth record's StorageJSON. The host round-trips this opaque blob back to
// the plugin on every executor call.
const storageKey = "codearts_provider_credential"

// loginSession tracks one interactive browser login flow. CodeArts Agent
// 26.9.x uses OAuth authorization-code + PKCE and binds the resulting token to
// a DPoP proof key. The legacy ticket/secret fields remain so credentials made
// by the 26.3.x flow can still be completed while operators migrate.
type loginSession struct {
	state             string
	ticketID          string
	secret            string
	authorizationCode string
	oauthContext      *oauthLoginContext
	hostAuthDir       string

	mu       sync.Mutex
	listener net.Listener
	server   *http.Server
	received bool
	expires  time.Time
	cancel   context.CancelFunc

	// callbackURL is the URL handed to the CodeArts console; bindAddr is the
	// local socket it is served from. They differ when the deployment advertises
	// a different host (login_callback_base) or binds a non-loopback interface.
	callbackURL string
	bindAddr    string

	// progress is the observable state of the flow, so a page can always tell
	// whether it is waiting for the browser or for the ticket exchange.
	callbackAt   time.Time
	attempts     int
	lastStatus   int
	lastMessage  string
	closedListen bool

	credential *credential
	err        string
}

var (
	loginMu       sync.Mutex
	loginSessions = map[string]*loginSession{}
)

// authParse recognises an auth JSON file for this provider and maps it onto a
// CLIProxyAPI auth record. Files are matched on the `type` field so users can
// drop a credential file into the auth directory:
//
//	{"type":"codearts-provider","access_key_id":"...","secret_access_key":"...",
//	 "security_token":"...","domain_id":"..."}
func authParse(request []byte) ([]byte, error) {
	var req pluginapi.AuthParseRequest
	if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
		return nil, fmt.Errorf("decode auth parse request: %w", errUnmarshal)
	}
	if normalizeProvider(req.Provider) != providerID {
		// Not ours: report unhandled so other parsers can try.
		return okEnvelope(pluginapi.AuthParseResponse{Handled: false})
	}
	parsed, errCredential := credentialFromStorage(req.RawJSON)
	if errCredential != nil || !parsed.valid() {
		return okEnvelope(pluginapi.AuthParseResponse{Handled: false})
	}
	cred := *parsed
	if !cred.valid() {
		logWarn("auth file is missing an access key or secret key", map[string]any{"file": req.FileName})
		return okEnvelope(pluginapi.AuthParseResponse{Handled: false})
	}

	storage, errMarshal := json.Marshal(map[string]any{storageKey: cred})
	if errMarshal != nil {
		return nil, errMarshal
	}
	label := cred.UserName
	if label == "" {
		label = cred.UserID
	}
	if label == "" {
		label = req.FileName
	}
	return okEnvelope(pluginapi.AuthParseResponse{
		Handled: true,
		Auth: pluginapi.AuthData{
			Provider:         providerID,
			FileName:         req.FileName,
			Label:            label,
			StorageJSON:      storage,
			Metadata:         credentialAuthMetadata(&cred),
			NextRefreshAfter: refreshDeadline(&cred),
		},
	})
}

// authLoginStart begins the OAuth PKCE browser login used by CodeArts Agent
// 26.9.x. A loopback listener supports a local CPA deployment. When CPA runs on
// another machine the browser will fail to open its own localhost callback; the
// operator copies that full URL back into the authenticated management panel,
// which supplies the authorization code to this same session.
func authLoginStart(request []byte) ([]byte, error) {
	var req pluginapi.AuthLoginStartRequest
	if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
		return nil, fmt.Errorf("decode login start request: %w", errUnmarshal)
	}
	cfg := config()
	oauthContext, errOAuth := newOAuthLoginContext()
	if errOAuth != nil {
		return failEnvelope("login_unavailable", errOAuth.Error(), http.StatusInternalServerError)
	}

	bindAddr := fmt.Sprintf("%s:%d", cfg.LoginCallbackBind, cfg.LoginCallbackPort)
	listener, errListen := net.Listen("tcp", bindAddr)
	if errListen != nil && cfg.LoginCallbackPort != 0 {
		// A pinned port may already be in use; an ephemeral one still lets the
		// flow work for a browser on the same machine.
		logWarn("the configured login callback port is unavailable; using an ephemeral port", map[string]any{
			"bind":  bindAddr,
			"error": errListen.Error(),
		})
		bindAddr = fmt.Sprintf("%s:0", cfg.LoginCallbackBind)
		listener, errListen = net.Listen("tcp", bindAddr)
	}
	if errListen != nil {
		return failEnvelope("login_unavailable", "failed to bind the browser callback listener on "+bindAddr+": "+errListen.Error(), http.StatusInternalServerError)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	state := randomHex(16)
	callbackURL := fmt.Sprintf("http://127.0.0.1:%d%s", port, codeArtsOAuthCallback)
	if cfg.LoginCallbackBase != "" {
		callbackURL = cfg.LoginCallbackBase + codeArtsOAuthCallback
	}

	ticketID := randomUUIDv4()
	ctx, cancel := context.WithTimeout(context.Background(), cfg.loginTimeout())

	session := &loginSession{
		state:        state,
		ticketID:     ticketID,
		cancel:       cancel,
		listener:     listener,
		callbackURL:  callbackURL,
		bindAddr:     listener.Addr().String(),
		expires:      time.Now().Add(cfg.loginTimeout()),
		oauthContext: oauthContext,
		hostAuthDir:  strings.TrimSpace(req.Host.AuthDir),
	}
	mux := http.NewServeMux()
	mux.HandleFunc(codeArtsOAuthCallback, session.handleCallback)
	// Keep the old callback path for a browser that was already open during an
	// upgrade. New authorization URLs never advertise it.
	mux.HandleFunc("/authentication", session.handleCallback)
	server := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	session.mu.Lock()
	session.server = server
	session.mu.Unlock()

	go func() {
		if errServe := server.Serve(listener); errServe != nil && errServe != http.ErrServerClosed {
			logWarn("login callback listener stopped", map[string]any{"error": errServe.Error()})
		}
	}()

	loginMu.Lock()
	loginSessions[state] = session
	evictLoginSessionsLocked()
	loginMu.Unlock()

	// Exchange the authorization code as soon as either the loopback server or
	// the management panel supplies it.
	go session.poll(ctx)

	loginURL := buildLoginURL(cfg, ticketID, callbackURL, oauthContext)
	logInfo("started OAuth PKCE browser login", map[string]any{"port": port})

	return okEnvelope(pluginapi.AuthLoginStartResponse{
		Provider:  providerID,
		URL:       loginURL,
		State:     state,
		ExpiresAt: session.expires,
		Metadata:  map[string]any{"callback_url": callbackURL, "ticket_id": ticketID, "flow": "oauth_pkce"},
	})
}

// maxLoginSessions bounds the live login flows. Each one holds a loopback
// listener, a server goroutine and a poll goroutine for the whole login timeout,
// so an unbounded table would let a caller that never finishes a login exhaust
// sockets. Callers hold loginMu.
const maxLoginSessions = 8

// evictLoginSessionsLocked drops expired sessions and, when the table is still
// full, the session closest to expiry. Evicting the oldest keeps the newest
// attempt - the one the operator just started - working.
func evictLoginSessionsLocked() {
	now := time.Now()
	for key, item := range loginSessions {
		if now.After(item.expires) {
			item.stop()
			delete(loginSessions, key)
		}
	}
	for len(loginSessions) > maxLoginSessions {
		var oldestKey string
		var oldestExpiry time.Time
		for key, item := range loginSessions {
			if oldestKey == "" || item.expires.Before(oldestExpiry) {
				oldestKey, oldestExpiry = key, item.expires
			}
		}
		if oldestKey == "" {
			return
		}
		logWarn("dropping an unfinished login flow to make room for a new one", map[string]any{"state": oldestKey})
		loginSessions[oldestKey].stop()
		delete(loginSessions, oldestKey)
	}
}

// authLoginPoll reports the current state of an interactive login flow.
func authLoginPoll(request []byte) ([]byte, error) {
	var req pluginapi.AuthLoginPollRequest
	if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
		return nil, fmt.Errorf("decode login poll request: %w", errUnmarshal)
	}
	loginMu.Lock()
	session := loginSessions[strings.TrimSpace(req.State)]
	loginMu.Unlock()
	if session == nil {
		return okEnvelope(pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: "unknown or expired login state; restart the login flow",
		})
	}
	if time.Now().After(session.expires) {
		session.stop()
		loginMu.Lock()
		delete(loginSessions, session.state)
		loginMu.Unlock()
		return okEnvelope(pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: "the login flow expired before it completed",
		})
	}
	if errCallback := session.consumeHostOAuthCallback(firstNonEmptyString(req.Host.AuthDir, session.hostAuthDir)); errCallback != nil {
		session.mu.Lock()
		if session.err == "" {
			session.err = errCallback.Error()
		}
		session.mu.Unlock()
	}

	session.mu.Lock()
	cred := session.credential
	errMessage := session.err
	session.mu.Unlock()

	if errMessage != "" {
		session.stop()
		loginMu.Lock()
		delete(loginSessions, session.state)
		loginMu.Unlock()
		return okEnvelope(pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: errMessage,
		})
	}
	if cred == nil {
		// Pending, but never silent: the message says whether the plugin is still
		// waiting for the browser or already exchanging the authorization code.
		return okEnvelope(pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusPending,
			Message: session.progress(),
		})
	}

	// A browser authorization is a WEB credential; stamping it on the stored
	// credential matches the extension's transferToUserInfo(..., "WEB") and lets
	// the account view report how the account signed in.
	stamped := *cred
	stamped.LoginType = firstNonEmptyString(stamped.LoginType, "WEB")
	storage, errMarshal := json.Marshal(map[string]any{storageKey: stamped})
	if errMarshal != nil {
		return nil, errMarshal
	}
	session.stop()
	loginMu.Lock()
	delete(loginSessions, session.state)
	loginMu.Unlock()

	label := cred.UserName
	if label == "" {
		label = providerID
	}
	logInfo("browser login completed", map[string]any{"user": label})
	return okEnvelope(pluginapi.AuthLoginPollResponse{
		Status:  pluginapi.AuthLoginStatusSuccess,
		Message: "signed in to CodeArts Doer",
		Auth: pluginapi.AuthData{
			Provider:         providerID,
			Label:            label,
			FileName:         credentialFileName(&stamped),
			StorageJSON:      storage,
			Metadata:         credentialAuthMetadata(&stamped),
			Attributes:       map[string]string{"provider_type": providerID},
			NextRefreshAfter: refreshDeadline(cred),
		},
	})
}

// consumeHostOAuthCallback imports a callback submitted with the CPA session
// state explicitly supplied. Huawei's callback state is independent of this
// state, so the stock CPA UI cannot submit the unmodified Huawei URL here.
// The CodeArts panel submits through the plugin's authenticated route instead.
func (s *loginSession) consumeHostOAuthCallback(authDir string) error {
	if s == nil || strings.TrimSpace(authDir) == "" || strings.TrimSpace(s.state) == "" {
		return nil
	}
	path := filepath.Join(authDir, fmt.Sprintf(".oauth-%s-%s.oauth", providerID, s.state))
	file, errOpen := os.Open(path)
	if os.IsNotExist(errOpen) {
		return nil
	}
	if errOpen != nil {
		return fmt.Errorf("read CPA OAuth callback: %w", errOpen)
	}
	raw, errRead := io.ReadAll(io.LimitReader(file, 64<<10))
	errClose := file.Close()
	if errRead != nil {
		return fmt.Errorf("read CPA OAuth callback: %w", errRead)
	}
	if errClose != nil {
		return fmt.Errorf("close CPA OAuth callback: %w", errClose)
	}
	var callback struct {
		Code  string `json:"code"`
		State string `json:"state"`
		Error string `json:"error"`
	}
	if errDecode := json.Unmarshal(raw, &callback); errDecode != nil {
		return fmt.Errorf("decode CPA OAuth callback: %w", errDecode)
	}
	if strings.TrimSpace(callback.State) != s.state {
		return fmt.Errorf("CPA OAuth callback state does not match this login")
	}
	code := strings.TrimSpace(callback.Code)
	callbackError := strings.TrimSpace(callback.Error)
	if code == "" && callbackError == "" {
		return fmt.Errorf("CPA OAuth callback contains neither code nor error")
	}
	if errRemove := os.Remove(path); errRemove != nil && !os.IsNotExist(errRemove) {
		return fmt.Errorf("remove consumed CPA OAuth callback: %w", errRemove)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.received || s.err != "" {
		return nil
	}
	if callbackError != "" {
		s.err = "CodeArts OAuth authorization failed: " + callbackError
		return nil
	}
	s.authorizationCode = code
	s.received = true
	s.callbackAt = time.Now()
	return nil
}

// authRefresh serializes all refresh entry points for one account. The host
// auto-refresh loop and a request-time catch-up can become due together; the
// in-memory snapshot makes the second caller reuse the first caller's rotated
// credential even before the host finishes persisting it.
func authRefresh(request []byte) ([]byte, error) {
	var req pluginapi.AuthRefreshRequest
	if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
		return nil, fmt.Errorf("decode auth refresh request: %w", errUnmarshal)
	}
	cred, errCred := credentialFromStorage(req.StorageJSON)
	if errCred != nil {
		return failEnvelope("invalid_credential", "stored credential could not be decoded: "+errCred.Error(), http.StatusUnauthorized)
	}
	if !cred.valid() {
		return failEnvelope("invalid_credential", "stored credential is incomplete", http.StatusUnauthorized)
	}
	lockValue, _ := credentialRefreshLocks.LoadOrStore(credentialRefreshKey(cred), &sync.Mutex{})
	lock := lockValue.(*sync.Mutex)
	lock.Lock()
	defer lock.Unlock()

	current := latestStoredCredential(req.AuthID, cred)
	if credentialMaterialChanged(cred, current) {
		return authRefreshEnvelope(current)
	}
	raw, errRefresh := authRefreshUnlocked(request)
	if errRefresh != nil {
		return nil, errRefresh
	}
	if updated, errUpdated := credentialFromRefreshEnvelope(raw); errUpdated == nil {
		rememberRefreshedCredential(updated)
	}
	return raw, nil
}

// authRefreshUnlocked renews the temporary AK/SK pair through the CodeArts
// refresh endpoint. Callers must hold the per-account refresh lock.
func authRefreshUnlocked(request []byte) ([]byte, error) {
	var req pluginapi.AuthRefreshRequest
	if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
		return nil, fmt.Errorf("decode auth refresh request: %w", errUnmarshal)
	}
	cred, errCred := credentialFromStorage(req.StorageJSON)
	if errCred != nil {
		return failEnvelope("invalid_credential", "stored credential could not be decoded: "+errCred.Error(), http.StatusUnauthorized)
	}
	if !cred.valid() {
		return failEnvelope("invalid_credential", "stored credential is incomplete", http.StatusUnauthorized)
	}
	cfg := config()

	// CodeArts Agent 26.9.x refreshes through the STS OAuth endpoint using the
	// stored refresh token, PKCE verifier and DPoP private key. Do this before
	// the legacy token/renew path so newly issued credentials never fall back to
	// the retired ticket-era protocol.
	if strings.TrimSpace(cred.RefreshToken) != "" || cred.OAuthContext != nil {
		updated, status, _, errRefresh := oauthRefreshCredential(cfg, cred)
		if errRefresh != nil {
			return failEnvelope("refresh_rejected", errRefresh.Error(), httpStatusFor(status))
		}
		logInfo("refreshed OAuth upstream credential", map[string]any{"expires_at": updated.ExpiresAt})
		return authRefreshEnvelope(updated)
	}

	// A long-lived AK/SK pair has nothing to renew: the endpoint requires a
	// security token. Hand the credential back unchanged rather than failing.
	if strings.TrimSpace(cred.SecurityToken) == "" {
		logInfo("credential has no security token; treating it as a permanent key pair", nil)
		storage, errMarshal := json.Marshal(map[string]any{storageKey: *cred})
		if errMarshal != nil {
			return nil, errMarshal
		}
		return okEnvelope(pluginapi.AuthRefreshResponse{
			Auth: pluginapi.AuthData{
				Provider:         providerID,
				Label:            cred.UserName,
				StorageJSON:      storage,
				Metadata:         credentialAuthMetadata(cred),
				NextRefreshAfter: refreshDeadline(cred),
			},
			NextRefreshAfter: refreshDeadline(cred),
		})
	}

	// token/renew accepts a security-token authenticated request and returns a
	// fresh credential triple.
	renewBody, errMarshal := json.Marshal(map[string]any{
		"access":           cred.AccessKeyID,
		"securitytoken":    cred.SecurityToken,
		"duration_seconds": 24 * 60 * 60,
	})
	if errMarshal != nil {
		return nil, errMarshal
	}
	endpoint := strings.TrimRight(cfg.BaseURL, "/") + "/snap-manager/v1/token/renew"
	headers, errSign := signRequest(http.MethodPost, endpoint, map[string]string{
		"Content-Type": "application/json",
		"Accept":       "application/json",
	}, renewBody, cred, cfg.SignHost)
	if errSign != nil {
		return failEnvelope("sign_failed", errSign.Error(), http.StatusInternalServerError)
	}

	response, errDo := hostHTTPDo(http.MethodPost, endpoint, headers, renewBody)
	if errDo != nil {
		return failEnvelope("upstream_unreachable", "token renew request failed: "+errDo.Error(), http.StatusBadGateway)
	}
	if response.StatusCode != http.StatusOK {
		return failEnvelope("refresh_rejected", fmt.Sprintf("token renew returned HTTP %d: %s", response.StatusCode, truncate(string(response.Body), 400)), httpStatusFor(response.StatusCode))
	}

	updated, errDecode := decodeCredentialResponse(response.Body)
	if errDecode != nil {
		return failEnvelope("refresh_failed", "token renew response could not be parsed: "+errDecode.Error(), http.StatusBadGateway)
	}
	// Preserve identity fields the renew endpoint does not echo back.
	if updated.DomainID == "" {
		updated.DomainID = cred.DomainID
	}
	if updated.UserName == "" {
		updated.UserName = cred.UserName
	}
	if updated.UserID == "" {
		updated.UserID = cred.UserID
	}
	updated.LoginType = firstNonEmptyString(updated.LoginType, cred.LoginType, "WEB")

	logInfo("refreshed upstream credential", map[string]any{"expires_at": updated.ExpiresAt})
	return authRefreshEnvelope(&updated)
}

func authRefreshEnvelope(updated *credential) ([]byte, error) {
	if updated == nil || !updated.valid() {
		return failEnvelope("refresh_failed", "refreshed credential is incomplete", http.StatusBadGateway)
	}
	storage, errMarshal := json.Marshal(map[string]any{storageKey: *updated})
	if errMarshal != nil {
		return nil, errMarshal
	}
	return okEnvelope(pluginapi.AuthRefreshResponse{
		Auth: pluginapi.AuthData{
			Provider:         providerID,
			Label:            updated.UserName,
			StorageJSON:      storage,
			Metadata:         credentialAuthMetadata(updated),
			NextRefreshAfter: refreshDeadline(updated),
		},
		NextRefreshAfter: refreshDeadline(updated),
	})
}

// ---------------------------------------------------------------------------
// login session plumbing
// ---------------------------------------------------------------------------

// handleCallback accepts the new OAuth authorization code and, for compatibility,
// the old ticket-flow secret. A remote CPA receives the same URL through the
// authenticated management panel rather than through this loopback listener.
func (s *loginSession) handleCallback(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
	w.Header().Set("Access-Control-Max-Age", "86400")
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if time.Now().After(s.expires) {
		w.WriteHeader(http.StatusGone)
		return
	}

	code := strings.TrimSpace(r.URL.Query().Get("code"))
	secret := callbackSecret(r)
	if code == "" && secret == "" && r.URL.Path == codeArtsOAuthCallback {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("callback URL is missing authorization code"))
		return
	}
	s.mu.Lock()
	first := !s.received
	if first {
		s.authorizationCode = code
		s.secret = secret
		s.received = true
		s.callbackAt = time.Now()
	}
	s.mu.Unlock()

	if first {
		if code != "" {
			logInfo("received OAuth authorization callback", map[string]any{"state": s.state})
		} else if secret == "" {
			logWarn("the legacy browser callback carried no secret; exchanging the ticket anyway", map[string]any{"state": s.state})
		}
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`<!doctype html><meta charset="utf-8"><title>CodeArts</title><p>授权信息已收到，请返回 CPA 等待账号保存。</p>`))
}

// callbackSecret extracts the secret from the callback request. The console
// redirects the browser with it as a query parameter; a JSON or form body is
// also accepted so a proxied, scripted or content-type-less callback still
// works. An empty result is not an error: the secret is optional here.
func callbackSecret(r *http.Request) string {
	if secret := strings.TrimSpace(r.URL.Query().Get("secret")); secret != "" {
		return secret
	}
	if r.Body == nil {
		return ""
	}
	raw, errRead := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if errRead != nil || len(bytes.TrimSpace(raw)) == 0 {
		return ""
	}
	var body struct {
		Secret string `json:"secret"`
	}
	if json.Unmarshal(raw, &body) == nil && strings.TrimSpace(body.Secret) != "" {
		return strings.TrimSpace(body.Secret)
	}
	if strings.Contains(r.Header.Get("Content-Type"), "form-urlencoded") {
		if errParse := r.ParseForm(); errParse == nil {
			if secret := strings.TrimSpace(r.PostForm.Get("secret")); secret != "" {
				return secret
			}
		}
	}
	if values, errParse := url.ParseQuery(string(raw)); errParse == nil {
		return strings.TrimSpace(values.Get("secret"))
	}
	return ""
}

// closeListener stops accepting callbacks without ending the login session: the
// ticket exchange still has to run.
func (s *loginSession) closeListener() {
	s.mu.Lock()
	already := s.closedListen
	s.closedListen = true
	server := s.server
	s.mu.Unlock()
	if already || server == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = server.Shutdown(ctx)
}

// progress renders the observable state of the flow for the panel.
func (s *loginSession) progress() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.progressLocked()
}

// progressLocked is progress for callers that already hold the session lock.
func (s *loginSession) progressLocked() string {
	switch {
	case s.credential != nil:
		return "授权成功，账号已写入 CPA。"
	case s.err != "":
		return s.err
	case !s.received:
		return "等待浏览器完成 OAuth 授权。远程 CPA 请把浏览器最终的 localhost 回调地址完整粘贴到面板。"
	case s.attempts == 0:
		return "已收到浏览器回调，正在通过 STS 换取凭证…"
	case s.lastStatus == http.StatusOK:
		return "已收到回调，凭证尚未就绪，正在重试…"
	case s.lastMessage != "":
		return fmt.Sprintf("已收到回调，换取凭证返回 HTTP %d：%s", s.lastStatus, s.lastMessage)
	default:
		return fmt.Sprintf("已收到回调，换取凭证返回 HTTP %d，正在重试…", s.lastStatus)
	}
}

// poll exchanges the OAuth authorization code once it arrives. The legacy
// ticket exchange remains available only for an old callback carrying secret.
func (s *loginSession) poll(ctx context.Context) {
	defer s.stop()
	cfg := config()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			s.mu.Lock()
			if s.credential == nil && s.err == "" {
				if !s.received {
					s.err = "登录超时：未收到 OAuth 回调。如果 CPA 在远程服务器上，请把浏览器最终的 localhost /oauth/callback 地址完整粘贴到面板。"
				} else if s.lastMessage != "" {
					s.err = fmt.Sprintf("登录超时：换取凭证一直返回 HTTP %d（%s）。", s.lastStatus, s.lastMessage)
				} else {
					s.err = "登录超时：未能换取凭证。"
				}
			}
			s.mu.Unlock()
			return
		case <-ticker.C:
		}

		s.mu.Lock()
		received := s.received
		code := s.authorizationCode
		s.mu.Unlock()
		if !received {
			continue
		}
		var cred *credential
		var retry bool
		var status int
		var message string
		var errPoll error
		if code != "" {
			cred, status, message, errPoll = oauthExchangeAuthorizationCode(cfg, code, s.callbackURL, s.oauthContext)
			retry = false
		} else {
			cred, retry, status, message, errPoll = s.exchangeTicket(cfg)
		}
		s.mu.Lock()
		s.attempts++
		s.lastStatus = status
		if message != "" {
			s.lastMessage = message
		}
		if cred != nil {
			s.credential = cred
		} else if errPoll != nil && !retry {
			s.err = errPoll.Error()
		}
		done := s.credential != nil || s.err != ""
		s.mu.Unlock()
		if done {
			return
		}
	}
}

// exchangeTicket performs one ticket poll. retry reports whether another attempt
// is worthwhile; the status and a short body excerpt are returned for progress
// reporting even when the exchange keeps failing.
func (s *loginSession) exchangeTicket(cfg *Config) (*credential, bool, int, string, error) {
	endpoint := strings.TrimRight(cfg.BaseURL, "/") + "/snap-manager/v1/login/ticket"
	parsed, errParse := url.Parse(endpoint)
	if errParse != nil {
		return nil, false, 0, "", errParse
	}
	query := parsed.Query()
	query.Set("ticket_id", s.ticketID)
	s.mu.Lock()
	query.Set("secret", s.secret)
	s.mu.Unlock()
	parsed.RawQuery = query.Encode()

	// This request is intentionally unsigned: it is exactly what the extension
	// sends (plugin identity headers and the ticket/secret pair), and the ticket
	// is the credential here, not an AK/SK pair.
	headers := map[string]string{
		"Content-Type":   "application/json;charset=UTF-8",
		"plugin-name":    cfg.PluginName,
		"plugin-version": cfg.PluginVersion,
		"Accept":         "application/json",
	}
	response, errDo := hostHTTPDo(http.MethodGet, parsed.String(), headers, nil)
	if errDo != nil {
		return nil, true, 0, truncate(errDo.Error(), 200), nil
	}
	if response.StatusCode != http.StatusOK {
		// The ticket is not ready yet; keep polling until the deadline, exactly
		// like the extension, and report the last status for the panel.
		return nil, true, response.StatusCode, truncate(string(response.Body), 300), nil
	}
	cred, errDecode := decodeCredentialResponse(response.Body)
	if errDecode != nil {
		return nil, true, response.StatusCode, truncate(errDecode.Error(), 200), nil
	}
	if !cred.valid() {
		return nil, true, response.StatusCode, "凭证字段不完整", nil
	}
	return &cred, false, response.StatusCode, "", nil
}

func (s *loginSession) stop() {
	if s == nil {
		return
	}
	if s.cancel != nil {
		s.cancel()
	}
	if s.server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = s.server.Shutdown(ctx)
		cancel()
		return
	}
	if s.listener != nil {
		_ = s.listener.Close()
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// decodeCredentialResponse extracts the credential triple from the CodeArts
// envelope. The upstream nests it under `credential`:
//
//	{"credential":{"access":"..","secret":"..","securitytoken":"..",
//	               "expires_at":".."},"domain_id":"..","user_name":".."}
func decodeCredentialResponse(body []byte) (credential, error) {
	var envelope struct {
		Credential *struct {
			Access        string `json:"access"`
			Secret        string `json:"secret"`
			SecurityToken string `json:"securitytoken"`
			ExpiresAt     string `json:"expires_at"`
		} `json:"credential"`
		DomainID  string `json:"domain_id"`
		UserName  string `json:"user_name"`
		UserID    string `json:"user_id"`
		LoginType string `json:"login_type"`
	}
	if errUnmarshal := json.Unmarshal(body, &envelope); errUnmarshal != nil {
		return credential{}, errUnmarshal
	}
	if envelope.Credential == nil {
		return credential{}, fmt.Errorf("response contains no credential object")
	}
	cred := credential{
		AccessKeyID:     strings.TrimSpace(envelope.Credential.Access),
		SecretAccessKey: strings.TrimSpace(envelope.Credential.Secret),
		SecurityToken:   strings.TrimSpace(envelope.Credential.SecurityToken),
		DomainID:        strings.TrimSpace(envelope.DomainID),
		UserName:        strings.TrimSpace(envelope.UserName),
		UserID:          strings.TrimSpace(envelope.UserID),
		ExpiresAt:       strings.TrimSpace(envelope.Credential.ExpiresAt),
		LoginType:       strings.TrimSpace(envelope.LoginType),
	}
	if !cred.valid() || cred.SecurityToken == "" {
		return credential{}, fmt.Errorf("response contains an incomplete temporary credential")
	}
	return cred, nil
}

// credentialFromStorage pulls the credential out of the opaque storage blob the
// host round-trips to the plugin.
func credentialFromStorage(storage []byte) (*credential, error) {
	if len(storage) == 0 {
		return nil, nil
	}
	// Look the field up by name rather than through a struct tag, so the key
	// exists in exactly one place (storageKey) and cannot drift from what
	// buildAuthFileDocument writes.
	var document map[string]any
	if errUnmarshal := json.Unmarshal(storage, &document); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	if raw, ok := document[storageKey]; ok {
		nested, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("invalid credential object")
		}
		document = nested
	}
	var oauthContext *oauthLoginContext
	if rawContext, ok := document["oauth_context"]; ok && rawContext != nil {
		encoded, errMarshal := json.Marshal(rawContext)
		if errMarshal != nil {
			return nil, fmt.Errorf("encode OAuth context: %w", errMarshal)
		}
		var parsed oauthLoginContext
		if errUnmarshal := json.Unmarshal(encoded, &parsed); errUnmarshal != nil {
			return nil, fmt.Errorf("decode OAuth context: %w", errUnmarshal)
		}
		oauthContext = &parsed
	}
	return &credential{
		AccessKeyID:     firstString(document, "access_key_id", "accessKeyId", "ak"),
		SecretAccessKey: firstString(document, "secret_access_key", "secretAccessKey", "sk"),
		SecurityToken:   firstString(document, "security_token", "securityToken", "accessToken"),
		DomainID:        firstString(document, "domain_id", "domainId", "X-Domain-Id"),
		UserName:        firstString(document, "user_name", "userName"),
		UserID:          firstString(document, "user_id", "userId"),
		ExpiresAt:       firstString(document, "expires_at", "expiresAt"),
		LoginType:       firstString(document, "login_type", "loginType"),
		RefreshToken:    firstString(document, "refresh_token", "refreshToken"),
		OAuthContext:    oauthContext,
	}, nil
}

func stopLoginSessions() {
	loginMu.Lock()
	sessions := loginSessions
	loginSessions = map[string]*loginSession{}
	loginMu.Unlock()
	for _, session := range sessions {
		session.stop()
	}
}

// refreshDeadline returns when the host should next renew the credential. The
// extension renews hourly. OAuth credentials can last only a few hours, so they
// are scheduled at half-life (bounded to 30 minutes..2 hours); legacy temporary
// credentials are renewed an hour before expiry.
//
// A credential without a security token is a permanent AK/SK pair; the renewal
// endpoint does not apply to it, so it is parked far in the future instead of
// scheduling a refresh that would fail on every attempt.
func refreshDeadline(cred *credential) time.Time {
	if cred == nil || strings.TrimSpace(cred.SecurityToken) == "" {
		return time.Now().Add(30 * 24 * time.Hour)
	}
	if cred.ExpiresAt != "" {
		if parsed, errParse := time.Parse(time.RFC3339, cred.ExpiresAt); errParse == nil {
			if cred.RefreshToken != "" && cred.OAuthContext != nil {
				remaining := time.Until(parsed)
				if remaining <= 0 {
					return time.Now().Add(time.Minute)
				}
				delay := remaining / 2
				if delay < 30*time.Minute {
					delay = 30 * time.Minute
				}
				if delay > 2*time.Hour {
					delay = 2 * time.Hour
				}
				return time.Now().Add(delay)
			}
			if target := parsed.Add(-time.Hour); target.After(time.Now()) {
				return target
			}
			return time.Now().Add(5 * time.Minute)
		}
	}
	return time.Now().Add(time.Hour)
}

// credentialAuthMetadata contains only non-secret host scheduling and identity
// fields. In particular, the OAuth refresh token and proof key remain inside
// StorageJSON. CLIProxyAPI requires a refresh interval (or a built-in provider
// policy) in addition to NextRefreshAfter before its generic refresh loop will
// call a third-party auth provider when the deadline becomes due.
func credentialAuthMetadata(cred *credential) map[string]any {
	metadata := map[string]any{"type": providerID}
	if cred == nil {
		return metadata
	}
	metadata["user_name"] = cred.UserName
	metadata["user_id"] = cred.UserID
	metadata["domain_id"] = cred.DomainID
	metadata["session_identity"] = credentialRefreshKey(cred)
	loginType := cred.LoginType
	if strings.TrimSpace(cred.SecurityToken) == "" {
		loginType = firstNonEmptyString(loginType, "AKSK")
	}
	metadata["login_type"] = loginType
	if strings.TrimSpace(cred.ExpiresAt) != "" {
		metadata["expires_at"] = cred.ExpiresAt
	}
	if strings.TrimSpace(cred.SecurityToken) != "" {
		metadata["refresh_interval_seconds"] = int64(time.Hour / time.Second)
	}
	return metadata
}

// buildLoginURL mirrors CodeArts Agent 26.9.x WebLoginStrategy.openAuthorizeUrl.
func buildLoginURL(cfg *Config, ticketID, callbackURL string, context *oauthLoginContext) string {
	base := cfg.WebLoginBase
	if base == "" {
		base = cfg.IDEBaseURL
	}
	if base == "" {
		base = "https://codearts.huaweicloud.com"
	}
	base = strings.TrimRight(base, "/") + "/portal/authorize"
	if context == nil {
		return base
	}
	callback, _ := url.Parse(callbackURL)
	locale := "en"
	if strings.HasPrefix(strings.ToLower(cfg.Language), "zh") {
		locale = "zh-cn"
	}
	params := [][2]string{
		{"theme", "2"},
		{"locale", locale},
		{"uri_scheme", codeArtsOAuthURIScheme},
		{"client_id", codeArtsOAuthClientID},
		{"port", callback.Port()},
		{"code_challenge", context.PKCEPair.CodeChallenge},
		{"code_challenge_method", context.PKCEPair.CodeChallengeMethod},
		{"ticket_id", ticketID},
		{"auth_callback_url", callbackURL},
		{"plugin-name", cfg.PluginName},
		{"plugin-version", cfg.PluginVersion},
	}
	parts := make([]string, 0, len(params))
	for _, pair := range params {
		parts = append(parts, pair[0]+"="+encodeURIComponent(pair[1]))
	}
	return base + "?" + strings.Join(parts, "&")
}

func encodeURIComponent(value string) string {
	encoded := url.QueryEscape(value)
	encoded = strings.ReplaceAll(encoded, "+", "%20")
	for _, pair := range [][2]string{{"%21", "!"}, {"%27", "'"}, {"%28", "("}, {"%29", ")"}, {"%2A", "*"}} {
		encoded = strings.ReplaceAll(encoded, pair[0], pair[1])
	}
	return encoded
}

func normalizeProvider(provider string) string {
	return strings.ToLower(strings.TrimSpace(provider))
}

func providerLoginFile(label string) string {
	label = strings.TrimSpace(label)
	if label == "" {
		label = providerID
	}
	return providerID + "-" + sanitizeFileToken(label) + ".json"
}

func credentialFileName(cred *credential) string {
	identity := cred.DomainID + ":" + cred.UserID
	if cred.UserID == "" {
		identity = cred.AccessKeyID
	}
	return providerLoginFile(firstNonEmptyString(cred.UserName, "account") + "-" + sha256Hex([]byte(identity))[:12])
}

func sanitizeFileToken(value string) string {
	var builder strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			builder.WriteRune(r)
		default:
			builder.WriteByte('-')
		}
	}
	out := strings.Trim(builder.String(), "-")
	if out == "" {
		return "account"
	}
	return out
}

func firstString(payload map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := payload[key].(string); ok {
			if trimmed := strings.TrimSpace(value); trimmed != "" {
				return trimmed
			}
		}
	}
	return ""
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "..."
}

func randomHex(n int) string {
	buffer := make([]byte, n)
	if _, errRead := rand.Read(buffer); errRead != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buffer)
}

// randomUUIDv4 produces the dashed UUID form the CodeArts console expects.
func randomUUIDv4() string {
	buffer := make([]byte, 16)
	if _, errRead := rand.Read(buffer); errRead != nil {
		return randomHex(16)
	}
	buffer[6] = (buffer[6] & 0x0f) | 0x40
	buffer[8] = (buffer[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", buffer[0:4], buffer[4:6], buffer[6:8], buffer[8:10], buffer[10:16])
}

func httpStatusFor(status int) int {
	if status >= 400 && status < 600 {
		return status
	}
	return http.StatusBadGateway
}
