package proxy

import (
	"encoding/json"
	"fmt"
	"kiro-go/config"
	"kiro-go/logger"
	"os"
	"path/filepath"
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
	failureKind         string
	consecutiveFailures int
	cooldownUntil       time.Time
	lastCooldown        time.Duration
	lastError           string
	lastFailureAt       time.Time
	lastAccess          time.Time
}

type accountEndpointPreference struct {
	endpoint           string
	expiresAt          time.Time
	persistedExpiresAt time.Time
	lastAccess         time.Time
}

type accountEndpointRouteRegistry struct {
	mu             sync.Mutex
	routes         map[accountEndpointRouteKey]accountEndpointRouteState
	preferences    map[accountEndpointPreferenceKey]accountEndpointPreference
	now            func() time.Time
	persistMu      sync.Mutex
	persistWriteMu sync.Mutex
	persistPath    string
	persistTimer   *time.Timer
	persistEpoch   uint64
}

type accountEndpointRouteSnapshot struct {
	AccountID           string `json:"accountId"`
	Model               string `json:"model"`
	Workload            string `json:"workload"`
	Endpoint            string `json:"endpoint"`
	FailureKind         string `json:"failureKind,omitempty"`
	ConsecutiveFailures int    `json:"consecutiveFailures"`
	CooldownSeconds     int64  `json:"cooldownSeconds"`
	LastCooldownMs      int64  `json:"lastCooldownMs"`
	LastError           string `json:"lastError,omitempty"`
	LastFailureAt       int64  `json:"lastFailureAt,omitempty"`
}

type persistedAccountEndpointRoute struct {
	AccountID           string `json:"accountId"`
	Model               string `json:"model"`
	EndpointKey         string `json:"endpointKey"`
	EndpointName        string `json:"endpointName,omitempty"`
	FailureKind         string `json:"failureKind,omitempty"`
	ConsecutiveFailures int    `json:"consecutiveFailures,omitempty"`
	CooldownUntil       int64  `json:"cooldownUntil"`
	LastCooldownMs      int64  `json:"lastCooldownMs,omitempty"`
	LastFailureAt       int64  `json:"lastFailureAt,omitempty"`
}

type persistedAccountEndpointPreference struct {
	AccountID   string `json:"accountId"`
	Model       string `json:"model"`
	EndpointKey string `json:"endpointKey"`
	ExpiresAt   int64  `json:"expiresAt"`
}

type persistedAccountEndpointRouteState struct {
	Version     int                                  `json:"version"`
	SavedAt     int64                                `json:"savedAt"`
	Routes      []persistedAccountEndpointRoute      `json:"routes,omitempty"`
	Preferences []persistedAccountEndpointPreference `json:"preferences,omitempty"`
}

type accountEndpointPreferenceSnapshot struct {
	AccountID     string `json:"accountId"`
	Model         string `json:"model"`
	Workload      string `json:"workload"`
	Endpoint      string `json:"endpoint"`
	ExpiresInSecs int64  `json:"expiresInSeconds"`
}

type accountEndpointRoutingSnapshot struct {
	Cooldowns  []accountEndpointRouteSnapshot      `json:"cooldowns"`
	Affinities []accountEndpointPreferenceSnapshot `json:"affinities"`
}

const longToolEndpointRouteSuffix = "|long-tool"

const (
	accountEndpointRouteStateVersion = 1
	accountEndpointRouteSaveDelay    = 750 * time.Millisecond
)

var sharedAccountEndpointRoutes = newAccountEndpointRouteRegistry()

func newAccountEndpointRouteRegistry() *accountEndpointRouteRegistry {
	return &accountEndpointRouteRegistry{
		routes:      make(map[accountEndpointRouteKey]accountEndpointRouteState),
		preferences: make(map[accountEndpointPreferenceKey]accountEndpointPreference),
		now:         time.Now,
	}
}

func accountEndpointRouteStatePath() string {
	return filepath.Join(config.GetConfigDir(), "endpoint_routes.json")
}

