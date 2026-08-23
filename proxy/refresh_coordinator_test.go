package proxy

import (
	"context"
	"errors"
	"io"
	"kiro-go/auth"
	"kiro-go/config"
	"kiro-go/internal/outboundipv6"
	accountpool "kiro-go/pool"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestClassifyRefreshFailureDistinguishesRevokedAndTransientErrors(t *testing.T) {
	tests := []struct {
		name          string
		err           error
		wantKind      UpstreamErrorKind
		retryAccounts bool
		blocked       bool
	}{
		{name: "invalid grant", err: errors.New(`refresh failed: 400 {"error":"invalid_grant"}`), wantKind: UpstreamErrorAuthRevoked, retryAccounts: true},
		{name: "bad credentials", err: errors.New("refresh failed: 401 Bad credentials"), wantKind: UpstreamErrorAuthRevoked, retryAccounts: true},
		{name: "invalid token", err: errors.New(`refresh failed: 400 {"error":"invalid_token"}`), wantKind: UpstreamErrorAuthRevoked, retryAccounts: true},
		{name: "typed rejected credential", err: &auth.RefreshHTTPError{StatusCode: 400, CredentialRejected: true}, wantKind: UpstreamErrorAuthRevoked, retryAccounts: true},
		{name: "typed auth mismatch only", err: &auth.RefreshHTTPError{StatusCode: 400, AuthenticationMismatch: true}, wantKind: UpstreamErrorTransient, retryAccounts: true},
		{name: "server error", err: errors.New("refresh failed: 503 service unavailable"), wantKind: UpstreamErrorTransient, retryAccounts: true},
		{name: "network timeout", err: context.DeadlineExceeded, wantKind: UpstreamErrorTransient, retryAccounts: true},
		{name: "cloudfront block", err: auth.ErrRefreshUpstreamBlocked, wantKind: UpstreamErrorEndpointUnavailable, retryAccounts: true, blocked: true},
		{name: "local IPv6 bind", err: outboundipv6.WrapBindError("2001:db8::1", errors.New("cannot assign requested address")), wantKind: UpstreamErrorLocalConfiguration, retryAccounts: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyRefreshFailure("token_refresh", tc.err)
			if got.Kind != tc.wantKind || got.RetryAcrossAccounts != tc.retryAccounts {
				t.Fatalf("classification = %+v, want kind=%s retryAccounts=%v", got, tc.wantKind, tc.retryAccounts)
			}
			if auth.IsRefreshUpstreamBlocked(got) != tc.blocked {
				t.Fatalf("blocked classification = %v, want %v", auth.IsRefreshUpstreamBlocked(got), tc.blocked)
			}
		})
	}
}

func TestBackgroundRefreshDisablesRevokedCredentials(t *testing.T) {
	account := setupRefreshAccountTest(t, "background-revoked")
	installRefreshResponseServer(t, http.StatusBadRequest, `{"error":"invalid_grant","error_description":"refresh token revoked"}`)
	h := &Handler{
		pool: accountpool.GetPool(), backgroundCtx: context.Background(), autoRefreshFail: make(map[string]int64),
	}
	result := h.refreshOneAccount(&account, config.GetAutoRefreshConfig())
	if result.Status != "failed" {
		t.Fatalf("background refresh result = %+v", result)
	}
	assertRefreshAccountEnabled(t, account.ID, false)
	upstreamErr, ok := asUpstreamError(result.Err)
	if !ok || upstreamErr.Kind != UpstreamErrorAuthRevoked {
		t.Fatalf("revoked refresh was not structured: %#v", result.Err)
	}
}

func TestManualRefreshDisablesRevokedCredentials(t *testing.T) {
	account := setupRefreshAccountTest(t, "manual-revoked")
	installRefreshResponseServer(t, http.StatusUnauthorized, `{"message":"Bad credentials"}`)
	h := &Handler{pool: accountpool.GetPool(), backgroundCtx: context.Background()}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/admin/api/accounts/"+account.ID+"/refresh", nil)
	h.apiRefreshAccount(recorder, request, account.ID)
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("manual refresh status = %d, want 500", recorder.Code)
	}
	assertRefreshAccountEnabled(t, account.ID, false)
}

func TestBackgroundRefreshKeepsAccountEnabledOnTransientFailure(t *testing.T) {
	account := setupRefreshAccountTest(t, "background-transient")
	installRefreshResponseServer(t, http.StatusServiceUnavailable, `{"message":"temporary outage"}`)
	h := &Handler{
		pool: accountpool.GetPool(), backgroundCtx: context.Background(), autoRefreshFail: make(map[string]int64),
	}
	result := h.refreshOneAccount(&account, config.GetAutoRefreshConfig())
	if result.Status != "failed" {
		t.Fatalf("background refresh result = %+v", result)
	}
	assertRefreshAccountEnabled(t, account.ID, true)
	upstreamErr, ok := asUpstreamError(result.Err)
	if !ok || upstreamErr.Kind != UpstreamErrorTransient {
		t.Fatalf("transient refresh was not structured: %#v", result.Err)
	}
}

