package proxy

import (
	"fmt"
	"kiro-go/config"
	"sort"
	"strings"
	"sync"
	"time"
)

type accountEndpointRouteKey struct {
	accountID string
	model     string
	endpoint  string
}

type accountEndpointPreferenceKey struct {
	accountID string
	model     string
}

type accountEndpointRouteState struct {
	endpoint            string
	consecutiveFailures int
	cooldownUntil       time.Time
	lastCooldown        time.Duration
	lastError           string
	lastFailureAt       time.Time
	lastAccess          time.Time
}

type accountEndpointPreference struct {
	endpoint   string
	expiresAt  time.Time
	lastAccess time.Time
}

type accountEndpointRouteRegistry struct {
	mu          sync.Mutex
	routes      map[accountEndpointRouteKey]accountEndpointRouteState
	preferences map[accountEndpointPreferenceKey]accountEndpointPreference
	now         func() time.Time
}

type accountEndpointRouteSnapshot struct {
	AccountID           string `json:"accountId"`
	Model               string `json:"model"`
	Workload            string `json:"workload"`
	Endpoint            string `json:"endpoint"`
	ConsecutiveFailures int    `json:"consecutiveFailures"`
	CooldownSeconds     int64  `json:"cooldownSeconds"`
	LastCooldownMs      int64  `json:"lastCooldownMs"`
	LastError           string `json:"lastError,omitempty"`
	LastFailureAt       int64  `json:"lastFailureAt,omitempty"`
}

type accountEndpointPreferenceSnapshot struct {
	AccountID     string `json:"accountId"`
	Model         string `json:"model"`
	Workload      string `json:"workload"`
	Endpoint      string `json:"endpoint"`
	ExpiresInSecs int64  `json:"expiresInSeconds"`
}

const longToolEndpointRouteSuffix = "|long-tool"

var sharedAccountEndpointRoutes = newAccountEndpointRouteRegistry()

func newAccountEndpointRouteRegistry() *accountEndpointRouteRegistry {
	return &accountEndpointRouteRegistry{
		routes:      make(map[accountEndpointRouteKey]accountEndpointRouteState),
		preferences: make(map[accountEndpointPreferenceKey]accountEndpointPreference),
		now:         time.Now,
	}
}

func endpointRouteModel(payload *KiroPayload) string {
	if payload == nil {
		return ""
	}
	model := strings.ToLower(strings.TrimSpace(payload.ConversationState.CurrentMessage.UserInputMessage.ModelID))
	if model != "" && payloadHasHighRiskTools(payload) {
		model += longToolEndpointRouteSuffix
	}
	return model
}

func endpointRouteDisplayModel(model string) (string, string) {
	if strings.HasSuffix(model, longToolEndpointRouteSuffix) {
		return strings.TrimSuffix(model, longToolEndpointRouteSuffix), "long-tool"
	}
	return model, "default"
}

