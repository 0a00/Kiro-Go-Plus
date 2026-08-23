package proxy

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"kiro-go/auth"
	"kiro-go/config"
	accountpool "kiro-go/pool"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

// installCleanAuthClient replaces the global auth HTTP client with one whose
// Transport does not consult http.ProxyFromEnvironment — that function caches
// env vars on first call and would otherwise poison TestBuildKiroTransport*
// when tests run in the default order. Returns a cleanup that restores the
// previous client.
func installCleanAuthClient(t *testing.T) func() {
	t.Helper()
	c := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{}}
	prev := auth.SetGlobalAuthClientForTest(c)
	return func() { auth.SetGlobalAuthClientForTest(prev) }
}

// TestApiImportCredentialsRejectsWhenRefreshFails verifies the regression:
// previously, when auth.RefreshToken failed and the user supplied an accessToken,
// the handler stored that accessToken with ExpiresAt = now+300, producing an
// account that the pool would skip (Pick uses now > ExpiresAt-120 → ~3 min) and
// that the on-demand refresh path could never repair (Pick filters it out before
// ensureValidToken runs). The fix is to reject the import outright; the caller
// must provide a refreshToken that actually works.
func TestApiImportCredentialsRejectsWhenRefreshFails(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	defer installCleanAuthClient(t)()

	// Stand up a fake OIDC endpoint that always 400s, simulating an unreachable
	// or invalid refresh.
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
	}))
	defer fake.Close()

	oldOIDC := authOidcURL()
	auth.SetOIDCTokenURLForTest(func(string) string { return fake.URL })
	defer auth.SetOIDCTokenURLForTest(oldOIDC)

	h := &Handler{pool: accountpool.GetPool()}

	body := `{"refreshToken":"rt-broken","accessToken":"at-still-valid-elsewhere","clientId":"c","clientSecret":"s","authMethod":"idc","region":"us-east-1"}`
	req := httptest.NewRequest("POST", "/auth/credentials", strings.NewReader(body))
	rec := httptest.NewRecorder()

	h.apiImportCredentials(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 when refresh fails, got %d body=%s", rec.Code, rec.Body.String())
	}

	var resp map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !strings.Contains(resp["error"], "Token refresh failed") {
		t.Fatalf("expected refresh-failed error, got %q", resp["error"])
	}

	// Crucial: no account should have been created. The previous bug stored a
	// half-broken account with ExpiresAt ~now+300 that would die in 3 minutes.
	if accs := config.GetAccounts(); len(accs) != 0 {
		t.Fatalf("expected no accounts to be persisted on failed import, got %+v", accs)
	}
}

// TestApiImportCredentialsUsesUpstreamExpiresAt verifies the happy path: when
// refresh succeeds, the persisted ExpiresAt reflects the upstream expiresIn,
// not a hard-coded 300s.
func TestApiImportCredentialsUsesUpstreamExpiresAt(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	defer installCleanAuthClient(t)()

	const upstreamExpiresIn = 3600
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"accessToken":"at-new","refreshToken":"rt-rotated","expiresIn":%d,"profileArn":"arn:aws:codewhisperer:profile/test"}`, upstreamExpiresIn)
	}))
	defer fake.Close()

	oldOIDC := authOidcURL()
	auth.SetOIDCTokenURLForTest(func(string) string { return fake.URL })
	defer auth.SetOIDCTokenURLForTest(oldOIDC)

	h := &Handler{pool: accountpool.GetPool()}

	before := time.Now().Unix()
	body := `{"refreshToken":"rt-good","clientId":"c","clientSecret":"s","authMethod":"idc","region":"us-east-1"}`
	req := httptest.NewRequest("POST", "/auth/credentials", strings.NewReader(body))
	rec := httptest.NewRecorder()

	h.apiImportCredentials(rec, req)
	after := time.Now().Unix()

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 on successful refresh, got %d body=%s", rec.Code, rec.Body.String())
	}

	accs := config.GetAccounts()
	if len(accs) != 1 {
		t.Fatalf("expected exactly one account persisted, got %d", len(accs))
	}
	got := accs[0]
	if got.AccessToken != "at-new" {
		t.Fatalf("expected upstream-issued accessToken, got %q", got.AccessToken)
	}
	if got.RefreshToken != "rt-rotated" {
		t.Fatalf("expected rotated refreshToken to be persisted, got %q", got.RefreshToken)
	}
	// Allow ±5s of drift but require the value to clearly come from upstream's
	// expiresIn rather than the old 300s fallback.
	expectMin := before + upstreamExpiresIn - 5
	expectMax := after + upstreamExpiresIn + 5
	if got.ExpiresAt < expectMin || got.ExpiresAt > expectMax {
		t.Fatalf("expected ExpiresAt ≈ now+%d ([%d..%d]), got %d", upstreamExpiresIn, expectMin, expectMax, got.ExpiresAt)
	}
	if got.ExpiresAt-time.Now().Unix() < 1500 {
		t.Fatalf("ExpiresAt too short — looks like the 300s fallback is still in play: %d (delta %d)", got.ExpiresAt, got.ExpiresAt-time.Now().Unix())
	}
}

func TestApiImportCredentialsAcceptsLoginHelperSnakeCase(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	defer installCleanAuthClient(t)()

	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"accessToken":"helper-access","refreshToken":"helper-rotated","expiresIn":3600}`)
	}))
	defer fake.Close()
	oldOIDC := authOidcURL()
	auth.SetOIDCTokenURLForTest(func(region string) string {
		if region != "eu-north-1" {
			t.Errorf("refresh region = %q, want eu-north-1", region)
		}
		return fake.URL
	})
	defer auth.SetOIDCTokenURLForTest(oldOIDC)

	body := `{
		"access_token":"helper-old-access",
		"refresh_token":"helper-refresh",
		"client_id":"helper-client",
		"client_secret":"helper-secret",
		"auth_method":"idc",
		"region":"eu-north-1",
		"start_url":"https://example.awsapps.com/start",
		"profile_arn":"arn:aws:codewhisperer:eu-north-1:123:profile/helper",
		"username":"helper@example.com",
		"disabled":false,
		"type":"kiro"
	}`
	rec := httptest.NewRecorder()
	(&Handler{pool: accountpool.GetPool()}).apiImportCredentials(
		rec, httptest.NewRequest(http.MethodPost, "/auth/credentials", strings.NewReader(body)),
	)
	if rec.Code != http.StatusOK {
		t.Fatalf("snake_case import status = %d body=%s", rec.Code, rec.Body.String())
	}
	accounts := config.GetAccounts()
	if len(accounts) != 1 {
		t.Fatalf("account count = %d, want 1", len(accounts))
	}
	account := accounts[0]
	if account.Email != "helper@example.com" || account.AuthMethod != "idc" ||
		account.ClientID != "helper-client" || account.ClientSecret != "helper-secret" ||
		account.StartUrl != "https://example.awsapps.com/start" || account.Region != "eu-north-1" || !account.Enabled {
		t.Fatalf("helper fields were not normalized: %+v", account)
	}
}

