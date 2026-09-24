package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// This file implements the usage observer and the thinking applier.
//
// usage_plugin: the host reports one terminal record per completed request. The
// plugin keeps a bounded in-memory rollup so the panel can show real per-account
// traffic without a second upstream call. Records are the host's own, so the
// numbers are exact rather than estimated.
//
// thinking_applier: converts the validated thinking configuration into the
// request fields CodeArts Doer accepts.

// ---------------------------------------------------------------------------
// usage
// ---------------------------------------------------------------------------

// usageSample is one recent request, kept for the panel.
type usageSample struct {
	At              time.Time `json:"at"`
	Model           string    `json:"model"`
	AuthID          string    `json:"auth_id,omitempty"`
	InputTokens     int64     `json:"input_tokens"`
	OutputTokens    int64     `json:"output_tokens"`
	ReasoningTokens int64     `json:"reasoning_tokens,omitempty"`
	CachedTokens    int64     `json:"cached_tokens,omitempty"`
	TotalTokens     int64     `json:"total_tokens"`
	LatencyMS       int64     `json:"latency_ms"`
	TTFTMS          int64     `json:"ttft_ms,omitempty"`
	Failed          bool      `json:"failed"`
	Error           string    `json:"error,omitempty"`
	ReasoningEffort string    `json:"reasoning_effort,omitempty"`
}

// usageRollup is the bounded aggregate the panel renders.
type usageRollup struct {
	mu sync.Mutex

	Requests    int64
	Failures    int64
	InputTotal  int64
	OutputTotal int64
	ReasonTotal int64
	CacheTotal  int64

	// ByModel and ByAuth aggregate token totals.
	ByModel map[string]*usageBucket
	ByAuth  map[string]*usageBucket

	// Recent is a ring of the most recent samples, newest last.
	Recent []usageSample
	// latency/total track averages with integer math.
	LatencySumMS int64
	TTFTSumMS    int64
	TTFTCount    int64
}

type usageBucket struct {
	Requests     int64 `json:"requests"`
	Failures     int64 `json:"failures"`
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	TotalTokens  int64 `json:"total_tokens"`
}

// maxRecentSamples bounds memory: the plugin runs inside the gateway process, so
// an unbounded history would grow for the lifetime of the deployment.
const maxRecentSamples = 200

var usage = &usageRollup{
	ByModel: map[string]*usageBucket{},
	ByAuth:  map[string]*usageBucket{},
}

// usageHandle records one terminal usage record from the host.
//
// The host calls this for every completed request, so it must be cheap, must
// never block, and must not fail the request: errors here are intentionally
// swallowed after logging.
func usageHandle(request []byte) ([]byte, error) {
	var record pluginapi.UsageRecord
	if len(request) > 0 {
		if errUnmarshal := json.Unmarshal(request, &record); errUnmarshal != nil {
			logWarn("could not decode a usage record", map[string]any{"error": errUnmarshal.Error()})
			return okEnvelope(map[string]any{})
		}
	}
	if record.Provider != "" && normalizeProvider(record.Provider) != providerID {
		return okEnvelope(map[string]any{})
	}
	usage.observe(record)
	return okEnvelope(map[string]any{})
}

// observe folds one record into the rollup.
func (u *usageRollup) observe(record pluginapi.UsageRecord) {
	detail := record.Detail
	total := detail.TotalTokens
	if total == 0 {
		total = detail.InputTokens + detail.OutputTokens
	}

	u.mu.Lock()
	defer u.mu.Unlock()

	u.Requests++
	if record.Failed {
		u.Failures++
	}
	u.InputTotal += detail.InputTokens
	u.OutputTotal += detail.OutputTokens
	u.ReasonTotal += detail.ReasoningTokens
	u.CacheTotal += detail.CachedTokens
	if record.Latency > 0 {
		u.LatencySumMS += record.Latency.Milliseconds()
	}
	if record.TTFT > 0 {
		u.TTFTSumMS += record.TTFT.Milliseconds()
		u.TTFTCount++
	}

	if model := strings.TrimSpace(firstNonEmptyString(record.Model, record.Alias)); model != "" {
		bucket := u.ByModel[model]
		if bucket == nil {
			bucket = &usageBucket{}
			u.ByModel[model] = bucket
		}
		accumulate(bucket, record, detail.InputTokens, detail.OutputTokens, total)
	}
	if authID := strings.TrimSpace(record.AuthID); authID != "" {
		bucket := u.ByAuth[authID]
		if bucket == nil {
			bucket = &usageBucket{}
			u.ByAuth[authID] = bucket
		}
		accumulate(bucket, record, detail.InputTokens, detail.OutputTokens, total)
	}

	// UsageRecord carries RequestedAt plus a latency, not an absolute completion
	// time; reconstruct it so recent samples sort sensibly.
	at := record.RequestedAt
	if at.IsZero() {
		at = time.Now()
	} else if record.Latency > 0 {
		at = at.Add(record.Latency)
	}
	sample := usageSample{
		At:              at,
		Model:           record.Model,
		AuthID:          record.AuthID,
		InputTokens:     detail.InputTokens,
		OutputTokens:    detail.OutputTokens,
		ReasoningTokens: detail.ReasoningTokens,
		CachedTokens:    detail.CachedTokens,
		TotalTokens:     total,
		LatencyMS:       record.Latency.Milliseconds(),
		TTFTMS:          record.TTFT.Milliseconds(),
		Failed:          record.Failed,
		ReasoningEffort: record.ReasoningEffort,
	}
	if record.Failed {
		// UsageFailure carries a status code and body, not a message field.
		sample.Error = strings.TrimSpace(record.Failure.Body)
		if sample.Error == "" && record.Failure.StatusCode != 0 {
			sample.Error = fmt.Sprintf("HTTP %d", record.Failure.StatusCode)
		}
		sample.Error = truncate(sample.Error, 300)
	}
	u.Recent = append(u.Recent, sample)
	if len(u.Recent) > maxRecentSamples {
		u.Recent = append([]usageSample(nil), u.Recent[len(u.Recent)-maxRecentSamples:]...)
	}
}