func normalizeEndpointRoutePart(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

// availableEndpoints removes account-specific cooling endpoints and, in auto
// mode, starts with the endpoint that most recently succeeded for this model.
func (r *accountEndpointRouteRegistry) availableEndpoints(accountID, model, preferred string, endpoints []kiroEndpoint) ([]kiroEndpoint, error) {
	if r == nil || strings.TrimSpace(accountID) == "" || strings.TrimSpace(model) == "" || len(endpoints) == 0 {
		return append([]kiroEndpoint(nil), endpoints...), nil
	}
	accountID = strings.TrimSpace(accountID)
	model = normalizeEndpointRoutePart(model)
	preferred = normalizeEndpointRoutePart(preferred)
	now := r.now()

	r.mu.Lock()
	r.pruneExpiredLocked(now)
	ordered := append([]kiroEndpoint(nil), endpoints...)
	preferenceKey := accountEndpointPreferenceKey{accountID: accountID, model: model}
	if preferred == "" || preferred == "auto" {
		if preference, ok := r.preferences[preferenceKey]; ok && preference.expiresAt.After(now) {
			preference.lastAccess = now
			r.preferences[preferenceKey] = preference
			ordered = moveEndpointFirst(ordered, preference.endpoint)
		}
	}

	available := make([]kiroEndpoint, 0, len(ordered))
	var retryAfter time.Duration
	for _, endpoint := range ordered {
		key := accountEndpointRouteKey{accountID: accountID, model: model, endpoint: normalizeEndpointRoutePart(endpoint.Key)}
		state, ok := r.routes[key]
		if !ok || !state.cooldownUntil.After(now) {
			available = append(available, endpoint)
			continue
		}
		state.lastAccess = now
		r.routes[key] = state
		remaining := state.cooldownUntil.Sub(now)
		if retryAfter == 0 || remaining < retryAfter {
			retryAfter = remaining
		}
	}
	r.mu.Unlock()

	if len(available) > 0 {
		return available, nil
	}
	return nil, &UpstreamError{
		Kind:                UpstreamErrorRateLimit,
		Endpoint:            "account endpoints",
		Message:             fmt.Sprintf("all endpoints are cooling for account model %s", model),
		RetryAcrossAccounts: true,
		RetryAfter:          retryAfter,
	}
}

func moveEndpointFirst(endpoints []kiroEndpoint, endpointKey string) []kiroEndpoint {
	endpointKey = normalizeEndpointRoutePart(endpointKey)
	for i := range endpoints {
		if normalizeEndpointRoutePart(endpoints[i].Key) != endpointKey || i == 0 {
			continue
		}
		preferred := endpoints[i]
		copy(endpoints[1:i+1], endpoints[0:i])
		endpoints[0] = preferred
		break
	}
	return endpoints
}

func endpointRouteFailure(err error) (*UpstreamError, bool) {
	upstreamErr, ok := asUpstreamError(err)
	if !ok {
		return nil, false
	}
	switch upstreamErr.Kind {
	case UpstreamErrorQuota, UpstreamErrorRateLimit, UpstreamErrorTransient,
		UpstreamErrorFirstTokenTimeout, UpstreamErrorToolAssemblyTimeout,
		UpstreamErrorToolOutputTruncated, UpstreamErrorEndpointUnavailable, UpstreamErrorEmptyResponse:
		return upstreamErr, true
	default:
		return upstreamErr, false
	}
}

func (r *accountEndpointRouteRegistry) recordFailure(accountID, model string, endpoint kiroEndpoint, err error) time.Duration {
	upstreamErr, eligible := endpointRouteFailure(err)
	if r == nil || !eligible || strings.TrimSpace(accountID) == "" || strings.TrimSpace(model) == "" {
		return 0
	}
	accountID = strings.TrimSpace(accountID)
	model = normalizeEndpointRoutePart(model)
	endpointKey := normalizeEndpointRoutePart(endpoint.Key)
	now := r.now()
	settings := config.GetUpstreamProtectionConfig()
	base := time.Duration(settings.RateLimitCooldownMs) * time.Millisecond
	if base <= 0 {
		base = 2 * time.Second
	}
	maximum := time.Duration(settings.MaxRateLimitCooldownMs) * time.Millisecond
	if maximum <= 0 {
		maximum = time.Minute
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	key := accountEndpointRouteKey{accountID: accountID, model: model, endpoint: endpointKey}
	state := r.routes[key]
	state.endpoint = endpoint.Name
	state.consecutiveFailures++
	cooldown := base
	for i := 1; i < state.consecutiveFailures && cooldown < maximum; i++ {
		cooldown *= 2
		if cooldown >= maximum {
			cooldown = maximum
			break
		}
	}
	if upstreamErr.Kind == UpstreamErrorQuota {
		cooldown = maximum
	}
	if upstreamErr.RetryAfter > cooldown {
		cooldown = upstreamErr.RetryAfter
		if cooldown > 24*time.Hour {
			cooldown = 24 * time.Hour
		}
	}
	state.cooldownUntil = now.Add(cooldown)
	state.lastCooldown = cooldown
	state.lastError = upstreamErr.Error()
	state.lastFailureAt = now
	state.lastAccess = now
	r.routes[key] = state

	preferenceKey := accountEndpointPreferenceKey{accountID: accountID, model: model}
	if preference, ok := r.preferences[preferenceKey]; ok && normalizeEndpointRoutePart(preference.endpoint) == endpointKey {
		delete(r.preferences, preferenceKey)
	}
	r.enforceLimitsLocked()
	return cooldown
}

func (r *accountEndpointRouteRegistry) recordSuccess(accountID, model string, endpoint kiroEndpoint) {
	if r == nil || strings.TrimSpace(accountID) == "" || strings.TrimSpace(model) == "" {
		return
	}
	accountID = strings.TrimSpace(accountID)
	model = normalizeEndpointRoutePart(model)
	endpointKey := normalizeEndpointRoutePart(endpoint.Key)
	now := r.now()
	ttl := time.Duration(config.GetUpstreamProtectionConfig().RouteAffinityTTLSeconds) * time.Second
	if ttl <= 0 {
		ttl = time.Hour
	}

	r.mu.Lock()
	delete(r.routes, accountEndpointRouteKey{accountID: accountID, model: model, endpoint: endpointKey})
	r.preferences[accountEndpointPreferenceKey{accountID: accountID, model: model}] = accountEndpointPreference{
		endpoint:   endpointKey,
		expiresAt:  now.Add(ttl),
		lastAccess: now,
	}
	r.pruneExpiredLocked(now)
	r.enforceLimitsLocked()
	r.mu.Unlock()
}

func (r *accountEndpointRouteRegistry) pruneExpiredLocked(now time.Time) {
	for key, preference := range r.preferences {
		if !preference.expiresAt.After(now) {
			delete(r.preferences, key)
		}
	}
	retention := time.Duration(config.GetUpstreamProtectionConfig().RouteAffinityTTLSeconds) * time.Second
	if retention <= 0 {
		retention = time.Hour
	}
	for key, state := range r.routes {
		if !state.cooldownUntil.After(now) && now.Sub(state.lastAccess) >= retention {
			delete(r.routes, key)
		}
	}
}

func (r *accountEndpointRouteRegistry) enforceLimitsLocked() {
	limit := config.GetUpstreamProtectionConfig().RouteAffinityMaxEntries
	if limit <= 0 {
		limit = 20000
	}
	for len(r.preferences) > limit {
		var oldestKey accountEndpointPreferenceKey
		var oldest time.Time
		for key, preference := range r.preferences {
			if oldest.IsZero() || preference.lastAccess.Before(oldest) {
				oldestKey = key
				oldest = preference.lastAccess
			}
		}
		delete(r.preferences, oldestKey)
	}
	for len(r.routes) > limit {
		var oldestKey accountEndpointRouteKey
		var oldest time.Time
		for key, state := range r.routes {
			if oldest.IsZero() || state.lastAccess.Before(oldest) {
				oldestKey = key
				oldest = state.lastAccess
			}
		}
		delete(r.routes, oldestKey)
	}
}

func (r *accountEndpointRouteRegistry) snapshot() map[string]interface{} {
	if r == nil {
		return map[string]interface{}{"cooldowns": []accountEndpointRouteSnapshot{}, "affinities": []accountEndpointPreferenceSnapshot{}}
	}
	now := r.now()
	r.mu.Lock()
	r.pruneExpiredLocked(now)
	cooldowns := make([]accountEndpointRouteSnapshot, 0, len(r.routes))
	for key, state := range r.routes {
		if !state.cooldownUntil.After(now) {
			continue
		}
		model, workload := endpointRouteDisplayModel(key.model)
		cooldowns = append(cooldowns, accountEndpointRouteSnapshot{
			AccountID:           key.accountID,
			Model:               model,
			Workload:            workload,
			Endpoint:            state.endpoint,
			ConsecutiveFailures: state.consecutiveFailures,
			CooldownSeconds:     ceilDurationSeconds(state.cooldownUntil.Sub(now)),
			LastCooldownMs:      state.lastCooldown.Milliseconds(),
			LastError:           state.lastError,
			LastFailureAt:       state.lastFailureAt.Unix(),
		})
	}
	affinities := make([]accountEndpointPreferenceSnapshot, 0, len(r.preferences))
	for key, preference := range r.preferences {
		model, workload := endpointRouteDisplayModel(key.model)
		affinities = append(affinities, accountEndpointPreferenceSnapshot{
			AccountID:     key.accountID,
			Model:         model,
			Workload:      workload,
			Endpoint:      preference.endpoint,
			ExpiresInSecs: ceilDurationSeconds(preference.expiresAt.Sub(now)),
		})
	}
	r.mu.Unlock()
	sort.Slice(cooldowns, func(i, j int) bool {
		if cooldowns[i].AccountID != cooldowns[j].AccountID {
			return cooldowns[i].AccountID < cooldowns[j].AccountID
		}
		if cooldowns[i].Model != cooldowns[j].Model {
			return cooldowns[i].Model < cooldowns[j].Model
		}
		return cooldowns[i].Endpoint < cooldowns[j].Endpoint
	})
	sort.Slice(affinities, func(i, j int) bool {
		if affinities[i].AccountID != affinities[j].AccountID {
			return affinities[i].AccountID < affinities[j].AccountID
		}
		return affinities[i].Model < affinities[j].Model
	})
	return map[string]interface{}{"cooldowns": cooldowns, "affinities": affinities}
}

func ceilDurationSeconds(duration time.Duration) int64 {
	if duration <= 0 {
		return 0
	}
	return int64((duration + time.Second - 1) / time.Second)
}

func (r *accountEndpointRouteRegistry) reset() {
	if r == nil {
		return
	}
	r.mu.Lock()
	clear(r.routes)
	clear(r.preferences)
	r.mu.Unlock()
}
