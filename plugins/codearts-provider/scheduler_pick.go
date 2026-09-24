package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// This file implements the scheduler capability.
//
// CLIProxyAPI normally picks a credential itself (fill-first or round-robin).
// A plugin scheduler runs first and may:
//   - pick a specific credential by ID,
//   - delegate back to a built-in strategy,
//   - or leave the pick unhandled so lower-priority schedulers decide.
//
// CodeArts Doer has no published per-credential quota, so the useful policy here
// is a local round-robin/least-recently-used rotation across the configured
// accounts, plus the ability to pin a preferred account. That spreads requests
// instead of hammering whichever credential the host happens to prefer, which
// matters because the upstream applies per-account rate limits.

// schedulerStrategy selects how the plugin chooses among candidates.
type schedulerStrategy string

const (
	// strategyRoundRobin rotates across candidates, skipping the last pick.
	strategyRoundRobin schedulerStrategy = "round-robin"
	// strategyPreferred always prefers the configured account when it is a
	// candidate, falling back to round-robin.
	strategyPreferred schedulerStrategy = "preferred"
	// strategyHost delegates every pick to the host's built-in scheduler.
	strategyHost schedulerStrategy = "host"
)

// pickState tracks rotation so consecutive picks do not repeat an account.
type pickState struct {
	mu   sync.Mutex
	last string
	// cursor is the position after the most recent pick. Advancing it (rather
	// than scanning for "the first ID that differs from the last one") is what
	// makes the rotation cover every candidate instead of oscillating between
	// the first two.
	cursor int
}

var picker = &pickState{}

// schedulerIdentifier is the provider key this scheduler serves.
func schedulerIdentifier() string { return providerID }

// schedulerPick chooses a credential for one request.
//
// It returns Handled=false when the plugin has no opinion, which lets the host's
// built-in scheduler proceed. That is the safe default: a plugin scheduler that
// handles everything but picks badly quietly degrades the whole gateway.
func schedulerPick(request []byte) ([]byte, error) {
	var req pluginapi.SchedulerPickRequest
	if len(request) > 0 {
		if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
			return nil, fmt.Errorf("decode scheduler request: %w", errUnmarshal)
		}
	}
	cfg := config()

	// Only act on this plugin's own provider. Another provider's request is none
	// of our business and must stay unhandled.
	provider := normalizeProvider(req.Provider)
	if provider != "" && provider != providerID {
		return okEnvelope(pluginapi.SchedulerPickResponse{Handled: false})
	}
	if len(req.Providers) > 0 {
		matches := false
		for _, candidate := range req.Providers {
			if normalizeProvider(candidate) == providerID {
				matches = true
				break
			}
		}
		if !matches {
			return okEnvelope(pluginapi.SchedulerPickResponse{Handled: false})
		}
	}

	strategy := cfg.normalizedSchedulerStrategy()
	if strategy == strategyHost {
		// Explicitly defer everything to the built-in scheduler. Reporting the
		// delegate keeps the intent visible in host logs.
		return okEnvelope(pluginapi.SchedulerPickResponse{
			Handled:         true,
			DelegateBuiltin: cfg.Scheduler.Delegate,
		})
	}

	candidates := usableCandidates(req.Candidates)
	available := make([]pluginapi.SchedulerAuthCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		if candidateSessionAvailable(cfg, candidate) {
			available = append(available, candidate)
		}
	}
	// Picking is advisory: concurrent requests can race. The executor reserves
	// atomically. If all are full it returns a request-scoped 409, not an unknown auth.
	if len(available) > 0 {
		candidates = available
	}
	if len(candidates) == 0 {
		// No usable credential of ours: stay unhandled so the host applies its own
		// error handling rather than us inventing one.
		return okEnvelope(pluginapi.SchedulerPickResponse{Handled: false})
	}

	if strategy == strategyPreferred && cfg.Scheduler.PreferredAuthID != "" {
		for _, candidate := range candidates {
			if candidate.ID == cfg.Scheduler.PreferredAuthID {
				picker.remember(candidate.ID)
				return okEnvelope(pluginapi.SchedulerPickResponse{Handled: true, AuthID: candidate.ID})
			}
		}
		// The preferred account is not currently a candidate (disabled, cooling
		// down, or filtered by the host), so fall through to rotation.
	}

	picked := picker.rotate(candidates)
	return okEnvelope(pluginapi.SchedulerPickResponse{Handled: true, AuthID: picked})
}

// usableCandidates filters out credentials the host already marked unusable.
func usableCandidates(candidates []pluginapi.SchedulerAuthCandidate) []pluginapi.SchedulerAuthCandidate {
	out := make([]pluginapi.SchedulerAuthCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		if strings.TrimSpace(candidate.ID) == "" {
			continue
		}
		// A credential the host reports as unavailable would fail if chosen.
		if strings.EqualFold(strings.TrimSpace(candidate.Status), "unavailable") {
			continue
		}
		out = append(out, candidate)
	}
	return out
}

// remember records the most recent pick. It deliberately does not move the
// rotation cursor: a preferred pick is an override, and the next rotation should
// resume where it left off rather than skip a candidate.
func (p *pickState) remember(authID string) {
	p.mu.Lock()
	p.last = authID
	p.mu.Unlock()
}

// rotate returns the next candidate in sequence, advancing a cursor so every
// candidate is used before any is repeated.
func (p *pickState) rotate(candidates []pluginapi.SchedulerAuthCandidate) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(candidates) == 0 {
		return ""
	}
	index := p.cursor % len(candidates)
	p.cursor = (index + 1) % len(candidates)
	p.last = candidates[index].ID
	return p.last
}

// normalizedSchedulerStrategy resolves the configured strategy, defaulting to
// round-robin because it is the behaviour that helps most with per-account rate
// limits.
func (c *Config) normalizedSchedulerStrategy() schedulerStrategy {
	switch strings.ToLower(strings.TrimSpace(string(c.Scheduler.Strategy))) {
	case string(strategyHost):
		return strategyHost
	case string(strategyPreferred):
		return strategyPreferred
	case string(strategyRoundRobin), "":
		return strategyRoundRobin
	default:
		// An unknown value falls back to the safe default rather than silently
		// disabling rotation.
		logWarn("unknown scheduler strategy, falling back to round-robin", map[string]any{
			"configured": c.Scheduler.Strategy,
		})
		return strategyRoundRobin
	}
}

// lastPick reports the most recent scheduler decision, for the panel.
func lastPick() string {
	picker.mu.Lock()
	defer picker.mu.Unlock()
	return picker.last
}
