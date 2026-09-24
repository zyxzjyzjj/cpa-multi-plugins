package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// Counts include acquisition, active chat and upstream idle cleanup. They are
// process-local, shared by duplicate logins of the same upstream identity, and
// independent of model, protocol and temporary token rotation.
var sessionCapacity = struct {
	sync.Mutex
	active map[string]int
}{active: map[string]int{}}

// Serialize saves with admission, so a lowered limit governs every acquisition
// after the settings API returns. Existing permits are never revoked.
var sessionLimitSettingsMu sync.Mutex

type sessionLimitState struct {
	Version int `json:"version"`
	Limit   int `json:"limit"` // zero means inherit YAML/default
}
type sessionConcurrencyView struct {
	Limit    int    `json:"limit"`
	Override int    `json:"override"`
	Default  int    `json:"default"`
	Active   int    `json:"active"`
	Error    string `json:"error,omitempty"`
}

func defaultSessionLimit(cfg *Config) int {
	if cfg != nil && cfg.ChatSessionConcurrency >= 1 && cfg.ChatSessionConcurrency <= 64 {
		return cfg.ChatSessionConcurrency
	}
	return 3
}

func sessionCapacityKey(cfg *Config, identity string) string {
	return sha256Hex([]byte(strings.TrimRight(cfg.BaseURL, "/") + "\n" + identity))
}

func sessionLimitPath(cfg *Config, key, authPath string) (string, error) {
	name := "session-limit-" + key + ".state"
	if cfg.StateDir != "" {
		return cfg.statePath("", name)
	}
	if cfg.scheduleStatePath != "" {
		return statePathInDirectory(filepath.Dir(cfg.scheduleStatePath), name)
	}
	if filepath.IsAbs(authPath) {
		return cfg.statePath(filepath.Dir(authPath), name)
	}
	return "", nil // Nothing could have been saved without a resolved directory.
}

func readSessionLimit(cfg *Config, key, authPath string) (sessionConcurrencyView, error) {
	view := sessionConcurrencyView{Limit: defaultSessionLimit(cfg), Default: defaultSessionLimit(cfg)}
	path, err := sessionLimitPath(cfg, key, authPath)
	if err != nil {
		return view, err
	}
	if path == "" {
		return view, nil
	}
	var state sessionLimitState
	err = readPluginState(path, &state)
	if os.IsNotExist(err) {
		return view, nil
	}
	if err != nil {
		return view, fmt.Errorf("无法读取账号并发设置，请在账号卡重新保存")
	}
	if state.Version != 1 || state.Limit < 0 || state.Limit > 64 {
		return view, fmt.Errorf("账号并发设置无效，请重新保存")
	}
	view.Override = state.Limit
	if state.Limit > 0 {
		view.Limit = state.Limit
	}
	return view, nil
}

func accountSessionConcurrency(cfg *Config, cred *credential, authPath string) sessionConcurrencyView {
	key := sessionCapacityKey(cfg, credentialRefreshKey(cred))
	sessionLimitSettingsMu.Lock()
	view, err := readSessionLimit(cfg, key, authPath)
	sessionLimitSettingsMu.Unlock()
	if err != nil {
		view.Error = err.Error()
	}
	sessionCapacity.Lock()
	view.Active = sessionCapacity.active[key]
	sessionCapacity.Unlock()
	return view
}

type sessionLimitReached struct{ limit int }

func (e *sessionLimitReached) Error() string {
	return fmt.Sprintf("CodeArts account concurrent session limit (%d) reached; retry after an active request finishes", e.limit)
}

type sessionPermit struct {
	key  string
	once sync.Once
}

func (p *sessionPermit) release() {
	if p == nil {
		return
	}
	p.once.Do(func() {
		sessionCapacity.Lock()
		defer sessionCapacity.Unlock()
		if sessionCapacity.active[p.key] <= 1 {
			delete(sessionCapacity.active, p.key)
		} else {
			sessionCapacity.active[p.key]--
		}
	})
}

