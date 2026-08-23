package proxy

import (
	"context"
	"encoding/json"
	"kiro-go/config"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newAccountEndpointRouteTestRegistry(t *testing.T) (*accountEndpointRouteRegistry, *time.Time) {
	t.Helper()
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	registry := newAccountEndpointRouteRegistry()
	registry.now = func() time.Time { return now }
	return registry, &now
}

func testRouteEndpoints() []kiroEndpoint {
	return []kiroEndpoint{
		{Key: "runtime", Name: "Kiro Runtime"},
		{Key: "kiro", Name: "Kiro IDE"},
		{Key: "codewhisperer", Name: "CodeWhisperer"},
		{Key: "amazonq", Name: "AmazonQ"},
	}
}

func TestAccountEndpointRoutePrefersRecentAutoSuccess(t *testing.T) {
	registry, _ := newAccountEndpointRouteTestRegistry(t)
	registry.recordSuccess("account-a", "claude-sonnet-5", kiroEndpoint{Key: "codewhisperer", Name: "CodeWhisperer"})

	endpoints, err := registry.availableEndpoints("account-a", "claude-sonnet-5", "auto", testRouteEndpoints())
	if err != nil {
		t.Fatalf("available endpoints: %v", err)
	}
	if len(endpoints) != 4 || endpoints[0].Key != "codewhisperer" {
		t.Fatalf("recent successful endpoint was not preferred: %+v", endpoints)
	}
}

func TestLongToolRequestPrefersKiroAndUsesSeparateRouteKey(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	payload := &KiroPayload{}
	payload.ConversationState.CurrentMessage.UserInputMessage.ModelID = "claude-sonnet-5"
	var tool KiroToolWrapper
	tool.ToolSpecification.Name = "Write"
	payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext = &UserInputMessageContext{Tools: []KiroToolWrapper{tool}}

	endpoints := getRequestEndpoints("auto", payload)
	if len(endpoints) == 0 || endpoints[0].Key != "kiro" {
		t.Fatalf("long tool endpoint order = %+v", endpoints)
	}
	explicit := getRequestEndpoints("runtime", payload)
	if len(explicit) == 0 || explicit[0].Key != "runtime" {
		t.Fatalf("explicit endpoint preference was not honored: %+v", explicit)
	}
	if got := endpointRouteModel(payload); got != "claude-sonnet-5"+longToolEndpointRouteSuffix {
		t.Fatalf("long tool route key = %q", got)
	}

	err := newToolOutputTruncatedError("Kiro Runtime", &EventStreamError{Kind: EventStreamIncompleteToolUse})
	if _, eligible := endpointRouteFailure(err); !eligible {
		t.Fatal("tool truncation did not cool the long-tool endpoint route")
	}
}

func TestAPIKeyAccountEndpointsSkipRuntimeAndPreferKiro(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	account := &config.Account{AuthMethod: "api_key", KiroApiKey: "ksk_test"}
	endpoints := getRequestEndpointsForAccount("auto", &KiroPayload{}, account)
	if len(endpoints) == 0 || endpoints[0].Key != "kiro" {
		t.Fatalf("API key endpoint order = %+v", endpoints)
	}
	for _, endpoint := range endpoints {
		if endpoint.RequiresProfileArn || endpoint.Key == "runtime" {
			t.Fatalf("API key account received profile-bound endpoint: %+v", endpoints)
		}
	}

	account.ProfileArn = "arn:aws:codewhisperer:us-east-1:123456789012:profile/test"
	withProfile := getRequestEndpointsForAccount("auto", &KiroPayload{}, account)
	if len(withProfile) == 0 || withProfile[0].Key != "runtime" {
		t.Fatalf("profile-backed API key endpoint order = %+v", withProfile)
	}
}

func TestAccountEndpointPreferenceOverridesGlobalFixedEndpoint(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	if err := config.UpdatePreferredEndpoint("runtime"); err != nil {
		t.Fatalf("set global endpoint: %v", err)
	}
	if err := config.UpdateEndpointFallback(false); err != nil {
		t.Fatalf("disable global fallback: %v", err)
	}
	account := &config.Account{
		EndpointPreference: "auto",
		ProfileArn:         "arn:aws:codewhisperer:us-east-1:123456789012:profile/test",
	}
	adaptive := getRequestEndpointsForAccount("runtime", &KiroPayload{}, account)
	if len(adaptive) != len(kiroEndpoints) || adaptive[0].Key != "runtime" {
		t.Fatalf("account adaptive override endpoints = %+v", adaptive)
	}
	account.EndpointPreference = "kiro"
	fixed := getRequestEndpointsForAccount("runtime", &KiroPayload{}, account)
	if len(fixed) != 1 || fixed[0].Key != "kiro" {
		t.Fatalf("account fixed override endpoints = %+v", fixed)
	}
}

func TestBuilderIDHighRiskRequestStillStartsWithRuntime(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	payload := &KiroPayload{}
	payload.ConversationState.CurrentMessage.UserInputMessage.ModelID = "claude-sonnet-5"
	var tool KiroToolWrapper
	tool.ToolSpecification.Name = "Write"
	payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext = &UserInputMessageContext{Tools: []KiroToolWrapper{tool}}
	account := &config.Account{
		AuthMethod: "idc",
		Provider:   "builderid",
		ProfileArn: "arn:aws:codewhisperer:us-east-1:123456789012:profile/test",
	}

	endpoints := getRequestEndpointsForAccount("auto", payload, account)
	if len(endpoints) == 0 || endpoints[0].Key != "runtime" {
		t.Fatalf("Builder ID high-risk endpoint order = %+v", endpoints)
	}
}

func TestIDCAccountWithoutProviderStillPrefersRuntime(t *testing.T) {
	if got := accountAutoEndpointHint(&config.Account{AuthMethod: "IdC"}); got != "runtime" {
		t.Fatalf("IDC endpoint hint = %q, want runtime", got)
	}
	if got := accountAutoEndpointHint(&config.Account{Provider: "Enterprise"}); got != "runtime" {
		t.Fatalf("Enterprise endpoint hint = %q, want runtime", got)
	}
}

func TestAccountEndpointRouteSkipsCoolingRateLimitedEndpoint(t *testing.T) {
	registry, _ := newAccountEndpointRouteTestRegistry(t)
	err := classifyUpstreamHTTPError(http.StatusTooManyRequests, "Kiro Runtime", []byte(`{"message":"too many requests"}`))
	if cooldown := registry.recordFailure("account-a", "claude-sonnet-5", kiroEndpoint{Key: "runtime", Name: "Kiro Runtime"}, err); cooldown <= 0 {
		t.Fatal("expected endpoint cooldown")
	}

	endpoints, routeErr := registry.availableEndpoints("account-a", "claude-sonnet-5", "auto", testRouteEndpoints())
	if routeErr != nil {
		t.Fatalf("available endpoints: %v", routeErr)
	}
	if len(endpoints) != 3 {
		t.Fatalf("expected one cooling endpoint to be skipped, got %+v", endpoints)
	}
	for _, endpoint := range endpoints {
		if endpoint.Key == "runtime" {
			t.Fatal("cooling runtime endpoint was returned")
		}
	}
}

func TestAccountEndpointRouteClearsAffinityAfterTransportFailure(t *testing.T) {
	registry, _ := newAccountEndpointRouteTestRegistry(t)
	endpoint := kiroEndpoint{Key: "codewhisperer", Name: "CodeWhisperer"}
	registry.recordSuccess("account-a", "claude-sonnet-5", endpoint)
	if cooldown := registry.recordFailure("account-a", "claude-sonnet-5", endpoint, classifyTransportError(endpoint.Name, context.DeadlineExceeded)); cooldown <= 0 {
		t.Fatal("expected transport failure to cool the sticky endpoint")
	}

	endpoints, err := registry.availableEndpoints("account-a", "claude-sonnet-5", "auto", testRouteEndpoints())
	if err != nil {
		t.Fatalf("available endpoints: %v", err)
	}
	for _, candidate := range endpoints {
		if candidate.Key == endpoint.Key {
			t.Fatalf("failed sticky endpoint remained available: %+v", endpoints)
		}
	}
	if len(endpoints) == 0 || endpoints[0].Key != "runtime" {
		t.Fatalf("auto order did not return to the default route: %+v", endpoints)
	}
}

func TestAccountEndpointRouteReturnsRateLimitWhenAllEndpointsCool(t *testing.T) {
	registry, _ := newAccountEndpointRouteTestRegistry(t)
	err := classifyUpstreamHTTPError(http.StatusTooManyRequests, "upstream", []byte(`{"message":"too many requests"}`))
	for _, endpoint := range testRouteEndpoints() {
		registry.recordFailure("account-a", "claude-sonnet-5", endpoint, err)
	}

	endpoints, routeErr := registry.availableEndpoints("account-a", "claude-sonnet-5", "auto", testRouteEndpoints())
	if len(endpoints) != 0 {
		t.Fatalf("expected no available endpoints, got %+v", endpoints)
	}
	upstreamErr, ok := asUpstreamError(routeErr)
	if !ok || upstreamErr.Kind != UpstreamErrorRateLimit || !upstreamErr.RetryAcrossAccounts || upstreamErr.RetryAfter <= 0 {
		t.Fatalf("unexpected all-cooling error: %#v", routeErr)
	}
}

func TestAccountEndpointRouteCoolsModelUnavailableEndpoint(t *testing.T) {
	registry, _ := newAccountEndpointRouteTestRegistry(t)
	endpoint := kiroEndpoint{Key: "runtime", Name: "Kiro Runtime"}
	err := classifyUpstreamHTTPError(http.StatusBadRequest, endpoint.Name, []byte(`{"message":"invalid_model_id"}`))
	if cooldown := registry.recordFailure("account-a", "future-model", endpoint, err); cooldown != time.Hour {
		t.Fatalf("model-unavailable cooldown = %s, want 1h", cooldown)
	}
	endpoints, routeErr := registry.availableEndpoints("account-a", "future-model", "auto", testRouteEndpoints())
	if routeErr != nil || len(endpoints) != 3 {
		t.Fatalf("model-unavailable route filtering: endpoints=%+v err=%v", endpoints, routeErr)
	}
	for _, candidate := range endpoints {
		if candidate.Key == endpoint.Key {
			t.Fatalf("model-unavailable endpoint remained selectable: %+v", endpoints)
		}
	}
}

func TestExplicitPreferredEndpointStaysFirstUntilItCools(t *testing.T) {
	registry, _ := newAccountEndpointRouteTestRegistry(t)
	registry.recordSuccess("account-a", "claude-sonnet-5", kiroEndpoint{Key: "kiro", Name: "Kiro IDE"})

	endpoints, err := registry.availableEndpoints("account-a", "claude-sonnet-5", "codewhisperer", getSortedEndpoints("codewhisperer"))
	if err != nil {
		t.Fatalf("available endpoints: %v", err)
	}
	if endpoints[0].Key != "codewhisperer" {
		t.Fatalf("explicit preferred endpoint was displaced: %+v", endpoints)
	}
}

func TestAccountEndpointRoutePersistenceReloadsAffinityAndCooldown(t *testing.T) {
	registry, _ := newAccountEndpointRouteTestRegistry(t)
	path := filepath.Join(t.TempDir(), "endpoint_routes.json")
	if restored, err := registry.load(path); err != nil || restored != 0 {
		t.Fatalf("initialize route persistence: restored=%d err=%v", restored, err)
	}
	registry.recordSuccess("account-a", "claude-sonnet-5", kiroEndpoint{Key: "codewhisperer", Name: "CodeWhisperer"})
	registry.recordFailure(
		"account-a",
		"claude-sonnet-5",
		kiroEndpoint{Key: "runtime", Name: "Kiro Runtime"},
		classifyUpstreamHTTPError(http.StatusTooManyRequests, "Kiro Runtime", []byte(`{"message":"too many requests"}`)),
	)
	if err := registry.flush(); err != nil {
		t.Fatalf("flush route persistence: %v", err)
	}

	reloaded := newAccountEndpointRouteRegistry()
	reloaded.now = registry.now
	if restored, err := reloaded.load(path); err != nil || restored != 2 {
		t.Fatalf("reload route persistence: restored=%d err=%v", restored, err)
	}
	endpoints, err := reloaded.availableEndpoints("account-a", "claude-sonnet-5", "auto", testRouteEndpoints())
	if err != nil {
		t.Fatalf("available endpoints after reload: %v", err)
	}
	if len(endpoints) != 3 || endpoints[0].Key != "codewhisperer" {
		t.Fatalf("reloaded route order = %+v", endpoints)
	}
	for _, endpoint := range endpoints {
		if endpoint.Key == "runtime" {
			t.Fatalf("reloaded cooldown did not suppress runtime: %+v", endpoints)
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat route state: %v", err)
	}
	if got := info.Mode().Perm(); got != 0600 {
		t.Fatalf("route state permissions = %o, want 600", got)
	}
}

func TestAccountEndpointRouteRenewsPersistedAffinityBeforeExpiry(t *testing.T) {
	registry, now := newAccountEndpointRouteTestRegistry(t)
	path := filepath.Join(t.TempDir(), "endpoint_routes.json")
	if _, err := registry.load(path); err != nil {
		t.Fatalf("initialize route persistence: %v", err)
	}
	endpoint := kiroEndpoint{Key: "runtime", Name: "Kiro Runtime"}
	registry.recordSuccess("account-a", "claude-sonnet-5", endpoint)
	if err := registry.flush(); err != nil {
		t.Fatalf("initial route flush: %v", err)
	}

	*now = now.Add(31 * time.Minute)
	registry.recordSuccess("account-a", "claude-sonnet-5", endpoint)
	registry.persistMu.Lock()
	renewalScheduled := registry.persistTimer != nil
	registry.persistMu.Unlock()
	if !renewalScheduled {
		t.Fatal("affinity renewal was not scheduled after persisted TTL passed its midpoint")
	}
	if err := registry.flush(); err != nil {
		t.Fatalf("renewed route flush: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read renewed route state: %v", err)
	}
	var state persistedAccountEndpointRouteState
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatalf("decode renewed route state: %v", err)
	}
	wantExpiry := now.Add(time.Hour).Unix()
	if len(state.Preferences) != 1 || state.Preferences[0].ExpiresAt != wantExpiry {
		t.Fatalf("persisted affinity expiry = %+v, want %d", state.Preferences, wantExpiry)
	}
}

func TestAccountEndpointRouteMissingFileClearsMemory(t *testing.T) {
	registry, _ := newAccountEndpointRouteTestRegistry(t)
	registry.recordSuccess("account-a", "claude-sonnet-5", kiroEndpoint{Key: "kiro", Name: "Kiro IDE"})
	if restored, err := registry.load(filepath.Join(t.TempDir(), "missing.json")); err != nil || restored != 0 {
		t.Fatalf("load missing route state: restored=%d err=%v", restored, err)
	}
	if got := registry.snapshot()["affinities"].([]accountEndpointPreferenceSnapshot); len(got) != 0 {
		t.Fatalf("missing state file retained stale affinities: %+v", got)
	}
}

func TestAccountEndpointRouteClearsProbeFailureWithoutReplacingAffinity(t *testing.T) {
	registry, _ := newAccountEndpointRouteTestRegistry(t)
	preferred := kiroEndpoint{Key: "runtime", Name: "Kiro Runtime"}
	probe := kiroEndpoint{Key: "amazonq", Name: "AmazonQ"}
	registry.recordFailure(
		"account-a",
		"claude-sonnet-5",
		probe,
		classifyUpstreamHTTPError(http.StatusTooManyRequests, probe.Name, []byte(`{"message":"too many requests"}`)),
	)
	registry.recordSuccess("account-a", "claude-sonnet-5", preferred)
	registry.clearFailure("account-a", "claude-sonnet-5", probe)

	endpoints, err := registry.availableEndpoints("account-a", "claude-sonnet-5", "auto", testRouteEndpoints())
	if err != nil || len(endpoints) != 4 || endpoints[0].Key != preferred.Key {
		t.Fatalf("probe success replaced learned affinity: endpoints=%+v err=%v", endpoints, err)
	}
}