func (r *accountEndpointRouteRegistry) load(path string) (int, error) {
	if r == nil {
		return 0, nil
	}
	path = strings.TrimSpace(path)
	r.persistMu.Lock()
	r.persistEpoch++
	if r.persistTimer != nil {
		r.persistTimer.Stop()
		r.persistTimer = nil
	}
	r.persistPath = path
	r.persistMu.Unlock()
	r.persistWriteMu.Lock()
	defer r.persistWriteMu.Unlock()
	if path == "" {
		r.replace(nil, nil)
		return 0, nil
	}

	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		r.replace(nil, nil)
		return 0, nil
	}
	if err != nil {
		r.replace(nil, nil)
		return 0, err
	}
	var state persistedAccountEndpointRouteState
	if err := json.Unmarshal(data, &state); err != nil {
		r.replace(nil, nil)
		return 0, err
	}
	if state.Version != accountEndpointRouteStateVersion {
		r.replace(nil, nil)
		return 0, fmt.Errorf("unsupported endpoint route state version %d", state.Version)
	}

	now := r.now()
	routes := make(map[accountEndpointRouteKey]accountEndpointRouteState)
	preferences := make(map[accountEndpointPreferenceKey]accountEndpointPreference)
	for _, entry := range state.Routes {
		until := time.Unix(entry.CooldownUntil, 0)
		if strings.TrimSpace(entry.AccountID) == "" || strings.TrimSpace(entry.Model) == "" ||
			strings.TrimSpace(entry.EndpointKey) == "" || !until.After(now) {
			continue
		}
		key := accountEndpointRouteKey{
			accountID: strings.TrimSpace(entry.AccountID),
			model:     normalizeEndpointRoutePart(entry.Model),
			endpoint:  normalizeEndpointRoutePart(entry.EndpointKey),
		}
		routes[key] = accountEndpointRouteState{
			endpoint:            strings.TrimSpace(entry.EndpointName),
			failureKind:         strings.TrimSpace(entry.FailureKind),
			consecutiveFailures: entry.ConsecutiveFailures,
			cooldownUntil:       until,
			lastCooldown:        time.Duration(entry.LastCooldownMs) * time.Millisecond,
			lastFailureAt:       time.Unix(entry.LastFailureAt, 0),
			lastAccess:          now,
		}
	}
	for _, entry := range state.Preferences {
		expiresAt := time.Unix(entry.ExpiresAt, 0)
		if strings.TrimSpace(entry.AccountID) == "" || strings.TrimSpace(entry.Model) == "" ||
			strings.TrimSpace(entry.EndpointKey) == "" || !expiresAt.After(now) {
			continue
		}
		key := accountEndpointPreferenceKey{
			accountID: strings.TrimSpace(entry.AccountID),
			model:     normalizeEndpointRoutePart(entry.Model),
		}
		preferences[key] = accountEndpointPreference{
			endpoint:           normalizeEndpointRoutePart(entry.EndpointKey),
			expiresAt:          expiresAt,
			persistedExpiresAt: expiresAt,
			lastAccess:         now,
		}
	}

	r.replace(routes, preferences)
	return len(routes) + len(preferences), nil
}

