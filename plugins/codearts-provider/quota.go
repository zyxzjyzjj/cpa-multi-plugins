package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// This file implements the quota_provider capability and the CodeArts Doer
// subscription/quota view.
//
// The upstream endpoint is the same one the official extension uses to render
// its "usage" menu:
//
//	GET {base_url}/snap-manager/v1/statistics/plugin
//
// The response is a flat document whose fields the extension reads as:
//
//	{
//	  "show":   {"metrics": true, "package": true},
//	  "metrics": [{"name":"usageDataCodeCompletions","value":42,"show":true},
//	              {"name":"usageDataChatMessages","value":7,"show":true}],
//	  "end_date": "2026-10-01",
//	  "package": {"spec_code":"snap.enterprise","package_name_en":"...",
//	              "package_name_cn":"...","package_url":"...",
//	              "features":[{"name":"RagAgent","enable":true}, ...]}
//	}
//
// quotaSnapshot is the normalised view the plugin caches and serves.

// quotaSnapshot is one account's normalised subscription state.
type quotaSnapshot struct {
	AuthIndex string `json:"auth_index"`
	AuthID    string `json:"auth_id"`
	Label     string `json:"label"`

	// Plan is the upstream package spec code, for example "snap.enterprise".
	Plan string `json:"plan"`
	// PlanName is the human-readable package name.
	PlanName string `json:"plan_name"`
	// PlanURL is the upstream upgrade page, when the plan can be upgraded.
	PlanURL string `json:"plan_url"`
	// ResetDate is the upstream quota reset date ("end_date").
	ResetDate string `json:"reset_date"`

	// Meters are the usage rows the upstream both reports and asks to display.
	// A retired or unknown field is absent from this list rather than present
	// with a zero: the gateway signals "not reported" with a negative value and
	// marks fields it no longer shows with show:false, and rendering either as
	// 0% would tell the operator that nothing has been used.
	Meters []quotaMeter `json:"meters,omitempty"`

	// Features is the upstream feature enablement map.
	Features map[string]bool `json:"features"`

	// Benefit is the limited-time daily token pool from the benefit gateway. It
	// is separate from the subscription meters above: a benefit model can be
	// exhausted while every subscription meter still reads zero.
	Benefit *benefitBalance `json:"benefit,omitempty"`
	// BenefitError records a failed benefit allowance lookup without discarding
	// the subscription snapshot that did succeed.
	BenefitError     string    `json:"benefit_error,omitempty"`
	BenefitFetchedAt time.Time `json:"benefit_fetched_at,omitempty"`

	// FetchedAt is when this snapshot was taken.
	FetchedAt time.Time `json:"fetched_at"`
	// Error records the last fetch failure for this account, if any.
	Error string `json:"error,omitempty"`
}

// quotaMeter is one displayable usage metric from
// /snap-manager/v1/statistics/plugin.
//
// Upstream migrates metric names instead of deprecating them: the message-count
// row was retired (usageDataChatMessages, now show:false with a negative value)
// and its replacement reports tokens (usageTokenChatMessages with
// usage_token_num/package_token_amount). Every metric is parsed generically, so
// the next rename shows up as an extra row with its raw name instead of
// disappearing from the dashboard.
type quotaMeter struct {
	Name  string `json:"name"`
	Label string `json:"label"`
	// UsedPercent is the consumed share the upstream itself displays, or nil when
	// the upstream has no value to report.
	UsedPercent     *float64 `json:"used_percent,omitempty"`
	UsedTokens      int64    `json:"used_tokens,omitempty"`
	AllowanceTokens int64    `json:"allowance_tokens,omitempty"`
	CreditTotal     *float64 `json:"credit_total,omitempty"`
	CreditUsed      *float64 `json:"credit_used,omitempty"`
	CreditRemaining *float64 `json:"credit_remaining,omitempty"`
}

// quotaMeterLabels localises known metric names; anything else keeps the raw
// name, which is still more useful than hiding it.
var quotaMeterLabels = map[string]string{
	"usageDataCodeCompletions":   "代码补全额度",
	"usageDataChatMessages":      "对话消息额度",
	"usageTokenChatMessages":     "对话 token 额度",
	"usageTotalPackageCredit":    "套餐积分",
	"usageBasicPackageCredit":    "基础包积分",
	"usageOnDemandPackageCredit": "按需付费积分",
	"usageBonusPackageCredit":    "赠送积分",
}

