package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const maxLoadCorrelationLogs = 500

type loadCorrelationLog struct {
	RequestID          string `json:"requestId"`
	Endpoint           string `json:"endpoint"`
	AccountSelectionMs int64  `json:"accountSelectionMs"`
	AccountAttempts    int    `json:"accountAttempts"`
	RouteAffinityHit   bool   `json:"routeAffinityHit"`
	CacheStatus        string `json:"cacheStatus"`
	ToolUseCount       int    `json:"toolUseCount"`
}

func safeLoadEndpointClass(endpoint string) string {
	endpoint = strings.ToLower(strings.TrimSpace(endpoint))
	switch {
	case strings.Contains(endpoint, "runtime"):
		return "runtime"
	case strings.Contains(endpoint, "codewhisperer"):
		return "codewhisperer"
	case strings.Contains(endpoint, "amazonq") || strings.Contains(endpoint, "amazon q"):
		return "amazonq"
	case strings.Contains(endpoint, "kiro"):
		return "kiro"
	default:
		return ""
	}
}

func normalizeLoadCorrelationLog(entry loadCorrelationLog) (loadCorrelationLog, bool) {
	entry.RequestID = strings.TrimSpace(entry.RequestID)
	if entry.RequestID == "" {
		return loadCorrelationLog{}, false
	}
	entry.Endpoint = safeLoadEndpointClass(entry.Endpoint)
	entry.AccountSelectionMs = max(entry.AccountSelectionMs, 0)
	entry.AccountSelectionMs = min(entry.AccountSelectionMs, int64((24*time.Hour)/time.Millisecond))
	entry.AccountAttempts = min(max(entry.AccountAttempts, 0), 10000)
	entry.ToolUseCount = min(max(entry.ToolUseCount, 0), 10000)
	switch strings.ToLower(strings.TrimSpace(entry.CacheStatus)) {
	case "hit", "partial_hit", "miss", "create", "created":
		entry.CacheStatus = strings.ToLower(strings.TrimSpace(entry.CacheStatus))
	default:
		entry.CacheStatus = ""
	}
	return entry, true
}

func (r *runner) correlateLoadSamples(parent context.Context, samples []loadSample) {
	indexes := make(map[string][]int)
	for index := range samples {
		if samples[index].requestID != "" {
			indexes[samples[index].requestID] = append(indexes[samples[index].requestID], index)
		}
	}
	if len(indexes) == 0 {
		return
	}
	if parent == nil {
		parent = context.Background()
	}
	if parent.Err() != nil {
		return
	}
	limit := min(maxLoadCorrelationLogs, max(100, len(indexes)))
	ctx, cancel := r.scenarioContext(parent)
	defer cancel()
	response := r.get(ctx, fmt.Sprintf("/api/logs?limit=%d", limit), true)
	if !validJSONResponse(response) {
		return
	}
	var payload struct {
		Logs []loadCorrelationLog `json:"logs"`
	}
	if json.Unmarshal(response.body, &payload) != nil {
		return
	}
	for _, rawEntry := range payload.Logs {
		entry, ok := normalizeLoadCorrelationLog(rawEntry)
		if !ok {
			continue
		}
		for _, index := range indexes[entry.RequestID] {
			sample := &samples[index]
			if sample.correlated {
				continue
			}
			sample.endpoint = entry.Endpoint
			sample.selectionMS = entry.AccountSelectionMs
			sample.accountAttempts = entry.AccountAttempts
			sample.affinityHit = entry.RouteAffinityHit
			sample.cacheStatus = entry.CacheStatus
			sample.toolUses = entry.ToolUseCount
			sample.correlated = true
		}
	}
}
