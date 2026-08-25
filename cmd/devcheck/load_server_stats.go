package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// loadServerStats is intentionally limited to customer-visible counters. It
// gives the load report a server-side completion cross-check without exposing
// account identifiers or requiring admin credentials.
type loadServerStats struct {
	requests int64
	tokens   int64
	valid    bool
}

type loadServerStatsPayload struct {
	RequestsCount *int64 `json:"requestsCount"`
	TotalRequests *int64 `json:"totalRequests"`
	TokensUsed    *int64 `json:"tokensUsed"`
	TotalTokens   *int64 `json:"totalTokens"`
}

func (r *runner) fetchLoadServerStats(parent context.Context) (loadServerStats, error) {
	if r == nil || r.apiKey == "" {
		return loadServerStats{}, fmt.Errorf("customer API key is unavailable")
	}
	if parent == nil {
		parent = context.Background()
	}
	timeout := r.opts.timeout
	if timeout <= 0 || timeout > 5*time.Second {
		timeout = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	response := r.get(ctx, "/api/stats", true)
	if response.err != nil {
		return loadServerStats{}, response.err
	}
	if response.statusCode < 200 || response.statusCode >= 300 {
		return loadServerStats{}, fmt.Errorf("HTTP %d", response.statusCode)
	}
	var payload loadServerStatsPayload
	if err := json.Unmarshal(response.body, &payload); err != nil {
		return loadServerStats{}, err
	}
	return normalizeLoadServerStatsPayload(payload)
}

func normalizeLoadServerStatsPayload(payload loadServerStatsPayload) (loadServerStats, error) {
	requestsValue := payload.RequestsCount
	if requestsValue == nil {
		requestsValue = payload.TotalRequests
	}
	tokensValue := payload.TokensUsed
	if tokensValue == nil {
		tokensValue = payload.TotalTokens
	}
	if requestsValue == nil || tokensValue == nil {
		return loadServerStats{}, fmt.Errorf("stats response omitted request or token counters")
	}
	if *requestsValue < 0 || *tokensValue < 0 {
		return loadServerStats{}, fmt.Errorf("stats response contained a negative counter")
	}
	return loadServerStats{requests: *requestsValue, tokens: *tokensValue, valid: true}, nil
}

func loadStatsContext(parent context.Context) context.Context {
	if parent == nil {
		return context.Background()
	}
	return parent
}

func loadServerStatsDelta(before, after loadServerStats) (loadServerStats, bool) {
	if !before.valid || !after.valid || after.requests < before.requests || after.tokens < before.tokens {
		return loadServerStats{}, true
	}
	return loadServerStats{
		requests: after.requests - before.requests,
		tokens:   after.tokens - before.tokens,
		valid:    true,
	}, false
}
