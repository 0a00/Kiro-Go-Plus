package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"kiro-go/config"
	accountpool "kiro-go/pool"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func stubKiroAPIKeyProbe(t *testing.T, stub func(context.Context, string, string) (*config.AccountInfo, error)) {
	t.Helper()
	original := probeKiroAPIKeyRegion
	probeKiroAPIKeyRegion = stub
	t.Cleanup(func() { probeKiroAPIKeyRegion = original })
}

func TestResolveKiroAPIKeyRegionDiscoversWorkingCandidate(t *testing.T) {
	t.Setenv("KIRO_PROFILE_REGIONS", "us-east-1, eu-central-1,us-east-1")
	var calls []string
	stubKiroAPIKeyProbe(t, func(_ context.Context, key, region string) (*config.AccountInfo, error) {
		if key != "ksk_test" {
			t.Fatalf("unexpected key %q", key)
		}
		calls = append(calls, region)
		if region == "us-east-1" {
			return nil, errors.New("HTTP 403 from GetUsageLimits")
		}
		return &config.AccountInfo{Email: "verified@example.invalid", UserId: "user-verified"}, nil
	})

	region, info, retryable, err := resolveKiroAPIKeyRegion(context.Background(), "ksk_test", "")
	if err != nil {
		t.Fatalf("resolve region: %v", err)
	}
	if region != "eu-central-1" || info == nil || info.UserId != "user-verified" || retryable {
		t.Fatalf("unexpected result: region=%q info=%+v retryable=%v", region, info, retryable)
	}
	if want := []string{"us-east-1", "eu-central-1"}; !reflect.DeepEqual(calls, want) {
		t.Fatalf("probe order=%v want=%v", calls, want)
	}
}

func TestResolveKiroAPIKeyRegionClassifiesTransientFailure(t *testing.T) {
	t.Setenv("KIRO_PROFILE_REGIONS", "us-east-1,eu-central-1")
	stubKiroAPIKeyProbe(t, func(_ context.Context, _, region string) (*config.AccountInfo, error) {
		if region == "us-east-1" {
			return nil, context.DeadlineExceeded
		}
		return nil, errors.New("HTTP 403 from GetUsageLimits")
	})

	_, _, retryable, err := resolveKiroAPIKeyRegion(context.Background(), "ksk_test", "")
	if err == nil || !retryable {
		t.Fatalf("expected retryable aggregate failure, retryable=%v err=%v", retryable, err)
	}
}

func TestResolveKiroAPIKeyRegionValidatesExplicitRegionOnly(t *testing.T) {
	t.Setenv("KIRO_PROFILE_REGIONS", "us-east-1,eu-central-1")
	var calls []string
	stubKiroAPIKeyProbe(t, func(_ context.Context, _, region string) (*config.AccountInfo, error) {
		calls = append(calls, region)
		return &config.AccountInfo{UserId: "explicit-user"}, nil
	})

	region, _, _, err := resolveKiroAPIKeyRegion(context.Background(), "ksk_test", "ap-southeast-1")
	if err != nil || region != "ap-southeast-1" || !reflect.DeepEqual(calls, []string{"ap-southeast-1"}) {
		t.Fatalf("explicit region result=%q calls=%v err=%v", region, calls, err)
	}
	if _, _, _, err := resolveKiroAPIKeyRegion(context.Background(), "ksk_test", "bad/region"); err == nil {
		t.Fatal("invalid explicit region was accepted")
	}
}