func TestApiImportCredentialsRejectsDuplicateBatchBeforeRefresh(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	body := `[
		{"refresh_token":"same-token","client_id":"one","auth_method":"external_idp","token_endpoint":"https://login.microsoftonline.com/t/oauth2/v2.0/token"},
		{"refreshToken":"same-token","clientId":"two","authMethod":"external_idp","tokenEndpoint":"https://login.microsoftonline.com/t/oauth2/v2.0/token"}
	]`
	rec := httptest.NewRecorder()
	(&Handler{pool: accountpool.GetPool()}).apiImportCredentials(
		rec, httptest.NewRequest(http.MethodPost, "/auth/credentials", strings.NewReader(body)),
	)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "same credential") {
		t.Fatalf("duplicate batch status = %d body=%s", rec.Code, rec.Body.String())
	}
	if accounts := config.GetAccounts(); len(accounts) != 0 {
		t.Fatalf("duplicate batch persisted accounts: %+v", accounts)
	}
}

func TestApiImportCredentialsRejectsOversizedBody(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	rec := httptest.NewRecorder()
	(&Handler{pool: accountpool.GetPool()}).apiImportCredentials(
		rec,
		httptest.NewRequest(http.MethodPost, "/auth/credentials", strings.NewReader(strings.Repeat(" ", int(maxRequestBodyBytes)+1))),
	)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized import status = %d, want 413", rec.Code)
	}
}

func TestApiImportCredentialsAcceptsNestedPowerExport(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	defer installCleanAuthClient(t)()

	const upstreamExpiresIn = 3600
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"accessToken":"at-new","refreshToken":"rt-rotated","expiresIn":%d,"profileArn":"arn:aws:codewhisperer:profile/test"}`, upstreamExpiresIn)
	}))
	defer fake.Close()

	oldOIDC := authOidcURL()
	auth.SetOIDCTokenURLForTest(func(string) string { return fake.URL })
	defer auth.SetOIDCTokenURLForTest(oldOIDC)

	h := &Handler{pool: accountpool.GetPool()}

	body, err := json.Marshal(map[string]interface{}{
		"id":        "export-id",
		"email":     "power@example.com",
		"idp":       "BuilderId",
		"userId":    "user-1",
		"machineId": "machine-1",
		"status":    "active",
		"credentials": map[string]interface{}{
			"accessToken":  "at-exported",
			"refreshToken": "rt-good",
			"clientId":     "client",
			"clientSecret": "secret",
			"region":       "us-east-1",
			"expiresAt":    int64(1710000000),
			"authMethod":   "idc",
		},
		"subscription": map[string]interface{}{
			"type":  "power",
			"title": "Power",
		},
		"usage": map[string]interface{}{
			"current":     1.5,
			"limit":       100.0,
			"percentUsed": 0.015,
			"lastUpdated": int64(1710000001),
		},
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	req := httptest.NewRequest("POST", "/auth/credentials", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()

	h.apiImportCredentials(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 on nested Power export import, got %d body=%s", rec.Code, rec.Body.String())
	}

	accs := config.GetAccounts()
	if len(accs) != 1 {
		t.Fatalf("expected exactly one account persisted, got %d", len(accs))
	}
	got := accs[0]
	if got.ID == "export-id" {
		t.Fatalf("expected a fresh local account ID, got exported ID %q", got.ID)
	}
	if got.AccessToken != "at-new" {
		t.Fatalf("expected upstream-issued accessToken, got %q", got.AccessToken)
	}
	if got.RefreshToken != "rt-rotated" {
		t.Fatalf("expected rotated refreshToken to be persisted, got %q", got.RefreshToken)
	}
	if got.Email != "power@example.com" {
		t.Fatalf("expected imported email to override user-info lookup, got %q", got.Email)
	}
	if got.UserId != "user-1" {
		t.Fatalf("expected imported userId, got %q", got.UserId)
	}
	if got.Provider != "BuilderId" {
		t.Fatalf("expected idp to become provider, got %q", got.Provider)
	}
	if got.AuthMethod != "idc" || got.ClientID != "client" || got.ClientSecret != "secret" || got.Region != "us-east-1" {
		t.Fatalf("expected nested credentials to be preserved, got auth=%q client=%q secret=%q region=%q", got.AuthMethod, got.ClientID, got.ClientSecret, got.Region)
	}
	if got.MachineId != "machine-1" {
		t.Fatalf("expected imported machineId, got %q", got.MachineId)
	}
	if !got.Enabled || got.BanStatus != "ACTIVE" {
		t.Fatalf("expected active account status, got enabled=%v banStatus=%q", got.Enabled, got.BanStatus)
	}
	if got.SubscriptionType != "POWER" || got.SubscriptionTitle != "Power" {
		t.Fatalf("expected Power subscription metadata, got type=%q title=%q", got.SubscriptionType, got.SubscriptionTitle)
	}
	if got.UsageCurrent != 1.5 || got.UsageLimit != 100 || got.UsagePercent != 0.015 || got.LastRefresh != 1710000001 {
		t.Fatalf("expected usage metadata to be preserved, got current=%v limit=%v percent=%v last=%d", got.UsageCurrent, got.UsageLimit, got.UsagePercent, got.LastRefresh)
	}
}

func TestApiImportCredentialsAcceptsExportEnvelope(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}

	h := &Handler{pool: accountpool.GetPool()}

	body := `{
		"version": 1,
		"exportedAt": "2026-06-09T00:00:00Z",
		"accounts": [{
			"email": "power-key@example.com",
			"idp": "API Key",
			"machineId": "machine-envelope",
			"status": "active",
			"credentials": {
				"kiroApiKey": "kiro-direct-key",
				"authMethod": "api_key",
				"region": "us-east-1"
			},
			"subscription": {
				"type": "power",
				"title": "KIRO POWER"
			},
			"usage": {
				"current": 50,
				"limit": 100,
				"percentUsed": 0.5,
				"lastUpdated": 1710000002
			}
		}]
	}`
	req := httptest.NewRequest("POST", "/auth/credentials", strings.NewReader(body))
	rec := httptest.NewRecorder()

	h.apiImportCredentials(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 on export envelope import, got %d body=%s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Success  bool                     `json:"success"`
		Accounts []map[string]interface{} `json:"accounts"`
		Errors   []string                 `json:"errors"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !resp.Success || len(resp.Accounts) != 1 || len(resp.Errors) != 0 {
		t.Fatalf("expected one successful import, got success=%v accounts=%d errors=%v", resp.Success, len(resp.Accounts), resp.Errors)
	}

	accs := config.GetAccounts()
	if len(accs) != 1 {
		t.Fatalf("expected exactly one account persisted, got %d", len(accs))
	}
	got := accs[0]
	if got.Email != "power-key@example.com" || got.AuthMethod != "api_key" || got.KiroApiKey != "kiro-direct-key" {
		t.Fatalf("expected nested API key account metadata, got email=%q auth=%q key=%q", got.Email, got.AuthMethod, got.KiroApiKey)
	}
	if got.SubscriptionType != "POWER" || got.SubscriptionTitle != "KIRO POWER" {
		t.Fatalf("expected Power subscription metadata, got type=%q title=%q", got.SubscriptionType, got.SubscriptionTitle)
	}
	if got.UsageCurrent != 50 || got.UsageLimit != 100 || got.UsagePercent != 0.5 || got.LastRefresh != 1710000002 {
		t.Fatalf("expected usage metadata to be preserved, got current=%v limit=%v percent=%v last=%d", got.UsageCurrent, got.UsageLimit, got.UsagePercent, got.LastRefresh)
	}
}

