package proxy

import (
	"context"
	"kiro-go/config"
	"net/http"
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