func TestApiImportCredentialsDiscoversKiroAPIKeyRegion(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	t.Setenv("KIRO_PROFILE_REGIONS", "us-east-1,eu-central-1")
	stubKiroAPIKeyProbe(t, func(_ context.Context, _, region string) (*config.AccountInfo, error) {
		if region == "us-east-1" {
			return nil, errors.New("HTTP 403 from GetUsageLimits")
		}
		return &config.AccountInfo{
			Email:            "power@example.invalid",
			UserId:           "power-user",
			SubscriptionType: "POWER",
			UsageCurrent:     10,
			UsageLimit:       1000,
		}, nil
	})

	h := &Handler{pool: accountpool.GetPool()}
	rec := httptest.NewRecorder()
	h.apiImportCredentials(rec, httptest.NewRequest(http.MethodPost, "/admin/api/auth/credentials", strings.NewReader(`{"authMethod":"api_key","kiroApiKey":"ksk_import"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("import status=%d body=%s", rec.Code, rec.Body.String())
	}
	accounts := config.GetAccounts()
	if len(accounts) != 1 {
		t.Fatalf("expected one persisted account, got %d", len(accounts))
	}
	account := accounts[0]
	if account.Region != "eu-central-1" || account.UserId != "power-user" || account.Email != "power@example.invalid" {
		t.Fatalf("unexpected verified account: %+v", account)
	}
	if account.SubscriptionType != "POWER" || account.UsageLimit != 1000 {
		t.Fatalf("probe metadata was not persisted: %+v", account)
	}
}

func TestApiAddAccountDiscoversKiroAPIKeyRegion(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	t.Setenv("KIRO_PROFILE_REGIONS", "us-east-1,eu-central-1")
	stubKiroAPIKeyProbe(t, func(_ context.Context, _, region string) (*config.AccountInfo, error) {
		if region == "us-east-1" {
			return nil, errors.New("HTTP 403 from GetUsageLimits")
		}
		return &config.AccountInfo{Email: "direct@example.invalid", UserId: "direct-user"}, nil
	})

	h := &Handler{pool: accountpool.GetPool()}
	rec := httptest.NewRecorder()
	h.apiAddAccount(rec, httptest.NewRequest(http.MethodPost, "/admin/api/accounts", strings.NewReader(`{"authMethod":"api_key","kiroApiKey":"ksk_direct","enabled":false}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("add status=%d body=%s", rec.Code, rec.Body.String())
	}
	accounts := config.GetAccounts()
	if len(accounts) != 1 || accounts[0].Region != "eu-central-1" || accounts[0].UserId != "direct-user" || accounts[0].Provider != "API Key" {
		t.Fatalf("unexpected direct API key account: %+v", accounts)
	}
}

func TestApiUpdateAccountReenableClearsHardCooldown(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.AddAccount(config.Account{
		ID: "reenable", Enabled: true, RefreshToken: "refresh-token", AuthMethod: "social", Region: "us-east-1",
	}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	p := accountpool.GetPool()
	p.Reload()
	p.DisableAccount("reenable", "operator test")

	rec := httptest.NewRecorder()
	(&Handler{pool: p}).apiUpdateAccount(rec, httptest.NewRequest(http.MethodPut, "/admin/api/accounts/reenable", strings.NewReader(`{"enabled":true}`)), "reenable")
	if rec.Code != http.StatusOK {
		t.Fatalf("reenable status=%d body=%s", rec.Code, rec.Body.String())
	}
	if selected := p.GetNext(); selected == nil || selected.ID != "reenable" {
		t.Fatalf("re-enabled account remained unavailable: %+v", selected)
	}
	account := config.GetAccounts()[0]
	if !account.Enabled || account.BanStatus != "ACTIVE" || account.BanReason != "" {
		t.Fatalf("re-enabled account status was not cleared: %+v", account)
	}
}

func TestDecodeImportCredentialsAcceptsScopesStringOrArray(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "string", body: `{"refreshToken":"r","scopes":"scope-a, offline_access scope-a"}`, want: "scope-a offline_access"},
		{name: "array", body: `{"refreshToken":"r","scopes":["scope-a","offline_access","scope-a"]}`, want: "scope-a offline_access"},
		{name: "nested array", body: `{"credentials":{"refreshToken":"r","scopes":["scope-a","offline_access"]}}`, want: "scope-a offline_access"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			reqs, _, err := decodeImportCredentialsRequests([]byte(test.body))
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			req := normalizeNestedCredentialImport(reqs[0])
			if got := string(req.Scopes); got != test.want {
				t.Fatalf("scopes=%q want=%q", got, test.want)
			}
		})
	}
}

func TestStatusIncludesAccountInventory(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.AddAccount(config.Account{ID: "live", Enabled: true, AccessToken: "token"}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	p := accountpool.GetPool()
	p.Reload()
	rec := httptest.NewRecorder()
	(&Handler{pool: p}).apiGetStatus(rec, httptest.NewRequest(http.MethodGet, "/admin/api/status", nil))
	var body struct {
		AccountInventory struct {
			Total    int `json:"total"`
			Routable int `json:"routable"`
		} `json:"accountInventory"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if body.AccountInventory.Total != 1 || body.AccountInventory.Routable != 1 {
		t.Fatalf("unexpected status inventory: %+v", body.AccountInventory)
	}
}