func TestApiImportCredentialsUpdatesDuplicateIdentity(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}

	h := &Handler{pool: accountpool.GetPool()}

	first := `{"email":"dup@example.com","provider":"API Key","userId":"user-dup","authMethod":"api_key","kiroApiKey":"first-key","region":"us-east-1","machineId":"machine-dup"}`
	req := httptest.NewRequest("POST", "/auth/credentials", strings.NewReader(first))
	rec := httptest.NewRecorder()
	h.apiImportCredentials(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("first import failed: %d body=%s", rec.Code, rec.Body.String())
	}
	firstAccount := config.GetAccounts()[0]

	second := `{"email":"dup@example.com","provider":"API Key","userId":"user-dup","authMethod":"api_key","kiroApiKey":"second-key","region":"us-east-1","machineId":"machine-dup","subscription":{"type":"power","title":"KIRO POWER"}}`
	req = httptest.NewRequest("POST", "/auth/credentials", strings.NewReader(second))
	rec = httptest.NewRecorder()
	h.apiImportCredentials(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("second import failed: %d body=%s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Action  string `json:"action"`
		Account struct {
			ID     string `json:"id"`
			Action string `json:"action"`
		} `json:"account"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Action != "updated" || resp.Account.Action != "updated" {
		t.Fatalf("expected updated action, got root=%q account=%q", resp.Action, resp.Account.Action)
	}

	accs := config.GetAccounts()
	if len(accs) != 1 {
		t.Fatalf("expected duplicate import to update one account, got %d", len(accs))
	}
	got := accs[0]
	if got.ID != firstAccount.ID {
		t.Fatalf("expected existing ID to be preserved, got %q want %q", got.ID, firstAccount.ID)
	}
	if got.KiroApiKey != "second-key" || got.AccessToken != "second-key" {
		t.Fatalf("expected key to be updated, got key=%q token=%q", got.KiroApiKey, got.AccessToken)
	}
	if got.SubscriptionType != "POWER" || got.SubscriptionTitle != "KIRO POWER" {
		t.Fatalf("expected subscription metadata to update, got type=%q title=%q", got.SubscriptionType, got.SubscriptionTitle)
	}
}

func TestApiImportCredentialsAcceptsKiroAPIKeyWithoutRefreshToken(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}

	h := &Handler{pool: accountpool.GetPool()}

	body := `{"authMethod":"api_key","kiroApiKey":"kiro-direct-key","region":"us-east-1"}`
	req := httptest.NewRequest("POST", "/auth/credentials", strings.NewReader(body))
	rec := httptest.NewRecorder()

	h.apiImportCredentials(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 on API key import, got %d body=%s", rec.Code, rec.Body.String())
	}

	accs := config.GetAccounts()
	if len(accs) != 1 {
		t.Fatalf("expected exactly one account persisted, got %d", len(accs))
	}
	got := accs[0]
	if got.AuthMethod != "api_key" {
		t.Fatalf("expected authMethod api_key, got %q", got.AuthMethod)
	}
	if got.KiroApiKey != "kiro-direct-key" || got.AccessToken != "kiro-direct-key" {
		t.Fatalf("expected direct key to be stored as kiroApiKey/accessToken, got key=%q token=%q", got.KiroApiKey, got.AccessToken)
	}
	if got.RefreshToken != "" {
		t.Fatalf("expected refreshToken to stay empty for API key account, got %q", got.RefreshToken)
	}
	if got.ExpiresAt != 0 {
		t.Fatalf("expected API key account to have no token expiry, got %d", got.ExpiresAt)
	}
	if got.Email != "api-key--key" {
		t.Fatalf("expected masked API key label, got %q", got.Email)
	}
}

func TestApiImportCredentialsRecoversEnterpriseIDCMetadata(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	defer installCleanAuthClient(t)()

	serialized := `{"initiateLoginUri":"https://d-1234567890.awsapps.com/start/"}`
	secretPayload := `{"serialized":` + strconv.Quote(serialized) + `}`
	clientSecret := "header." + base64.RawURLEncoding.EncodeToString([]byte(secretPayload)) + ".signature"
	accessPayload := `{"email":"enterprise@example.com"}`
	accessToken := "header." + base64.RawURLEncoding.EncodeToString([]byte(accessPayload)) + ".signature"

	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"accessToken":  accessToken,
			"refreshToken": "rotated-refresh",
			"expiresIn":    3600,
		})
	}))
	defer fake.Close()

	oldOIDC := authOidcURL()
	auth.SetOIDCTokenURLForTest(func(region string) string {
		if region != "eu-north-1" {
			t.Errorf("OIDC refresh region = %q, want eu-north-1", region)
		}
		return fake.URL
	})
	defer auth.SetOIDCTokenURLForTest(oldOIDC)

	body := fmt.Sprintf(`{
		"credentials": {
			"refreshToken": "refresh-token",
			"clientId": "client-id",
			"clientSecret": %s,
			"authMethod": "idc",
			"region": "us-east-1",
			"authRegion": "eu-north-1"
		}
	}`, strconv.Quote(clientSecret))
	rec := httptest.NewRecorder()
	(&Handler{pool: accountpool.GetPool()}).apiImportCredentials(
		rec,
		httptest.NewRequest(http.MethodPost, "/auth/credentials", strings.NewReader(body)),
	)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 on enterprise import, got %d body=%s", rec.Code, rec.Body.String())
	}
	accounts := config.GetAccounts()
	if len(accounts) != 1 {
		t.Fatalf("account count = %d, want 1", len(accounts))
	}
	account := accounts[0]
	if account.Region != "eu-north-1" || account.StartUrl != "https://d-1234567890.awsapps.com/start" {
		t.Fatalf("enterprise IDC metadata was not retained: %+v", account)
	}
	if account.Provider != "Enterprise" || account.AuthMethod != "idc" {
		t.Fatalf("unexpected enterprise classification: method=%q provider=%q", account.AuthMethod, account.Provider)
	}
}

func TestApiKeyCredentialFingerprintPreventsMaskedLabelCollision(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	h := &Handler{pool: accountpool.GetPool()}

	for _, key := range []string{"first-prefix-shared", "second-prefix-shared"} {
		body := `{"authMethod":"api_key","kiroApiKey":"` + key + `"}`
		rec := httptest.NewRecorder()
		h.apiImportCredentials(rec, httptest.NewRequest(http.MethodPost, "/auth/credentials", strings.NewReader(body)))
		if rec.Code != http.StatusOK {
			t.Fatalf("import %q failed: %d body=%s", key, rec.Code, rec.Body.String())
		}
	}

	accounts := config.GetAccounts()
	if len(accounts) != 2 {
		t.Fatalf("expected colliding display labels to remain distinct, got %d accounts", len(accounts))
	}
	if accounts[0].Email != accounts[1].Email {
		t.Fatalf("test requires identical masked labels, got %q and %q", accounts[0].Email, accounts[1].Email)
	}
	if accounts[0].CredentialFingerprint == "" || accounts[0].CredentialFingerprint == accounts[1].CredentialFingerprint {
		t.Fatalf("expected distinct stable fingerprints: %+v", accounts)
	}
}

func TestEnsureValidTokenNoopsForKiroAPIKey(t *testing.T) {
	account := &config.Account{
		ID:           "api-key-account",
		AuthMethod:   "api_key",
		KiroApiKey:   "kiro-direct-key",
		RefreshToken: "should-be-cleared",
		ExpiresAt:    time.Now().Unix() - 1,
	}

	h := &Handler{}
	if err := h.ensureValidToken(account); err != nil {
		t.Fatalf("ensureValidToken: %v", err)
	}

	if account.AccessToken != "kiro-direct-key" {
		t.Fatalf("expected accessToken to be restored from kiroApiKey, got %q", account.AccessToken)
	}
	if account.RefreshToken != "" {
		t.Fatalf("expected refreshToken to be cleared, got %q", account.RefreshToken)
	}
	if account.ExpiresAt != 0 {
		t.Fatalf("expected expiresAt to be cleared, got %d", account.ExpiresAt)
	}
}

func TestNormalizeImportAuthMethod(t *testing.T) {
	tests := []struct {
		name          string
		authMethod    string
		provider      string
		refreshToken  string
		clientID      string
		clientSecret  string
		kiroAPIKey    string
		tokenEndpoint string
		issuerURL     string
		want          string
	}{
		{name: "explicit", authMethod: "external_idp", clientID: "client", want: "external_idp"},
		{name: "alias", authMethod: "Entra-ID", clientID: "client", want: "external_idp"},
		{name: "endpoint", clientID: "client", tokenEndpoint: "https://login.microsoftonline.com/t/oauth2/v2.0/token", want: "external_idp"},
		{name: "issuer", clientID: "client", issuerURL: "https://login.microsoftonline.com/t/v2.0", want: "external_idp"},
		{name: "provider", provider: "Microsoft", clientID: "client", want: "external_idp"},
		{name: "api key wins", authMethod: "external_idp", kiroAPIKey: "key", want: "api_key"},
		{name: "idc remains idc", authMethod: "idc", clientID: "client", clientSecret: "secret", tokenEndpoint: "https://login.microsoftonline.com/t/oauth2/v2.0/token", want: "idc"},
		{name: "social remains social", authMethod: "social", tokenEndpoint: "https://login.microsoftonline.com/t/oauth2/v2.0/token", want: "social"},
		{name: "generic social BuilderId conflict", authMethod: "social", provider: "BuilderId", refreshToken: "refresh", clientID: "client", clientSecret: "secret", want: "idc"},
		{name: "generic social Enterprise conflict", authMethod: "social", provider: "Enterprise", refreshToken: "refresh", clientID: "client", clientSecret: "secret", want: "idc"},
		{name: "incomplete generic social conflict remains social", authMethod: "social", provider: "BuilderId", refreshToken: "refresh", clientID: "client", want: "social"},
		{name: "Google provider remains social with IDC fields", authMethod: "social", provider: "Google", refreshToken: "refresh", clientID: "client", clientSecret: "secret", want: "social"},
		{name: "GitHub provider overrides IDC label", authMethod: "idc", provider: "GitHub", refreshToken: "refresh", clientID: "client", clientSecret: "secret", want: "social"},
		{name: "specific social method is not generic conflict", authMethod: "google", provider: "BuilderId", refreshToken: "refresh", clientID: "client", clientSecret: "secret", want: "social"},
		{name: "external provider overrides generic social", authMethod: "social", provider: "Microsoft", refreshToken: "refresh", clientID: "client", clientSecret: "secret", want: "external_idp"},
		{name: "enterprise provider wins", provider: "Enterprise", clientID: "client", tokenEndpoint: "https://login.microsoftonline.com/t/oauth2/v2.0/token", want: "idc"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := normalizeImportAuthMethod(test.authMethod, test.provider, test.refreshToken, test.clientID, test.clientSecret, test.kiroAPIKey, test.tokenEndpoint, test.issuerURL)
			if got != test.want {
				t.Fatalf("normalized auth method = %q, want %q", got, test.want)
			}
		})
	}
}

func TestPrepareCredentialsAccountCorrectsGenericSocialIDC(t *testing.T) {
	accessPayload := fmt.Sprintf(`{"email":"corrected-idc@example.com","exp":%d}`, time.Now().Add(time.Hour).Unix())
	accessToken := "header." + base64.RawURLEncoding.EncodeToString([]byte(accessPayload)) + ".signature"
	var idcCalls, socialCalls int
	previousClient := auth.SetGlobalAuthClientForTest(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Host {
		case "oidc.import.test":
			idcCalls++
			body := fmt.Sprintf(`{"accessToken":%q,"refreshToken":"idc-rotated","expiresIn":3600}`, accessToken)
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
		default:
			socialCalls++
			return &http.Response{StatusCode: http.StatusInternalServerError, Body: io.NopCloser(strings.NewReader("unexpected Social refresh")), Header: make(http.Header)}, nil
		}
	})})
	defer auth.SetGlobalAuthClientForTest(previousClient)
	oldOIDC := authOidcURL()
	auth.SetOIDCTokenURLForTest(func(string) string { return "https://oidc.import.test/token" })
	defer auth.SetOIDCTokenURLForTest(oldOIDC)

	account, importErr := (&Handler{}).prepareCredentialsAccount(importCredentialsRequest{
		AuthMethod: "social", Provider: "BuilderId", RefreshToken: "refresh",
		ClientID: "client", ClientSecret: "secret", Region: "us-east-1",
	})
	if importErr != nil {
		t.Fatalf("prepare corrected IDC account: %v", importErr)
	}
	if idcCalls != 1 || socialCalls != 0 {
		t.Fatalf("refresh calls: IDC=%d Social=%d, want 1/0", idcCalls, socialCalls)
	}
	if account.AuthMethod != "idc" || account.Provider != "BuilderId" || account.ClientID != "client" || account.ClientSecret != "secret" {
		t.Fatalf("corrected IDC account metadata = %+v", account)
	}
}

func TestPrepareCredentialsAccountFallsBackForCorrectedSocialIDCAuthMismatch(t *testing.T) {
	accessPayload := fmt.Sprintf(`{"email":"fallback-social@example.com","exp":%d}`, time.Now().Add(time.Hour).Unix())
	accessToken := "header." + base64.RawURLEncoding.EncodeToString([]byte(accessPayload)) + ".signature"
	var idcCalls, socialCalls int
	previousClient := auth.SetGlobalAuthClientForTest(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Host {
		case "oidc.import.test":
			idcCalls++
			return &http.Response{StatusCode: http.StatusUnauthorized, Body: io.NopCloser(strings.NewReader(`{"error":"Bad credentials"}`)), Header: make(http.Header)}, nil
		case "prod.us-east-1.auth.desktop.kiro.dev":
			socialCalls++
			body := fmt.Sprintf(`{"accessToken":%q,"refreshToken":"social-rotated","expiresIn":3600,"profileArn":"social-profile"}`, accessToken)
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
		default:
			return nil, fmt.Errorf("unexpected auth host %q", req.URL.Host)
		}
	})})
	defer auth.SetGlobalAuthClientForTest(previousClient)
	oldOIDC := authOidcURL()
	auth.SetOIDCTokenURLForTest(func(string) string { return "https://oidc.import.test/token" })
	defer auth.SetOIDCTokenURLForTest(oldOIDC)

	account, importErr := (&Handler{}).prepareCredentialsAccount(importCredentialsRequest{
		AuthMethod: "social", Provider: "BuilderId", RefreshToken: "refresh",
		ClientID: "client", ClientSecret: "secret", StartUrl: "https://view.awsapps.com/start",
		Region: "us-east-1",
	})
	if importErr != nil {
		t.Fatalf("prepare fallback Social account: %v", importErr)
	}
	if idcCalls != 1 || socialCalls != 1 {
		t.Fatalf("refresh calls: IDC=%d Social=%d, want 1/1", idcCalls, socialCalls)
	}
	if account.AuthMethod != "social" || account.Provider != "Kiro SSO" || account.RefreshToken != "social-rotated" {
		t.Fatalf("fallback Social classification = %+v", account)
	}
	if account.ClientID != "" || account.ClientSecret != "" || account.StartUrl != "" {
		t.Fatalf("fallback retained contradictory IDC metadata: %+v", account)
	}
}

func TestCorrectedSocialIDCDoesNotFallbackOnTransientFailures(t *testing.T) {
	tests := []struct {
		name      string
		response  *http.Response
		transport error
	}{
		{name: "timeout", transport: context.DeadlineExceeded},
		{name: "CloudFront block", response: &http.Response{
			StatusCode: http.StatusForbidden,
			Body:       io.NopCloser(strings.NewReader("The request could not be satisfied. Request blocked. Generated by cloudfront")),
			Header:     make(http.Header),
		}},
		{name: "server error", response: &http.Response{
			StatusCode: http.StatusServiceUnavailable,
			Body:       io.NopCloser(strings.NewReader(`{"error":"invalid_request"}`)),
			Header:     make(http.Header),
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var socialCalls int
			previousClient := auth.SetGlobalAuthClientForTest(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if req.URL.Host == "prod.us-east-1.auth.desktop.kiro.dev" {
					socialCalls++
					return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"accessToken":"unexpected","expiresIn":3600}`)), Header: make(http.Header)}, nil
				}
				if test.transport != nil {
					return nil, test.transport
				}
				return test.response, nil
			})})
			defer auth.SetGlobalAuthClientForTest(previousClient)
			oldOIDC := authOidcURL()
			auth.SetOIDCTokenURLForTest(func(string) string { return "https://oidc.import.test/token" })
			defer auth.SetOIDCTokenURLForTest(oldOIDC)

			_, importErr := (&Handler{}).prepareCredentialsAccount(importCredentialsRequest{
				AuthMethod: "social", Provider: "BuilderId", RefreshToken: "refresh",
				ClientID: "client", ClientSecret: "secret", Region: "us-east-1",
			})
			if importErr == nil {
				t.Fatal("expected import refresh to fail")
			}
			if socialCalls != 0 {
				t.Fatalf("Social fallback calls = %d, want 0", socialCalls)
			}
		})
	}
}

