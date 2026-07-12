package proxy

import (
	"fmt"
	"kiro-go/config"
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
	successes           uint64
	failures            uint64
	ewmaLatencyMs       float64
	lastError           string
	lastSuccessAt       time.Time
	lastFailureAt       time.Time
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
	state := states[key]
	state.label = label
	now := r.now()
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
	state := states[key]
	state.successes++
	state.consecutiveFailures = 0
	state.openCount = 0
	state.cooldownUntil = time.Time{}
	state.probeInFlight = false
	state.lastError = ""
	state.lastSuccessAt = r.now()
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
	state := states[key]
	wasProbe := state.probeInFlight
	state.probeInFlight = false
	state.failures++
	state.consecutiveFailures++
	state.lastFailureAt = now
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
	state := states[key]
	state.probeInFlight = false
	states[key] = state
	r.mu.Unlock()
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
	for _, state := range states {
		status := "closed"
		if state.cooldownUntil.After(now) {
			status = "open"
		} else if !state.cooldownUntil.IsZero() || state.probeInFlight {
			status = "half_open"
		}
		views = append(views, map[string]interface{}{
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
	upstreamErr, ok := asUpstreamError(err)
	if !ok {
		return err != nil
	}
	switch upstreamErr.Kind {
	case UpstreamErrorTransient, UpstreamErrorFirstTokenTimeout, UpstreamErrorEndpointUnavailable, UpstreamErrorEmptyResponse, UpstreamErrorUnknown:
		return true
	default:
		return false
	}
}