// remainingFraction converts a meter into the host's "remaining fraction" model.
// It reports false when the meter carries no usable ratio, so an unknown value is
// skipped instead of being advertised as a full bucket.
func (m quotaMeter) remainingFraction() (float64, bool) {
	if m.UsedPercent == nil {
		return 0, false
	}
	used := *m.UsedPercent
	if used < 0 {
		return 0, false
	}
	if used > 100 {
		used = 100
	}
	return (100 - used) / 100, true
}

// Describe renders the host-facing description. The metric name is used rather
// than the localised label because the host surface is not language-specific.
func (m quotaMeter) Describe() string {
	if m.UsedPercent == nil {
		if m.AllowanceTokens > 0 {
			return fmt.Sprintf("%s: %d of %d tokens", m.Name, m.UsedTokens, m.AllowanceTokens)
		}
		return fmt.Sprintf("%s reported without a percentage", m.Name)
	}
	text := fmt.Sprintf("%s used: %.1f%%", m.Name, *m.UsedPercent)
	if m.AllowanceTokens > 0 || m.UsedTokens > 0 {
		text += fmt.Sprintf(" (%d of %d tokens)", m.UsedTokens, m.AllowanceTokens)
	}
	return text
}

// quotaCache holds the most recent snapshot per auth index. The scheduled
// quota_refresh task and the management API both read from it so a panel load
// does not fan out one upstream request per account.
type quotaCache struct {
	mu        sync.RWMutex
	snapshots map[string]quotaSnapshot
}

var quotas = &quotaCache{snapshots: map[string]quotaSnapshot{}}

func (c *quotaCache) put(snapshot quotaSnapshot) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.snapshots == nil {
		c.snapshots = map[string]quotaSnapshot{}
	}
	// Package and benefit refreshes are independent. Merge under the same lock
	// so a late package response cannot erase a newer benefit result.
	if old, ok := c.snapshots[snapshot.AuthIndex]; ok && snapshot.Benefit == nil && snapshot.BenefitError == "" {
		old = freshBenefitSnapshot(old)
		snapshot.Benefit, snapshot.BenefitError = old.Benefit, old.BenefitError
		snapshot.BenefitFetchedAt = old.BenefitFetchedAt
	}
	c.snapshots[snapshot.AuthIndex] = snapshot
}

func freshBenefitSnapshot(snapshot quotaSnapshot) quotaSnapshot {
	if !snapshot.BenefitFetchedAt.IsZero() && time.Since(snapshot.BenefitFetchedAt) > 5*time.Minute {
		snapshot.Benefit = nil
		snapshot.BenefitError = "cached benefit allowance expired; refresh it separately"
	}
	return snapshot
}

func (c *quotaCache) putBenefit(authIndex string, balance *benefitBalance, message string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.snapshots == nil {
		c.snapshots = map[string]quotaSnapshot{}
	}
	snapshot := c.snapshots[authIndex]
	snapshot.AuthIndex = authIndex
	snapshot.Benefit, snapshot.BenefitError = balance, message
	snapshot.BenefitFetchedAt = time.Now()
	c.snapshots[authIndex] = snapshot
}