func (r *accountEndpointRouteRegistry) replace(routes map[accountEndpointRouteKey]accountEndpointRouteState, preferences map[accountEndpointPreferenceKey]accountEndpointPreference) {
	if routes == nil {
		routes = make(map[accountEndpointRouteKey]accountEndpointRouteState)
	}
	if preferences == nil {
		preferences = make(map[accountEndpointPreferenceKey]accountEndpointPreference)
	}
	r.mu.Lock()
	r.routes = routes
	r.preferences = preferences
	r.enforceLimitsLocked()
	r.mu.Unlock()
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
	case UpstreamErrorQuota, UpstreamErrorRateLimit, UpstreamErrorModelUnavailable, UpstreamErrorTransient,
		UpstreamErrorFirstTokenTimeout, UpstreamErrorActionableTimeout, UpstreamErrorToolAssemblyTimeout,
		UpstreamErrorToolOutputTruncated, UpstreamErrorEndpointUnavailable, UpstreamErrorEmptyResponse,
		UpstreamErrorStreamTruncated, UpstreamErrorForbidden:
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
	key := accountEndpointRouteKey{accountID: accountID, model: model, endpoint: endpointKey}
	state := r.routes[key]
	state.endpoint = endpoint.Name
	state.failureKind = string(upstreamErr.Kind)
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
	if upstreamErr.Kind == UpstreamErrorForbidden || upstreamErr.Kind == UpstreamErrorModelUnavailable ||
		(upstreamErr.Kind == UpstreamErrorEndpointUnavailable && upstreamErr.StatusCode != 0) {
		cooldown = time.Duration(settings.RouteAffinityTTLSeconds) * time.Second
		if cooldown <= 0 {
			cooldown = time.Hour
		}
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
	r.mu.Unlock()
	r.scheduleSave()
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
	preferenceKey := accountEndpointPreferenceKey{accountID: accountID, model: model}
	previous, hadPrevious := r.preferences[preferenceKey]
	persistedExpiresAt := time.Time{}
	if hadPrevious && previous.endpoint == endpointKey {
		persistedExpiresAt = previous.persistedExpiresAt
	}
	delete(r.routes, accountEndpointRouteKey{accountID: accountID, model: model, endpoint: endpointKey})
	r.preferences[preferenceKey] = accountEndpointPreference{
		endpoint:           endpointKey,
		expiresAt:          now.Add(ttl),
		persistedExpiresAt: persistedExpiresAt,
		lastAccess:         now,
	}
	r.pruneExpiredLocked(now)
	r.enforceLimitsLocked()
	r.mu.Unlock()
	if !hadPrevious || previous.endpoint != endpointKey || !persistedExpiresAt.After(now.Add(ttl/2)) {
		r.scheduleSave()
	}
}

func (r *accountEndpointRouteRegistry) clearFailure(accountID, model string, endpoint kiroEndpoint) {
	if r == nil || strings.TrimSpace(accountID) == "" || strings.TrimSpace(model) == "" {
		return
	}
	key := accountEndpointRouteKey{
		accountID: strings.TrimSpace(accountID),
		model:     normalizeEndpointRoutePart(model),
		endpoint:  normalizeEndpointRoutePart(endpoint.Key),
	}
	r.mu.Lock()
	_, changed := r.routes[key]
	delete(r.routes, key)
	r.mu.Unlock()
	if changed {
		r.scheduleSave()
	}
}

func (r *accountEndpointRouteRegistry) scheduleSave() {
	if r == nil {
		return
	}
	r.persistMu.Lock()
	if r.persistPath == "" || r.persistTimer != nil {
		r.persistMu.Unlock()
		return
	}
	epoch := r.persistEpoch
	r.persistTimer = time.AfterFunc(accountEndpointRouteSaveDelay, func() {
		r.persistMu.Lock()
		if epoch != r.persistEpoch {
			r.persistMu.Unlock()
			return
		}
		r.persistTimer = nil
		path := r.persistPath
		r.persistMu.Unlock()
		if err := r.saveTo(path); err != nil {
			logger.Warnf("[EndpointRouting] Failed to persist adaptive route state: %v", err)
		}
	})
	r.persistMu.Unlock()
}

func (r *accountEndpointRouteRegistry) flush() error {
	if r == nil {
		return nil
	}
	r.persistMu.Lock()
	if r.persistTimer != nil {
		r.persistTimer.Stop()
		r.persistTimer = nil
	}
	path := r.persistPath
	r.persistMu.Unlock()
	if path == "" {
		return nil
	}
	return r.saveTo(path)
}

func (r *accountEndpointRouteRegistry) saveTo(path string) error {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	r.persistWriteMu.Lock()
	defer r.persistWriteMu.Unlock()
	now := r.now()
	state := persistedAccountEndpointRouteState{
		Version: accountEndpointRouteStateVersion,
		SavedAt: now.Unix(),
	}
	r.mu.Lock()
	r.pruneExpiredLocked(now)
	for key, route := range r.routes {
		if !route.cooldownUntil.After(now) {
			continue
		}
		state.Routes = append(state.Routes, persistedAccountEndpointRoute{
			AccountID:           key.accountID,
			Model:               key.model,
			EndpointKey:         key.endpoint,
			EndpointName:        route.endpoint,
			FailureKind:         route.failureKind,
			ConsecutiveFailures: route.consecutiveFailures,
			CooldownUntil:       route.cooldownUntil.Unix(),
			LastCooldownMs:      route.lastCooldown.Milliseconds(),
			LastFailureAt:       route.lastFailureAt.Unix(),
		})
	}
	for key, preference := range r.preferences {
		if !preference.expiresAt.After(now) {
			continue
		}
		state.Preferences = append(state.Preferences, persistedAccountEndpointPreference{
			AccountID:   key.accountID,
			Model:       key.model,
			EndpointKey: preference.endpoint,
			ExpiresAt:   preference.expiresAt.Unix(),
		})
	}
	r.mu.Unlock()
	sort.Slice(state.Routes, func(i, j int) bool {
		if state.Routes[i].AccountID != state.Routes[j].AccountID {
			return state.Routes[i].AccountID < state.Routes[j].AccountID
		}
		if state.Routes[i].Model != state.Routes[j].Model {
			return state.Routes[i].Model < state.Routes[j].Model
		}
		return state.Routes[i].EndpointKey < state.Routes[j].EndpointKey
	})
	sort.Slice(state.Preferences, func(i, j int) bool {
		if state.Preferences[i].AccountID != state.Preferences[j].AccountID {
			return state.Preferences[i].AccountID < state.Preferences[j].AccountID
		}
		return state.Preferences[i].Model < state.Preferences[j].Model
	})
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	r.mu.Lock()
	for _, entry := range state.Preferences {
		key := accountEndpointPreferenceKey{
			accountID: entry.AccountID,
			model:     normalizeEndpointRoutePart(entry.Model),
		}
		preference, ok := r.preferences[key]
		if !ok || preference.endpoint != normalizeEndpointRoutePart(entry.EndpointKey) {
			continue
		}
		persistedExpiresAt := time.Unix(entry.ExpiresAt, 0)
		if persistedExpiresAt.After(preference.persistedExpiresAt) {
			preference.persistedExpiresAt = persistedExpiresAt
			r.preferences[key] = preference
		}
	}
	r.mu.Unlock()
	return nil
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
			FailureKind:         state.failureKind,
			ConsecutiveFailures: state.consecutiveFailures,
			CooldownSeconds:     ceilDurationSeconds(state.cooldownUntil.Sub(now)),
			LastCooldownMs:      state.lastCooldown.Milliseconds(),
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

func (r *accountEndpointRouteRegistry) snapshotsByAccount() map[string]accountEndpointRoutingSnapshot {
	result := make(map[string]accountEndpointRoutingSnapshot)
	if r == nil {
		return result
	}
	now := r.now()
	r.mu.Lock()
	r.pruneExpiredLocked(now)
	for key, state := range r.routes {
		if !state.cooldownUntil.After(now) {
			continue
		}
		model, workload := endpointRouteDisplayModel(key.model)
		view := result[key.accountID]
		view.Cooldowns = append(view.Cooldowns, accountEndpointRouteSnapshot{
			AccountID:           key.accountID,
			Model:               model,
			Workload:            workload,
			Endpoint:            state.endpoint,
			FailureKind:         state.failureKind,
			ConsecutiveFailures: state.consecutiveFailures,
			CooldownSeconds:     ceilDurationSeconds(state.cooldownUntil.Sub(now)),
			LastCooldownMs:      state.lastCooldown.Milliseconds(),
			LastError:           state.lastError,
			LastFailureAt:       state.lastFailureAt.Unix(),
		})
		result[key.accountID] = view
	}
	for key, preference := range r.preferences {
		if !preference.expiresAt.After(now) {
			continue
		}
		model, workload := endpointRouteDisplayModel(key.model)
		view := result[key.accountID]
		view.Affinities = append(view.Affinities, accountEndpointPreferenceSnapshot{
			AccountID:     key.accountID,
			Model:         model,
			Workload:      workload,
			Endpoint:      preference.endpoint,
			ExpiresInSecs: ceilDurationSeconds(preference.expiresAt.Sub(now)),
		})
		result[key.accountID] = view
	}
	r.mu.Unlock()
	return result
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

func (r *accountEndpointRouteRegistry) forgetAccount(accountID string) {
	if r == nil || strings.TrimSpace(accountID) == "" {
		return
	}
	accountID = strings.TrimSpace(accountID)
	changed := false
	r.mu.Lock()
	for key := range r.routes {
		if key.accountID == accountID {
			delete(r.routes, key)
			changed = true
		}
	}
	for key := range r.preferences {
		if key.accountID == accountID {
			delete(r.preferences, key)
			changed = true
		}
	}
	r.mu.Unlock()
	if changed {
		r.scheduleSave()
	}
}