func TestShouldFallbackIDCImportToSocial(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "typed auth mismatch", err: &auth.RefreshHTTPError{StatusCode: 401, AuthenticationMismatch: true}, want: true},
		{name: "typed non-auth failure", err: &auth.RefreshHTTPError{StatusCode: 429}, want: false},
		{name: "legacy error text is not classified", err: fmt.Errorf("refresh failed: 401 unauthorized"), want: false},
		{name: "canceled", err: context.Canceled, want: false},
		{name: "timeout", err: context.DeadlineExceeded, want: false},
		{name: "CloudFront", err: fmt.Errorf("%w: AWS OIDC", auth.ErrRefreshUpstreamBlocked), want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := shouldFallbackIDCImportToSocial(test.err); got != test.want {
				t.Fatalf("fallback classification = %v, want %v for %v", got, test.want, test.err)
			}
		})
	}
}

func TestPrepareCredentialsAccountRejectsRegionHostInjection(t *testing.T) {
	for _, test := range []struct {
		name       string
		region     string
		authRegion string
	}{
		{name: "region fragment", region: "attacker.example#"},
		{name: "region path", region: "us-east-1/path"},
		{name: "region query", region: "us-east-1?redirect=attacker"},
		{name: "auth region overrides safe region", region: "us-east-1", authRegion: "attacker.example#"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, importErr := (&Handler{}).prepareCredentialsAccount(importCredentialsRequest{
				AuthMethod: "idc", Provider: "BuilderId", RefreshToken: "refresh",
				ClientID: "client", ClientSecret: "secret", Region: test.region, AuthRegion: test.authRegion,
			})
			if importErr == nil || importErr.status != http.StatusBadRequest {
				t.Fatalf("region %q authRegion %q import error = %v, want HTTP 400", test.region, test.authRegion, importErr)
			}
		})
	}
}

