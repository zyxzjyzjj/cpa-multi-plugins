package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type benefitCatalogState struct {
	Version   int           `json:"version"`
	FetchedAt time.Time     `json:"fetched_at"`
	Models    []ModelConfig `json:"models"`
}

var benefitCatalogMemory = struct {
	sync.Mutex
	entries map[string]benefitCatalogState
	retry   map[string]time.Time
}{entries: map[string]benefitCatalogState{}, retry: map[string]time.Time{}}

func benefitCatalogIdentity(cfg *Config, cred *credential) string {
	return sha256Hex([]byte(cfg.BaseURL + "\n" + cfg.BenefitGatewayURL + "\n" + credentialRefreshKey(cred)))
}

func benefitStatePath(cfg *Config, cred *credential) (string, error) {
	name := "benefit-models-" + benefitCatalogIdentity(cfg, cred) + ".state"
	if cfg.StateDir != "" {
		return cfg.statePath("", name)
	}
	if cfg.scheduleStatePath != "" {
		return statePathInDirectory(filepath.Dir(cfg.scheduleStatePath), name)
	}
	return "", fmt.Errorf("账号状态目录尚未就绪，可设置 state_dir")
}

func storedBenefitCatalog(cfg *Config, cred *credential) (benefitCatalogState, bool) {
	if cfg == nil || !cred.valid() || cfg.APIMode == "native" {
		return benefitCatalogState{}, false
	}
	key := benefitCatalogIdentity(cfg, cred)
	benefitCatalogMemory.Lock()
	state, ok := benefitCatalogMemory.entries[key]
	benefitCatalogMemory.Unlock()
	if !ok {
		if path, err := benefitStatePath(cfg, cred); err == nil && readPluginState(path, &state) == nil {
			ok = true
		}
	}
	// Last-known-good is account scoped and bounded, not a fabricated fallback.
	if !ok || state.Version != 1 || state.FetchedAt.IsZero() || time.Since(state.FetchedAt) > 7*24*time.Hour {
		return benefitCatalogState{}, false
	}
	state.Models = append([]ModelConfig(nil), state.Models...)
	for i := range state.Models {
		state.Models[i].Source = "benefit"
	}
	return state, true
}

func saveBenefitCatalog(cfg *Config, cred *credential, models []ModelConfig) error {
	state := benefitCatalogState{Version: 1, FetchedAt: time.Now(), Models: append([]ModelConfig{}, models...)}
	key := benefitCatalogIdentity(cfg, cred)
	benefitCatalogMemory.Lock()
	if len(benefitCatalogMemory.entries) >= 512 {
		for k, v := range benefitCatalogMemory.entries {
			if time.Since(v.FetchedAt) > 24*time.Hour {
				delete(benefitCatalogMemory.entries, k)
				delete(benefitCatalogMemory.retry, k)
			}
		}
	}
	if len(benefitCatalogMemory.entries) >= 512 {
		for k := range benefitCatalogMemory.entries {
			delete(benefitCatalogMemory.entries, k)
			break
		}
	}
	benefitCatalogMemory.entries[key] = state
	benefitCatalogMemory.Unlock()
	path, err := benefitStatePath(cfg, cred)
	if err != nil && cfg.StateDir == "" && cfg.scheduleStatePath == "" {
		return nil
	} // host is still loading accounts
	if err != nil {
		return err
	}
	return savePluginState(path, state)
}

// Only CPA account registration performs bounded initial discovery. Request
// routing reads the saved catalogue, never waits on the optional gateway.
func bootstrapBenefitCatalog(cfg *Config, cred *credential) string {
	if !cfg.DiscoverModels || !cred.valid() || cfg.APIMode == "native" || cfg.BenefitGatewayURL == "" {
		return ""
	}
	key := benefitCatalogIdentity(cfg, cred)
	if state, ok := storedBenefitCatalog(cfg, cred); ok && time.Since(state.FetchedAt) < 6*time.Hour {
		return ""
	}
	release := acquireModelCatalogFlight("bootstrap:" + key)
	defer release()
	benefitCatalogMemory.Lock()
	if len(benefitCatalogMemory.retry) >= 512 {
		for k, v := range benefitCatalogMemory.retry {
			if time.Now().After(v) {
				delete(benefitCatalogMemory.retry, k)
			}
		}
	}
	if len(benefitCatalogMemory.retry) >= 512 {
		for k := range benefitCatalogMemory.retry {
			delete(benefitCatalogMemory.retry, k)
			break
		}
	}
	next := benefitCatalogMemory.retry[key]
	if time.Now().Before(next) {
		benefitCatalogMemory.Unlock()
		return ""
	}
	benefitCatalogMemory.retry[key] = time.Now().Add(time.Minute)
	benefitCatalogMemory.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	gateURL := strings.TrimRight(cfg.BaseURL, "/") + "/v1/benefit-gateway-config"
	headers := baseUpstreamHeaders(cfg, executorRequest{})
	headers["Agent-Type"], headers["Accept"] = "PromptCenter", "application/json"
	signed, err := signRequest(http.MethodGet, gateURL, headers, nil, cred, cfg.SignHost)
	if err != nil {
		return "福利模型签名失败"
	}
	resp, err := auxiliaryHTTP(ctx, cfg, http.MethodGet, gateURL, signed, nil)
	if err != nil || resp.StatusCode != 200 {
		return "福利模型首次发现失败或超时，可在插件页面重试"
	}
	var gate struct {
		Enabled *bool `json:"enabled"`
	}
	if json.Unmarshal(resp.Body, &gate) != nil || gate.Enabled == nil {
		return "福利模型可用性响应无效"
	}
	if !*gate.Enabled {
		_ = saveBenefitCatalog(cfg, cred, nil)
		return ""
	}
	endpoint := strings.TrimRight(cfg.BenefitGatewayURL, "/") + "/api/v1/gateway/config"
	copyCred := *cred
	copyCred.DomainID = ""
	signed, err = signRequest(http.MethodGet, endpoint, nil, nil, &copyCred, true)
	if err != nil {
		return "福利网关签名失败"
	}
	resp, err = auxiliaryHTTP(ctx, cfg, http.MethodGet, endpoint, signed, nil)
	if err != nil || resp.StatusCode != 200 {
		return "福利网关首次发现失败或超时，可在插件页面重试"
	}
	models, err := parseBenefitModels(resp.Body)
	if err != nil {
		return "福利模型目录响应无效"
	}
	if err = saveBenefitCatalog(cfg, cred, models); err != nil {
		return fmt.Sprintf("福利模型已发现，但持久化失败：%v", err)
	}
	return ""
}