func (c *quotaCache) get(authIndex string) (quotaSnapshot, bool) {
	if c == nil {
		return quotaSnapshot{}, false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	snapshot, ok := c.snapshots[authIndex]
	return freshBenefitSnapshot(snapshot), ok
}

func (c *quotaCache) all() map[string]quotaSnapshot {
	if c == nil {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make(map[string]quotaSnapshot, len(c.snapshots))
	for key, value := range c.snapshots {
		out[key] = freshBenefitSnapshot(value)
	}
	return out
}

func (c *quotaCache) forget(authIndex string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.snapshots, authIndex)
}

// fetchQuotaSnapshot queries the statistics endpoint for one credential and
// updates the cache. It returns the normalised snapshot.
func fetchQuotaSnapshot(authIndex string, cred *credential, callbackIDs ...string) (quotaSnapshot, error) {
	var callbackID string
	if len(callbackIDs) > 0 {
		callbackID = callbackIDs[0]
	}
	if cred != nil && cred.valid() {
		refreshed, errRefresh := prepareCredentialForUse(authIndex, cred)
		if errRefresh != nil {
			errFetch := fmt.Errorf("credential expired and silent refresh failed: %w", errRefresh)
			quotas.put(quotaSnapshot{AuthIndex: authIndex, FetchedAt: time.Now(), Error: errFetch.Error()})
			return quotaSnapshot{}, errFetch
		}
		cred = refreshed
	}
	cfg := config()
	endpoint := strings.TrimRight(cfg.BaseURL, "/") + "/snap-manager/v1/statistics/plugin"

	headers := map[string]string{
		"Accept":      "application/json",
		"X-Language":  cfg.Language,
		"plugin-name": cfg.PluginName,
	}
	if cfg.PluginVersion != "" {
		headers["plugin-version"] = cfg.PluginVersion
	}

	if cred.valid() {
		signed, errSign := signRequest(http.MethodGet, endpoint, headers, nil, cred, cfg.SignHost)
		if errSign != nil {
			errFetch := fmt.Errorf("sign statistics request: %w", errSign)
			quotas.put(quotaSnapshot{AuthIndex: authIndex, FetchedAt: time.Now(), Error: errFetch.Error()})
			return quotaSnapshot{}, errFetch
		}
		headers = signed
	}

	response, errDo := hostHTTPDoContext(callbackID, http.MethodGet, endpoint, headers, nil)
	if errDo != nil {
		errFetch := fmt.Errorf("statistics request failed: %w", errDo)
		quotas.put(quotaSnapshot{AuthIndex: authIndex, FetchedAt: time.Now(), Error: errFetch.Error()})
		return quotaSnapshot{}, errFetch
	}
	if response.StatusCode != http.StatusOK {
		errFetch := fmt.Errorf("statistics request returned HTTP %d: %s", response.StatusCode, truncate(string(response.Body), 300))
		quotas.put(quotaSnapshot{AuthIndex: authIndex, FetchedAt: time.Now(), Error: errFetch.Error()})
		return quotaSnapshot{}, errFetch
	}

	snapshot, errParse := parseQuotaSnapshot(response.Body)
	if errParse != nil {
		errFetch := fmt.Errorf("statistics response could not be parsed: %w", errParse)
		quotas.put(quotaSnapshot{AuthIndex: authIndex, FetchedAt: time.Now(), Error: errFetch.Error()})
		return quotaSnapshot{}, errFetch
	}
	snapshot.AuthIndex = authIndex
	snapshot.FetchedAt = time.Now()
	if cred != nil {
		snapshot.Label = firstNonEmptyString(cred.UserName, cred.UserID)
	}
	// Optional benefit I/O belongs to its own cancellable management request.
	// Automatic quota refresh must never wait on that separate gateway.
	quotas.put(snapshot)
	snapshot, _ = quotas.get(authIndex)
	return snapshot, nil
}

// statisticsResponse models the upstream statistics document. Only the fields
// the extension consumes are declared.
type statisticsResponse struct {
	Show *statisticsShow `json:"show"`
	End  string          `json:"end_date"`
	Metr []struct {
		Name             string   `json:"name"`
		Value            *float64 `json:"value"`
		UsageTokenNum    int64    `json:"usage_token_num"`
		PackageTokenAmnt int64    `json:"package_token_amount"`
		CreditTotal      *float64 `json:"package_credit_amount"`
		CreditUsed       *float64 `json:"package_credit_used"`
		CreditRemaining  *float64 `json:"package_credit_remain"`
		Show             bool     `json:"show"`
	} `json:"metrics"`
	Package *struct {
		SpecCode      string `json:"spec_code"`
		PackageNameEN string `json:"package_name_en"`
		PackageNameCN string `json:"package_name_cn"`
		PackageURL    string `json:"package_url"`
		Features      []struct {
			Name   string `json:"name"`
			Enable bool   `json:"enable"`
		} `json:"features"`
	} `json:"package"`
}

type statisticsShow struct {
	Metrics bool `json:"metrics"`
	Package bool `json:"package"`
}

// parseQuotaSnapshot converts the upstream document into the normalised view.
func parseQuotaSnapshot(body []byte) (quotaSnapshot, error) {
	var parsed statisticsResponse
	if errUnmarshal := json.Unmarshal(body, &parsed); errUnmarshal != nil {
		return quotaSnapshot{}, errUnmarshal
	}
	snapshot := quotaSnapshot{
		ResetDate: parsed.End,
		Features:  map[string]bool{},
	}
	for _, metric := range parsed.Metr {
		if strings.TrimSpace(metric.Name) == "" || !metric.Show {
			// show:false is the upstream instruction not to display this row; the
			// retired message-count metric arrives that way with value -1.
			continue
		}
		meter := quotaMeter{
			Name:            metric.Name,
			Label:           quotaMeterLabels[metric.Name],
			UsedTokens:      metric.UsageTokenNum,
			AllowanceTokens: metric.PackageTokenAmnt,
			CreditTotal:     metric.CreditTotal, CreditUsed: metric.CreditUsed, CreditRemaining: metric.CreditRemaining,
		}
		if meter.Label == "" {
			meter.Label = metric.Name
		}
		if metric.Value != nil && *metric.Value >= 0 {
			value := *metric.Value
			meter.UsedPercent = &value
		}
		if meter.UsedPercent == nil && meter.UsedTokens == 0 && meter.AllowanceTokens == 0 && meter.CreditTotal == nil && meter.CreditRemaining == nil {
			continue // nothing to show
		}
		snapshot.Meters = append(snapshot.Meters, meter)
	}
	if parsed.Package != nil {
		snapshot.Plan = parsed.Package.SpecCode
		snapshot.PlanName = firstNonEmptyString(parsed.Package.PackageNameEN, parsed.Package.PackageNameCN)
		snapshot.PlanURL = parsed.Package.PackageURL
		for _, feature := range parsed.Package.Features {
			if feature.Name != "" {
				snapshot.Features[feature.Name] = feature.Enable
			}
		}
	}
	return snapshot, nil
}

// quotaProviderIdentifier is the provider key this quota provider serves. It
// matches the executor provider identifier so the host can associate quota
// lookups with this plugin's credentials.
func quotaProviderIdentifier() string { return providerID }

// quotaDescribe reports the provider keys and reset support.
func quotaDescribe() pluginapi.QuotaDescribeResponse {
	return pluginapi.QuotaDescribeResponse{
		SupportedProviders: []string{providerID},
		DisplayName:        "CodeArts",
		// The upstream quota is reset by the subscription cycle; the plugin
		// cannot force a reset, so it reports that honestly.
		SupportsReset: false,
	}
}

// quotaFetch answers a quota request from the cache, refreshing on demand when
// the cached entry is missing or stale.
func quotaFetch(request []byte) ([]byte, error) {
	var req struct {
		pluginapi.QuotaFetchRequest
		HostCallbackID string `json:"host_callback_id"`
	}
	if len(request) > 0 {
		if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
			return nil, fmt.Errorf("decode quota fetch request: %w", errUnmarshal)
		}
	}
	if provider := normalizeProvider(req.Provider); provider != "" && provider != providerID {
		return failEnvelope("unsupported_provider", "this quota provider only serves "+providerID, http.StatusBadRequest)
	}

	authIndex := strings.TrimSpace(req.AuthIndex)
	if authIndex == "" {
		authIndex = strings.TrimSpace(req.AuthID)
	}
	if authIndex == "" {
		return failEnvelope("invalid_request", "auth_index is required", http.StatusBadRequest)
	}

	snapshot, ok := quotas.get(authIndex)
	if !ok || time.Since(snapshot.FetchedAt) > 5*time.Minute {
		cred, errCred := credentialFromStorage(req.StorageJSON)
		if errCred != nil {
			return failEnvelope("invalid_credential", "stored credential could not be decoded: "+errCred.Error(), http.StatusUnauthorized)
		}
		if !cred.valid() {
			// Without a credential the upstream cannot be queried, but a cached
			// snapshot may still exist; serving it is better than failing when
			// the account is simply mid-refresh.
			if !ok {
				return failEnvelope("missing_credential", "no credential is available for this account", http.StatusUnauthorized)
			}
		} else {
			fresh, errFetch := fetchQuotaSnapshot(authIndex, cred, req.HostCallbackID)
			if errFetch != nil {
				if ok {
					// Serve the stale snapshot rather than an error; the error is
					// surfaced in the snapshot itself.
					fresh = snapshot
					fresh.Error = errFetch.Error()
				} else {
					return failEnvelope("upstream_error", errFetch.Error(), http.StatusBadGateway)
				}
			}
			snapshot = fresh
		}
	}

	return okEnvelope(toQuotaFetchResponse(snapshot))
}