func accumulate(bucket *usageBucket, record pluginapi.UsageRecord, input, output, total int64) {
	bucket.Requests++
	if record.Failed {
		bucket.Failures++
	}
	bucket.InputTokens += input
	bucket.OutputTokens += output
	bucket.TotalTokens += total
}

// snapshot renders the rollup for the management API and the panel.
func (u *usageRollup) snapshot() map[string]any {
	u.mu.Lock()
	defer u.mu.Unlock()

	byModel := make(map[string]usageBucket, len(u.ByModel))
	for model, bucket := range u.ByModel {
		byModel[model] = *bucket
	}
	byAuth := make(map[string]usageBucket, len(u.ByAuth))
	for authID, bucket := range u.ByAuth {
		byAuth[authID] = *bucket
	}

	averageLatency := int64(0)
	if u.Requests > 0 {
		averageLatency = u.LatencySumMS / u.Requests
	}
	averageTTFT := int64(0)
	if u.TTFTCount > 0 {
		averageTTFT = u.TTFTSumMS / u.TTFTCount
	}

	recent := append([]usageSample(nil), u.Recent...)
	sort.SliceStable(recent, func(i, j int) bool { return recent[i].At.After(recent[j].At) })

	return map[string]any{
		"requests":         u.Requests,
		"failures":         u.Failures,
		"input_tokens":     u.InputTotal,
		"output_tokens":    u.OutputTotal,
		"reasoning_tokens": u.ReasonTotal,
		"cached_tokens":    u.CacheTotal,
		"total_tokens":     u.InputTotal + u.OutputTotal,
		"avg_latency_ms":   averageLatency,
		"avg_ttft_ms":      averageTTFT,
		"by_model":         byModel,
		"by_auth":          byAuth,
		"recent":           recent,
		"source":           "host usage records (exact, not estimated)",
		"counters_scope":   "since process start; not persisted",
	}
}

// ---------------------------------------------------------------------------
// thinking
// ---------------------------------------------------------------------------

// thinkingIdentifier is the provider key this thinking applier serves.
func thinkingIdentifier() string { return providerID }

// thinkingApply rewrites the request body with the validated reasoning settings.
//
// CodeArts Doer accepts an OpenAI-style `reasoning_effort` on the agent-mode
// endpoint; the native endpoint has no documented reasoning controls, so the
// fields are only injected for agent mode and are otherwise left untouched
// rather than being invented.
func thinkingApply(request []byte) ([]byte, error) {
	var req pluginapi.ThinkingApplyRequest
	if len(request) > 0 {
		if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
			return nil, fmt.Errorf("decode thinking request: %w", errUnmarshal)
		}
	}
	cfg := config()
	if cfg.APIMode != "agent" {
		// Nothing documented to set on the native endpoint: return the body
		// unchanged so the host's validated config is not silently dropped into a
		// field upstream would ignore or reject.
		return okEnvelope(pluginapi.PayloadResponse{Body: req.Body})
	}

	payload := map[string]any{}
	if len(req.Body) > 0 {
		if errUnmarshal := json.Unmarshal(req.Body, &payload); errUnmarshal != nil {
			// A body we cannot parse is returned untouched: mangling it would be
			// worse than not applying the hint.
			return okEnvelope(pluginapi.PayloadResponse{Body: req.Body})
		}
	}

	config := req.Config
	effort := normalizeReasoningEffort(config)
	if effort != "" {
		payload["reasoning_effort"] = effort
	}
	// The upstream rejects `temperature` alongside reasoning on some models, so
	// it is only dropped when an explicit budget was requested.
	if config.Budget > 0 {
		if _, ok := payload["max_tokens"]; !ok {
			payload["max_tokens"] = config.Budget
		}
	}

	body, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		return okEnvelope(pluginapi.PayloadResponse{Body: req.Body})
	}
	logInfo("applied thinking configuration", map[string]any{
		"model":  req.Model.ID,
		"mode":   config.Mode,
		"level":  config.Level,
		"budget": config.Budget,
		"effort": effort,
	})
	return okEnvelope(pluginapi.PayloadResponse{Body: body})
}

// normalizeReasoningEffort maps the host's thinking configuration onto the
// effort values the upstream accepts. An empty result means "do not set it".
func normalizeReasoningEffort(config pluginapi.ThinkingConfig) string {
	// An explicit level that is already an accepted value wins.
	switch strings.ToLower(strings.TrimSpace(config.Level)) {
	case "low", "medium", "high":
		return strings.ToLower(strings.TrimSpace(config.Level))
	case "minimal":
		return "low"
	}

	switch strings.ToLower(strings.TrimSpace(config.Mode)) {
	case "off", "none", "disabled":
		// The upstream agent endpoint has no "off" value; omitting the field is
		// the documented way to leave the model default in place.
		return ""
	case "low":
		return "low"
	case "medium", "auto", "dynamic":
		return "medium"
	case "high":
		return "high"
	}

	// Fall back to the budget when only a token count was provided.
	switch {
	case config.Budget >= 16000:
		return "high"
	case config.Budget > 0:
		return "medium"
	default:
		return ""
	}
}