func TestPrepareCredentialsAccountTrustsValidExternalIdpJWTBriefly(t *testing.T) {
	previousClient := auth.SetGlobalAuthClientForTest(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		t.Fatalf("trust-on-import should not contact token endpoint: %s", req.URL)
		return nil, fmt.Errorf("unexpected request")
	})})
	defer auth.SetGlobalAuthClientForTest(previousClient)

	issuer := "https://login.microsoftonline.com/tenant-123/v2.0"
	upstreamExpiry := time.Now().Add(2 * time.Hour).Unix()
	payload := fmt.Sprintf(`{"iss":%q,"preferred_username":"entra@example.com","exp":%d}`, issuer, upstreamExpiry)
	accessToken := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`)) + "." +
		base64.RawURLEncoding.EncodeToString([]byte(payload)) + ".signature"

	before := time.Now().Unix()
	account, importErr := (&Handler{}).prepareCredentialsAccount(importCredentialsRequest{
		AccessToken:  accessToken,
		RefreshToken: "refresh-token",
		ClientID:     "client-123",
		Region:       "eu-central-1",
	})
	after := time.Now().Unix()
	if importErr != nil {
		t.Fatalf("prepare external IdP account: %v", importErr)
	}
	if account.AuthMethod != "external_idp" || account.Provider != "AzureAD" {
		t.Fatalf("unexpected auth classification: method=%q provider=%q", account.AuthMethod, account.Provider)
	}
	if account.AccessToken != accessToken || account.RefreshToken != "refresh-token" {
		t.Fatal("trust-on-import should preserve the supplied tokens")
	}
	if account.Email != "entra@example.com" {
		t.Fatalf("email = %q, want entra@example.com", account.Email)
	}
	if account.TokenEndpoint != "https://login.microsoftonline.com/tenant-123/oauth2/v2.0/token" {
		t.Fatalf("unexpected derived token endpoint: %q", account.TokenEndpoint)
	}
	if account.IssuerURL != issuer || !strings.Contains(account.Scopes, "offline_access") {
		t.Fatalf("unexpected derived IdP metadata: issuer=%q scopes=%q", account.IssuerURL, account.Scopes)
	}
	minExpiry := before + int64(externalIdpImportMaxTrust/time.Second) - 1
	maxExpiry := after + int64(externalIdpImportMaxTrust/time.Second) + 1
	if account.ExpiresAt < minExpiry || account.ExpiresAt > maxExpiry || account.ExpiresAt >= upstreamExpiry {
		t.Fatalf("trusted expiry = %d, want capped interval [%d,%d] below %d", account.ExpiresAt, minExpiry, maxExpiry, upstreamExpiry)
	}
}

func TestPrepareCredentialsAccountRejectsUntrustedExternalIdpEndpoint(t *testing.T) {
	_, importErr := (&Handler{}).prepareCredentialsAccount(importCredentialsRequest{
		AuthMethod:    "external_idp",
		RefreshToken:  "refresh-token",
		ClientID:      "client-123",
		TokenEndpoint: "https://login.microsoftonline.com.evil.example/oauth2/v2.0/token",
	})
	if importErr == nil {
		t.Fatal("expected untrusted external IdP endpoint to be rejected")
	}
	if importErr.status != http.StatusBadRequest || !strings.Contains(importErr.Error(), "endpoint rejected") {
		t.Fatalf("unexpected import error: status=%d error=%q", importErr.status, importErr.Error())
	}
}

func TestPrepareCredentialsAccountExplicitIDCWinsExternalHints(t *testing.T) {
	issuer := "https://login.microsoftonline.com/tenant-123/v2.0"
	importPayload := fmt.Sprintf(`{"iss":%q,"email":"idc@example.com","exp":%d}`, issuer, time.Now().Add(time.Hour).Unix())
	importAccessToken := "header." + base64.RawURLEncoding.EncodeToString([]byte(importPayload)) + ".signature"
	refreshedPayload := fmt.Sprintf(`{"email":"idc-refreshed@example.com","exp":%d}`, time.Now().Add(time.Hour).Unix())
	refreshedAccessToken := "header." + base64.RawURLEncoding.EncodeToString([]byte(refreshedPayload)) + ".signature"

	previousClient := auth.SetGlobalAuthClientForTest(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host != "oidc.test" {
			t.Fatalf("explicit IDC import reached external endpoint: %s", req.URL)
		}
		body := fmt.Sprintf(`{"accessToken":%q,"refreshToken":"idc-rotated","expiresIn":3600}`, refreshedAccessToken)
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})})
	defer auth.SetGlobalAuthClientForTest(previousClient)
	oldOIDC := authOidcURL()
	auth.SetOIDCTokenURLForTest(func(string) string { return "https://oidc.test/refresh" })
	defer auth.SetOIDCTokenURLForTest(oldOIDC)

	account, importErr := (&Handler{}).prepareCredentialsAccount(importCredentialsRequest{
		AuthMethod:    "idc",
		AccessToken:   importAccessToken,
		RefreshToken:  "idc-refresh",
		ClientID:      "idc-client",
		ClientSecret:  "idc-secret",
		Region:        "us-east-1",
		TokenEndpoint: "https://login.microsoftonline.com/tenant-123/oauth2/v2.0/token",
		IssuerURL:     issuer,
	})
	if importErr != nil {
		t.Fatalf("prepare IDC account: %v", importErr)
	}
	if account.AuthMethod != "idc" || account.AccessToken != refreshedAccessToken || account.RefreshToken != "idc-rotated" {
		t.Fatalf("external hints overrode IDC refresh: %+v", account)
	}
}

func TestApiImportCredentialsValidatesExternalIdpProfile(t *testing.T) {
	issuer := "https://login.microsoftonline.com/tenant-123/v2.0"
	payload := fmt.Sprintf(`{"iss":%q,"preferred_username":"entra@example.com","exp":%d}`, issuer, time.Now().Add(time.Hour).Unix())
	accessToken := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`)) + "." +
		base64.RawURLEncoding.EncodeToString([]byte(payload)) + ".signature"
	const offeredProfile = "arn:aws:codewhisperer:us-east-1:123456789012:profile/offered"
	const rejectedProfile = "arn:aws:codewhisperer:us-east-1:123456789012:profile/rejected"

	for _, test := range []struct {
		name       string
		profileArn string
		wantStatus int
	}{
		{name: "offered", profileArn: offeredProfile, wantStatus: http.StatusOK},
		{name: "unoffered", profileArn: rejectedProfile, wantStatus: http.StatusBadRequest},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
				t.Fatalf("config.Init: %v", err)
			}
			t.Setenv("KIRO_PROFILE_REGIONS", "us-east-1")
			kiroRestHttpStore.Store(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if req.URL.Path != "/ListAvailableProfiles" {
					t.Fatalf("unexpected profile request path: %s", req.URL.Path)
				}
				body := `{"profiles":[{"arn":"` + offeredProfile + `"}]}`
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
			})})
			t.Cleanup(func() { InitKiroHttpClient("") })

			body, err := json.Marshal(map[string]interface{}{
				"accessToken":  accessToken,
				"refreshToken": "refresh-token",
				"clientId":     "client-123",
				"authMethod":   "external_idp",
				"region":       "us-east-1",
				"profileArn":   test.profileArn,
			})
			if err != nil {
				t.Fatalf("marshal request: %v", err)
			}
			rec := httptest.NewRecorder()
			(&Handler{pool: accountpool.GetPool()}).apiImportCredentials(
				rec,
				httptest.NewRequest(http.MethodPost, "/auth/credentials", strings.NewReader(string(body))),
			)

			if rec.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, test.wantStatus, rec.Body.String())
			}
			accounts := config.GetAccounts()
			if test.wantStatus == http.StatusOK {
				if len(accounts) != 1 || accounts[0].ProfileArn != offeredProfile {
					t.Fatalf("offered profile was not persisted: %+v", accounts)
				}
			} else {
				if !strings.Contains(rec.Body.String(), "not offered") {
					t.Fatalf("expected not-offered error, got %s", rec.Body.String())
				}
				if len(accounts) != 0 {
					t.Fatalf("rejected profile persisted accounts: %+v", accounts)
				}
			}
		})
	}
}