// toQuotaFetchResponse maps the snapshot onto the host's normalised quota shape
// so management clients can render a standard progress panel.
func toQuotaFetchResponse(snapshot quotaSnapshot) pluginapi.QuotaFetchResponse {
	response := pluginapi.QuotaFetchResponse{}

	if snapshot.Plan != "" || snapshot.PlanName != "" {
		response.Subscription = &pluginapi.QuotaSubscription{
			Plan:     snapshot.Plan,
			TierName: snapshot.PlanName,
			TierID:   snapshot.Plan,
		}
	}

	var buckets []pluginapi.QuotaBucket
	// The upstream reports consumption while the host models a "remaining
	// fraction". A meter without a ratio of its own is skipped rather than
	// reported as a full bucket.
	for _, meter := range snapshot.Meters {
		fraction, ok := meter.remainingFraction()
		if !ok {
			continue
		}
		buckets = append(buckets, pluginapi.QuotaBucket{
			Window:            "quota-period",
			RemainingFraction: fraction,
			ResetTime:         snapshot.ResetDate,
			Description:       meter.Describe(),
		})
	}
	if snapshot.Benefit != nil && snapshot.Benefit.DailyTokenLimit > 0 {
		used := snapshot.Benefit.DailyPercent()
		buckets = append(buckets, pluginapi.QuotaBucket{
			Window:            "1d",
			RemainingFraction: usedPercentToRemaining(used),
			Description: fmt.Sprintf("Benefit tokens used: %.1f%% (%d of %d daily tokens)",
				used, snapshot.Benefit.DailyTokensUsed, snapshot.Benefit.DailyTokenLimit),
		})
	}
	if len(buckets) > 0 {
		response.Groups = []pluginapi.QuotaGroup{
			{DisplayName: "CodeArts Doer", Buckets: buckets},
		}
	}

	response.Summary = []pluginapi.QuotaMetric{}
	for _, meter := range snapshot.Meters {
		if meter.UsedPercent != nil {
			response.Summary = append(response.Summary, pluginapi.QuotaMetric{
				Key: strings.ToLower(meter.Name) + "_used_percent",
				// The host shows the key when no label fits, so keep the metric name.
				Label: meter.Name + " used (%)", Value: *meter.UsedPercent, Unit: "percent",
			})
		}
		if meter.UsedTokens > 0 || meter.AllowanceTokens > 0 {
			response.Summary = append(response.Summary, pluginapi.QuotaMetric{
				Key: strings.ToLower(meter.Name) + "_used_tokens", Label: meter.Name + " used (tokens)",
				Value: float64(meter.UsedTokens), Unit: "tokens",
			})
		}
	}
	// Only a capped pool gets summary metrics: a "limit 0" reads as exhausted to
	// the host, while it actually means this account has no daily cap. The panel
	// still shows the raw counters for that case from /accounts.
	if snapshot.Benefit != nil && snapshot.Benefit.DailyTokenLimit > 0 {
		response.Summary = append(response.Summary,
			pluginapi.QuotaMetric{Key: "benefit_daily_tokens_used", Label: "Benefit tokens used today", Value: float64(snapshot.Benefit.DailyTokensUsed), Unit: "tokens"},
			pluginapi.QuotaMetric{Key: "benefit_daily_token_limit", Label: "Benefit daily token limit", Value: float64(snapshot.Benefit.DailyTokenLimit), Unit: "tokens"},
		)
	}
	if snapshot.ResetDate != "" {
		response.Summary = append(response.Summary, pluginapi.QuotaMetric{
			Key: "quota_reset_date", Label: "Quota reset date", Value: 0,
		})
	}
	return response
}

// usedPercentToRemaining converts a consumed percentage to a remaining
// fraction in [0, 1]. Upstream negative values mean "not reported".
func usedPercentToRemaining(usedPercent float64) float64 {
	if usedPercent < 0 {
		return 1
	}
	if usedPercent > 100 {
		usedPercent = 100
	}
	return (100 - usedPercent) / 100
}

// quotaReset reports that a reset is not possible upstream.
func quotaReset() ([]byte, error) {
	return okEnvelope(pluginapi.QuotaResetResponse{
		Success: false,
		Message: "CodeArts Doer quota is tied to the subscription cycle and cannot be reset on demand",
	})
}