func TestRefreshPreservesAdminDisableDuringInFlightRequest(t *testing.T) {
	account := setupRefreshAccountTest(t, "refresh-disable-race")
	coordinator, started, release, _ := installDelayedRefreshServer(t)
	copy := account
	done := make(chan error, 1)
	go func() { done <- coordinator.Refresh(&copy, true) }()
	<-started

	enabled := false
	status := "DISABLED"
	reason := "disabled by operator"
	if _, err := config.PatchAccountStatus(account.ID, config.AccountStatusPatch{
		Enabled: &enabled, BanStatus: &status, BanReason: &reason,
	}); err != nil {
		t.Fatalf("disable account: %v", err)
	}
	accountpool.GetPool().Reload()
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("refresh failed: %v", err)
	}

	persisted := refreshTestAccount(t, account.ID)
	if persisted.Enabled || persisted.BanReason != reason || persisted.AccessToken != "new-access" || persisted.RefreshToken != "new-refresh" {
		t.Fatalf("in-flight refresh replaced admin state or lost credentials: %+v", persisted)
	}
}

func TestRefreshDoesNotResurrectDeletedAccount(t *testing.T) {
	account := setupRefreshAccountTest(t, "refresh-delete-race")
	coordinator, started, release, _ := installDelayedRefreshServer(t)
	copy := account
	done := make(chan error, 1)
	go func() { done <- coordinator.Refresh(&copy, true) }()
	<-started
	if err := config.DeleteAccount(account.ID); err != nil {
		t.Fatalf("delete account: %v", err)
	}
	accountpool.GetPool().Reload()
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("refresh failed after deletion: %v", err)
	}
	if len(config.GetAccounts()) != 0 || accountpool.GetPool().GetByID(account.ID) != nil {
		t.Fatalf("deleted account was resurrected: config=%+v pool=%+v", config.GetAccounts(), accountpool.GetPool().GetByID(account.ID))
	}
}

func TestTokenRefreshSingleFlightSurvivesPoolReload(t *testing.T) {
	account := setupRefreshAccountTest(t, "refresh-reload-singleflight")
	coordinator, started, release, requests := installDelayedRefreshServer(t)
	const callers = 12
	start := make(chan struct{})
	errs := make(chan error, callers)
	var ready sync.WaitGroup
	ready.Add(callers)
	for i := 0; i < callers; i++ {
		go func() {
			copy := account
			ready.Done()
			<-start
			errs <- coordinator.Refresh(&copy, true)
		}()
	}
	ready.Wait()
	close(start)
	<-started
	for i := 0; i < 5; i++ {
		accountpool.GetPool().Reload()
		time.Sleep(5 * time.Millisecond)
	}
	close(release)
	for i := 0; i < callers; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("shared refresh failed: %v", err)
		}
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("pool reload broke single-flight: upstream requests=%d", got)
	}
}

func setupRefreshAccountTest(t *testing.T, id string) config.Account {
	t.Helper()
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	account := config.Account{
		ID: id, Email: id + "@example.test", Enabled: true, AuthMethod: "idc", Region: "us-east-1",
		AccessToken: "old-access", RefreshToken: "old-refresh", ExpiresAt: time.Now().Add(-time.Hour).Unix(),
		ClientID: "client-id", ClientSecret: "client-secret",
	}
	if err := config.AddAccount(account); err != nil {
		t.Fatalf("add account: %v", err)
	}
	accountpool.GetPool().Reload()
	return account
}

func installRefreshResponseServer(t *testing.T, status int, body string) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)
	installRefreshTestHooks(t, server.Client(), func(string) string { return server.URL })
}

func installDelayedRefreshServer(t *testing.T) (*tokenRefreshCoordinator, <-chan struct{}, chan struct{}, *atomic.Int32) {
	t.Helper()
	started := make(chan struct{})
	release := make(chan struct{})
	requests := &atomic.Int32{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if requests.Add(1) == 1 {
			close(started)
		}
		<-release
		_, _ = io.WriteString(w, `{"accessToken":"new-access","refreshToken":"new-refresh","expiresIn":3600,"profileArn":"new-profile"}`)
	}))
	t.Cleanup(server.Close)
	installRefreshTestHooks(t, server.Client(), func(string) string { return server.URL })
	return &tokenRefreshCoordinator{inFlight: make(map[string]*coordinatedRefreshCall), notify: make(chan struct{})}, started, release, requests
}

func installRefreshTestHooks(t *testing.T, client *http.Client, tokenURL func(string) string) {
	t.Helper()
	oldURL := auth.GetOIDCTokenURLForTest()
	oldClient := auth.SetGlobalAuthClientForTest(client)
	oldCoordinator := sharedTokenRefreshCoordinator
	sharedTokenRefreshCoordinator = &tokenRefreshCoordinator{inFlight: make(map[string]*coordinatedRefreshCall), notify: make(chan struct{})}
	auth.SetOIDCTokenURLForTest(tokenURL)
	t.Cleanup(func() {
		auth.SetOIDCTokenURLForTest(oldURL)
		auth.SetGlobalAuthClientForTest(oldClient)
		sharedTokenRefreshCoordinator = oldCoordinator
	})
}

func assertRefreshAccountEnabled(t *testing.T, id string, want bool) {
	t.Helper()
	if account := refreshTestAccount(t, id); account.Enabled != want {
		t.Fatalf("account enabled=%v, want %v: %+v", account.Enabled, want, account)
	}
}

func refreshTestAccount(t *testing.T, id string) config.Account {
	t.Helper()
	for _, account := range config.GetAccounts() {
		if account.ID == id {
			return account
		}
	}
	t.Fatalf("account %q not found", id)
	return config.Account{}
}