func TestApiImportCredentialsReturnsRotatedRefreshTokenWhenPersistFails(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	defer installCleanAuthClient(t)()

	accessPayload := fmt.Sprintf(`{"email":"persist-fail@example.com","exp":%d}`, time.Now().Add(time.Hour).Unix())
	accessToken := "header." + base64.RawURLEncoding.EncodeToString([]byte(accessPayload)) + ".signature"
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"accessToken": accessToken, "refreshToken": "rotated-after-refresh", "expiresIn": 3600,
		})
	}))
	defer fake.Close()

	oldOIDC := authOidcURL()
	auth.SetOIDCTokenURLForTest(func(string) string { return fake.URL })
	defer auth.SetOIDCTokenURLForTest(oldOIDC)

	if err := os.Remove(cfgFile); err != nil {
		t.Fatalf("remove config file: %v", err)
	}
	if err := os.Mkdir(cfgFile, 0o700); err != nil {
		t.Fatalf("replace config file with directory: %v", err)
	}

	rec := httptest.NewRecorder()
	(&Handler{pool: accountpool.GetPool()}).apiImportCredentials(
		rec,
		httptest.NewRequest(http.MethodPost, "/auth/credentials", strings.NewReader(`{
			"refreshToken":"original-refresh",
			"clientId":"client-id",
			"clientSecret":"client-secret",
			"authMethod":"idc",
			"region":"us-east-1"
		}`)),
	)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", rec.Code, rec.Body.String())
	}
	var response map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response["rotatedRefreshToken"] != "rotated-after-refresh" || response["hint"] == "" {
		t.Fatalf("missing rotated-token recovery response: %+v", response)
	}
	if accounts := config.GetAccounts(); len(accounts) != 0 {
		t.Fatalf("failed import persisted accounts: %+v", accounts)
	}
}

