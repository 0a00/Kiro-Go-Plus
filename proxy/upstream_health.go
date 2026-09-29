package proxy

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"kiro-go/config"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

type circuitRuntimeState struct {
	label               string
	consecutiveFailures int
	openCount           int
	cooldownUntil       time.Time
	probeInFlight       bool
	inFlight            int
	successes           uint64
	failures            uint64
	ewmaLatencyMs       float64
	lastError           string
	lastSuccessAt       time.Time
	lastFailureAt       time.Time
	lastAccess          time.Time
}

const maxCircuitRuntimeEntries = 4096

func endpointCircuitScope(endpoint, host, model, proxyURL string) string {
	proxyURL = strings.TrimSpace(proxyURL)
	if strings.EqualFold(proxyURL, "direct") {
		proxyURL = ""
	}
	// Do not retain credentials or caller-controlled model names in map keys.
	scope := sha256.Sum256([]byte(model + "\x00" + proxyURL))
	return endpoint + "|" + host + "|" + fmt.Sprintf("%x", scope[:16])
}

type upstreamHealthRegistry struct {
	mu        sync.Mutex
	endpoints map[string]circuitRuntimeState
	proxies   map[string]circuitRuntimeState
	now       func() time.Time
}

var sharedUpstreamHealth = newUpstreamHealthRegistry()

func newUpstreamHealthRegistry() *upstreamHealthRegistry {
	return &upstreamHealthRegistry{
		endpoints: make(map[string]circuitRuntimeState),
		proxies:   make(map[string]circuitRuntimeState),
		now:       time.Now,
	}
}

func (r *upstreamHealthRegistry) beginEndpoint(key, label string) bool {
	return r.begin(r.endpoints, key, label)
}

