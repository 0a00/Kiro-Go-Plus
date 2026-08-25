package proxy

import (
	"context"
	"errors"
	"io"
	"kiro-go/config"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestUsageMetadataPermissionFailureMatchesProductionBuilderIDError(t *testing.T) {
	err := classifyUpstreamHTTPError(http.StatusForbidden, "GetUsageLimits", []byte(`{"message":"User is not authorized to make this call."}`))
	cases := []config.Account{
		{AuthMethod: "idc", Provider: "BuilderId"},
		{AuthMethod: "builder_id"},
		{AuthMethod: "aws_sso"},
	}
	for _, account := range cases {
		if !isUsageLimitsUnavailable(&account, err) {
			t.Fatalf("expected metadata-only classification for %+v: %v", account, err)
		}
	}
}

func TestBuilderIDUsage403BecomesPartialRefresh(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	var calls atomic.Int32
	kiroRestHttpStore.Store(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		if req.URL.Path != "/getUsageLimits" {
			t.Fatalf("unexpected refresh path: %s", req.URL.Path)
		}
		return &http.Response{
			StatusCode: http.StatusForbidden,
			Body:       io.NopCloser(strings.NewReader(`{"message":"User is not authorized to make this call."}`)),
			Header:     make(http.Header),
		}, nil
	})})
	t.Cleanup(func() { InitKiroHttpClient("") })

	account := &config.Account{
		ID: "builder-refresh", Enabled: true, AccessToken: "access-token",
		AuthMethod: "idc", Provider: "BuilderId", Region: "us-east-1",
	}
	h := &Handler{backgroundCtx: context.Background(), autoRefreshFail: make(map[string]int64)}
	result := h.refreshOneAccount(account, config.AutoRefreshConfig{
		IntervalMinutes: 10, TokenRefreshBeforeSeconds: 1800,
		RefreshTaskTimeoutSeconds: 30,
	})
	if result.Status != "partial" || result.Info == nil || !result.Info.MetadataUnavailable {
		t.Fatalf("Builder ID usage 403 should be partial, got %+v", result)
	}
	if result.CooldownUntil != 0 {
		t.Fatalf("metadata-only refresh must not create cooldown: %+v", result)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("expected one usage request without region probing, got %d", got)
	}
}

func TestUsageMetadataPermissionFailureIsSoftForIDC(t *testing.T) {
	err := &UpstreamError{
		Kind:     UpstreamErrorForbidden,
		Endpoint: "GetUsageLimits",
		Message:  "User is not authorized to make this call",
	}
	if !isUsageLimitsUnavailable(&config.Account{AuthMethod: "idc"}, err) {
		t.Fatal("expected IDC usage permission failure to be metadata-only")
	}
}

func TestUsageMetadataPermissionFailureDoesNotHideEnterpriseRegionFallback(t *testing.T) {
	err := &UpstreamError{
		Kind:     UpstreamErrorForbidden,
		Endpoint: "GetUsageLimits",
		Message:  "not available in this region",
	}
	if isUsageLimitsUnavailable(&config.Account{AuthMethod: "idc", Provider: "Enterprise"}, err) {
		t.Fatal("enterprise regional 403 must remain eligible for region fallback")
	}
}

func TestUsageMetadataTransientFailureRemainsRetryable(t *testing.T) {
	err := &UpstreamError{
		Kind:     UpstreamErrorTransient,
		Endpoint: "GetUsageLimits",
		Cause:    errors.New("temporary upstream failure"),
	}
	if isUsageLimitsUnavailable(&config.Account{AuthMethod: "social"}, err) {
		t.Fatal("transient usage metadata failure must remain retryable")
	}
}

func TestUsageMetadataAuthFailureIsNotSoft(t *testing.T) {
	err := &UpstreamError{
		Kind:     UpstreamErrorAuthRevoked,
		Endpoint: "GetUsageLimits",
		Message:  "invalid_grant",
	}
	if isUsageLimitsUnavailable(&config.Account{AuthMethod: "idc"}, err) {
		t.Fatal("revoked credentials must remain a hard refresh failure")
	}
}

func TestUsageMetadataPermissionFailureStopsRegionFallback(t *testing.T) {
	err := &UpstreamError{
		Kind:     UpstreamErrorForbidden,
		Endpoint: "GetUsageLimits",
		Message:  "User is not authorized to make this call",
	}
	if shouldTryNextManagementRegion(nil, &config.Account{AuthMethod: "idc"}, err) {
		t.Fatal("known optional permission failure should not probe every region")
	}
}