func TestApiImportCredentialsReturnsBatchRotatedTokensWhenPersistFails(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	defer installCleanAuthClient(t)()

	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var requestBody struct {
			RefreshToken string `json:"refreshToken"`
		}
		if err := json.NewDecoder(r.Body).Decode(&requestBody); err != nil {
			t.Errorf("decode refresh request: %v", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		email := strings.TrimPrefix(requestBody.RefreshToken, "original-") + "@example.com"
		accessPayload := fmt.Sprintf(`{"email":%q,"exp":%d}`, email, time.Now().Add(time.Hour).Unix())
		accessToken := "header." + base64.RawURLEncoding.EncodeToString([]byte(accessPayload)) + ".signature"
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"accessToken":  accessToken,
			"refreshToken": "rotated-" + strings.TrimPrefix(requestBody.RefreshToken, "original-"),
			"expiresIn":    3600,
		})
	}))
	defer fake.Close()

	oldOIDC := authOidcURL()
	auth.SetOIDCTokenURLForTest(func(string) string { return fake.URL })
	defer auth.SetOIDCTokenURLForTest(oldOIDC)
	if err := os.Remove(cfgFile); err != nil {
		t.Fatalf("remove config file: %v", err)
	}
	if err := os.Mkdir(cfgFile, 0o700); err != nil {
		t.Fatalf("replace config file with directory: %v", err)
	}

	rec := httptest.NewRecorder()
	(&Handler{pool: accountpool.GetPool()}).apiImportCredentials(
		rec,
		httptest.NewRequest(http.MethodPost, "/auth/credentials", strings.NewReader(`[
			{"refreshToken":"original-one","clientId":"client-one","clientSecret":"secret-one","authMethod":"idc","region":"us-east-1"},
			{"refreshToken":"original-two","clientId":"client-two","clientSecret":"secret-two","authMethod":"idc","region":"us-east-1"}
		]`)),
	)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", rec.Code, rec.Body.String())
	}
	var response struct {
		Recoveries []struct {
			Index               int    `json:"index"`
			RotatedRefreshToken string `json:"rotatedRefreshToken"`
		} `json:"recoveries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	got := make(map[int]string, len(response.Recoveries))
	for _, recovery := range response.Recoveries {
		got[recovery.Index] = recovery.RotatedRefreshToken
	}
	want := map[int]string{1: "rotated-one", 2: "rotated-two"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("batch recoveries = %v, want %v; body=%s", got, want, rec.Body.String())
	}
	if accounts := config.GetAccounts(); len(accounts) != 0 {
		t.Fatalf("failed batch import persisted accounts: %+v", accounts)
	}
}

// authOidcURL captures the current oidc URL builder so the test can restore it.
func authOidcURL() func(string) string { return auth.GetOIDCTokenURLForTest() }
