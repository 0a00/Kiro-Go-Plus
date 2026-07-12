package proxy

import (
	"encoding/json"
	"kiro-go/config"
	accountpool "kiro-go/pool"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestAccountDetailsMaskCredentialsAndExportRequiresReauthentication(t *testing.T) {
	t.Setenv("KIRO_MASTER_KEY", "")
	t.Setenv("KIRO_MASTER_KEY_FILE", "")
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.UpdateSettingsPatch(nil, nil, "admin-secret"); err != nil {
		t.Fatalf("set password: %v", err)
	}
	account := config.Account{
		ID: "account-secret", Enabled: true, Email: "secret@example.com",
		AccessToken: "access-super-secret", RefreshToken: "refresh-super-secret",
		ClientID: "client-id-secret", ClientSecret: "client-super-secret",
	}
	if err := config.AddAccount(account); err != nil {
		t.Fatalf("add account: %v", err)
	}
	accountpool.GetPool().Reload()
	h := &Handler{pool: accountpool.GetPool()}

	details := httptest.NewRecorder()
	h.apiGetAccountFull(details, httptest.NewRequest(http.MethodGet, "/", nil), account.ID)
	if details.Code != http.StatusOK {
		t.Fatalf("details status=%d body=%s", details.Code, details.Body.String())
	}
	for _, secret := range []string{account.AccessToken, account.RefreshToken, account.ClientSecret} {
		if strings.Contains(details.Body.String(), secret) {
			t.Fatalf("details leaked credential %q: %s", secret, details.Body.String())
		}
	}

	denied := httptest.NewRecorder()
	h.apiExportAccountCredentials(denied, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"password":"wrong"}`)), account.ID)
	if denied.Code != http.StatusForbidden {
		t.Fatalf("expected forbidden export, got %d", denied.Code)
	}

	allowed := httptest.NewRecorder()
	h.apiExportAccountCredentials(allowed, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"password":"admin-secret"}`)), account.ID)
	if allowed.Code != http.StatusOK {
		t.Fatalf("expected successful export, got %d body=%s", allowed.Code, allowed.Body.String())
	}
	var exported map[string]interface{}
	if err := json.Unmarshal(allowed.Body.Bytes(), &exported); err != nil {
		t.Fatalf("decode export: %v", err)
	}
	if exported["refreshToken"] != account.RefreshToken || exported["clientSecret"] != account.ClientSecret {
		t.Fatalf("unexpected exported credentials: %+v", exported)
	}
}

func TestSanitizedProxyURLAndPasswordPreservation(t *testing.T) {
	raw := "socks5://user:secret-pass@127.0.0.1:1080"
	sanitized, passwordSet := sanitizedProxyURL(raw)
	if !passwordSet || strings.Contains(sanitized, "secret-pass") {
		t.Fatalf("proxy was not sanitized: %q passwordSet=%v", sanitized, passwordSet)
	}
	if got := preserveProxyPassword(raw, sanitized); got != raw {
		t.Fatalf("expected hidden password to be preserved, got %q", got)
	}
}