func (r *upstreamHealthRegistry) endpointRetryAfter(key string) time.Duration {
	if r == nil || strings.TrimSpace(key) == "" {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	state := r.endpoints[key]
	now := r.now()
	if state.cooldownUntil.After(now) {
		return state.cooldownUntil.Sub(now)
	}
	if state.probeInFlight {
		return time.Second
	}
	return 0
}

func (r *upstreamHealthRegistry) beginProxy(key, label string) bool {
	if sanitized, _ := sanitizedProxyURL(label); sanitized != "" {
		label = sanitized
	}
	return r.begin(r.proxies, key, label)
}

func (r *upstreamHealthRegistry) begin(states map[string]circuitRuntimeState, key, label string) bool {
	if r == nil || strings.TrimSpace(key) == "" {
		return true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	if _, exists := states[key]; !exists && len(states) >= maxCircuitRuntimeEntries {
		// Preserve cooling/probing entries. Evict only the least recently used
		// closed entry, or fail closed when all slots are protecting a route.
		oldestKey := ""
		var oldest time.Time
		for candidate, state := range states {
			if state.probeInFlight || state.inFlight > 0 {
				continue
			}
			if !state.cooldownUntil.IsZero() && (state.cooldownUntil.After(now) || now.Sub(state.lastAccess) < time.Hour) {
				continue
			}
			if oldestKey == "" || state.lastAccess.Before(oldest) {
				oldestKey, oldest = candidate, state.lastAccess
			}
		}
		if oldestKey == "" {
			return false
		}
		delete(states, oldestKey)
	}
	state := states[key]
	state.label = label
	state.lastAccess = now
	if state.cooldownUntil.After(now) {
		states[key] = state
		return false
	}
	if !state.cooldownUntil.IsZero() {
		if state.probeInFlight {
			states[key] = state
			return false
		}
		state.probeInFlight = true
	}
	state.inFlight++
	states[key] = state
	return true
}

func (r *upstreamHealthRegistry) endpointSuccess(key string, latency time.Duration) {
	r.success(r.endpoints, key, latency)
}

func (r *upstreamHealthRegistry) proxySuccess(key string, latency time.Duration) {
	r.success(r.proxies, key, latency)
}

func (r *upstreamHealthRegistry) success(states map[string]circuitRuntimeState, key string, latency time.Duration) {
	if r == nil || strings.TrimSpace(key) == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	state, exists := states[key]
	if !exists {
		return
	}
	state.successes++
	state.inFlight = max(0, state.inFlight-1)
	state.consecutiveFailures = 0
	state.openCount = 0
	state.cooldownUntil = time.Time{}
	state.probeInFlight = false
	state.lastError = ""
	state.lastSuccessAt = r.now()
	state.lastAccess = state.lastSuccessAt
	state.ewmaLatencyMs = updateLatencyEWMA(state.ewmaLatencyMs, latency)
	states[key] = state
}

func (r *upstreamHealthRegistry) endpointFailure(key string, err error, latency time.Duration) {
	cfg := config.GetRetryConfig()
	r.failure(r.endpoints, key, err, latency, cfg.EndpointFailureThreshold, time.Duration(cfg.EndpointCircuitCooldownSeconds)*time.Second)
}

func (r *upstreamHealthRegistry) proxyFailure(key string, err error, latency time.Duration) {
	cfg := config.GetRetryConfig()
	r.failure(r.proxies, key, err, latency, cfg.ProxyFailureThreshold, time.Duration(cfg.ProxyCircuitCooldownSeconds)*time.Second)
}

func (r *upstreamHealthRegistry) failure(states map[string]circuitRuntimeState, key string, err error, latency time.Duration, threshold int, baseCooldown time.Duration) {
	if r == nil || strings.TrimSpace(key) == "" {
		return
	}
	if threshold < 1 {
		threshold = 1
	}
	if baseCooldown < time.Second {
		baseCooldown = time.Second
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	state, exists := states[key]
	if !exists {
		return
	}
	wasProbe := state.probeInFlight
	state.inFlight = max(0, state.inFlight-1)
	state.probeInFlight = false
	state.failures++
	state.consecutiveFailures++
	state.lastFailureAt = now
	state.lastAccess = now
	state.ewmaLatencyMs = updateLatencyEWMA(state.ewmaLatencyMs, latency)
	if err != nil {
		state.lastError = truncateCircuitError(err.Error())
	}
	if wasProbe || (!state.cooldownUntil.After(now) && state.consecutiveFailures >= threshold) {
		state.openCount++
		multiplier := 1 << circuitMinInt(state.openCount-1, 4)
		cooldown := time.Duration(multiplier) * baseCooldown
		if cooldown > 15*time.Minute {
			cooldown = 15 * time.Minute
		}
		state.cooldownUntil = now.Add(cooldown)
		state.consecutiveFailures = 0
	}
	states[key] = state
}

func (r *upstreamHealthRegistry) releaseEndpoint(key string) {
	r.release(r.endpoints, key)
}

func (r *upstreamHealthRegistry) releaseProxy(key string) {
	r.release(r.proxies, key)
}

func (r *upstreamHealthRegistry) release(states map[string]circuitRuntimeState, key string) {
	if r == nil || strings.TrimSpace(key) == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	state, exists := states[key]
	if !exists {
		return
	}
	state.probeInFlight = false
	state.inFlight = max(0, state.inFlight-1)
	states[key] = state
}

func (r *upstreamHealthRegistry) Snapshot() map[string]interface{} {
	if r == nil {
		return map[string]interface{}{"endpoints": []interface{}{}, "proxies": []interface{}{}}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	return map[string]interface{}{
		"endpoints": circuitStateViews(r.endpoints, now),
		"proxies":   circuitStateViews(r.proxies, now),
	}
}

func circuitStateViews(states map[string]circuitRuntimeState, now time.Time) []map[string]interface{} {
	views := make([]map[string]interface{}, 0, len(states))
	for key, state := range states {
		status := "closed"
		if state.cooldownUntil.After(now) {
			status = "open"
		} else if !state.cooldownUntil.IsZero() || state.probeInFlight {
			status = "half_open"
		}
		scope := sha256.Sum256([]byte(key))
		views = append(views, map[string]interface{}{
			"scope":               fmt.Sprintf("%x", scope[:8]),
			"target":              state.label,
			"state":               status,
			"successes":           state.successes,
			"failures":            state.failures,
			"consecutiveFailures": state.consecutiveFailures,
			"cooldownUntil":       unixOrZeroTime(state.cooldownUntil),
			"latencyEwmaMs":       state.ewmaLatencyMs,
			"lastError":           state.lastError,
			"lastSuccessAt":       unixOrZeroTime(state.lastSuccessAt),
			"lastFailureAt":       unixOrZeroTime(state.lastFailureAt),
		})
	}
	sort.Slice(views, func(i, j int) bool { return fmt.Sprint(views[i]["target"]) < fmt.Sprint(views[j]["target"]) })
	return views
}

func updateLatencyEWMA(current float64, latency time.Duration) float64 {
	if latency <= 0 {
		return current
	}
	value := float64(latency.Microseconds()) / 1000
	if current == 0 {
		return value
	}
	return current*0.8 + value*0.2
}

func truncateCircuitError(message string) string {
	message = redactDiagnosticText(message)
	if len(message) > 300 {
		return message[:300]
	}
	return message
}

func unixOrZeroTime(value time.Time) int64 {
	if value.IsZero() {
		return 0
	}
	return value.Unix()
}

func circuitMinInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func circuitEligibleFailure(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	upstreamErr, ok := asUpstreamError(err)
	if ok {
		// Generic 500s and generation faults can be account/prompt specific.
		// Keep their account-route cooldown, not a shared service outage.
		if upstreamErr.StatusCode != 0 {
			return (upstreamErr.Kind == UpstreamErrorTransient || upstreamErr.Kind == UpstreamErrorFirstTokenTimeout) &&
				(upstreamErr.StatusCode == http.StatusBadGateway || upstreamErr.StatusCode == http.StatusServiceUnavailable || upstreamErr.StatusCode == http.StatusGatewayTimeout)
		}
		if upstreamErr.Kind != UpstreamErrorTransient && upstreamErr.Kind != UpstreamErrorUnknown {
			return false
		}
	}
	var netErr net.Error
	return errors.As(err, &netErr)
}