func acquireSessionPermit(cfg *Config, req executorRequest, cred *credential) (*sessionPermit, error) {
	if !cred.valid() {
		return nil, nil
	} // Existing explicit unsigned debugging mode.
	key := sessionCapacityKey(cfg, credentialRefreshKey(cred))
	sessionLimitSettingsMu.Lock()
	defer sessionLimitSettingsMu.Unlock()
	view, err := readSessionLimit(cfg, key, req.AuthAttributes["path"])
	if err != nil {
		return nil, err
	}
	sessionCapacity.Lock()
	defer sessionCapacity.Unlock()
	if sessionCapacity.active[key] >= view.Limit {
		return nil, &sessionLimitReached{view.Limit}
	}
	sessionCapacity.active[key]++
	return &sessionPermit{key: key}, nil
}

func sessionAdmissionFailure(err error) ([]byte, error) {
	var full *sessionLimitReached
	if errors.As(err, &full) {
		// CPA treats upstream 429s as quota exhaustion with credential cooldown.
		// A local capacity conflict is request-scoped (409): never cool a healthy
		// account merely because its other requests have not finished yet.
		return failEnvelope("session_concurrency_limit", full.Error(), http.StatusConflict)
	}
	return failEnvelope("session_concurrency_unavailable", "cannot read this account's concurrency settings; save the setting again in the plugin panel", http.StatusServiceUnavailable)
}

func candidateSessionAvailable(cfg *Config, candidate pluginapi.SchedulerAuthCandidate) bool {
	identity, _ := candidate.Metadata["session_identity"].(string)
	if identity == "" {
		cred := &credential{DomainID: firstString(candidate.Metadata, "domain_id"), UserID: firstString(candidate.Metadata, "user_id"), UserName: firstString(candidate.Metadata, "user_name")}
		if cred.DomainID == "" && cred.UserID == "" && cred.UserName == "" {
			return true
		} // Executor still enforces the limit atomically.
		identity = credentialRefreshKey(cred)
	}
	key := sessionCapacityKey(cfg, identity)
	sessionLimitSettingsMu.Lock()
	view, err := readSessionLimit(cfg, key, candidate.Attributes["path"])
	sessionLimitSettingsMu.Unlock()
	if err != nil {
		return false
	}
	sessionCapacity.Lock()
	active := sessionCapacity.active[key]
	sessionCapacity.Unlock()
	return active < view.Limit
}

// Authenticated settings route. It stores no credentials and never edits the
// host's auth JSON. A zero override restores the configured global default.
func handleSessionConcurrency(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	var body struct {
		AuthIndex string `json:"auth_index"`
		Limit     *int   `json:"limit"`
	}
	if json.Unmarshal(req.Body, &body) != nil || body.Limit == nil || *body.Limit < 0 || *body.Limit > 64 || strings.TrimSpace(body.AuthIndex) == "" {
		return errorJSON(400, "需要 auth_index 和 0–64 的整数 limit；0 表示继承默认值")
	}
	files, err := hostAuthList()
	if err != nil {
		return errorJSON(502, "读取账号列表失败")
	}
	for _, file := range files {
		if file.AuthIndex != body.AuthIndex || (normalizeProvider(file.Provider) != providerID && normalizeProvider(file.Type) != providerID) {
			continue
		}
		b, err := hostAuthGet(file.AuthIndex)
		if err != nil {
			return errorJSON(502, "读取账号失败")
		}
		cred, err := credentialFromStorage(b)
		if err != nil || !cred.valid() {
			return errorJSON(400, "账号凭证无效")
		}
		cfg := config()
		key := sessionCapacityKey(cfg, credentialRefreshKey(cred))
		path, err := sessionLimitPath(cfg, key, file.Path)
		if err != nil || path == "" {
			return errorJSON(409, "无法确定持久化目录，请配置 state_dir 后重试")
		}
		sessionLimitSettingsMu.Lock()
		err = savePluginState(path, sessionLimitState{Version: 1, Limit: *body.Limit})
		sessionLimitSettingsMu.Unlock()
		if err != nil {
			return errorJSON(500, err.Error())
		}
		return jsonResponse(200, mustJSON(map[string]any{"success": true, "persistent": true, "concurrency": accountSessionConcurrency(cfg, cred, file.Path), "note": "并发上限已保存；不影响正在运行的会话，也不会提高订阅本身的额度。"}))
	}
	return errorJSON(404, "未找到 CodeArts 账号")
}
