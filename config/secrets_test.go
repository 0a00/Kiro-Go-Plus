package config

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func configureTestMasterKey(t *testing.T) {
	t.Helper()
	t.Setenv("KIRO_MASTER_KEY_FILE", "")
	t.Setenv("KIRO_MASTER_KEY", base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef")))
}

func TestConfigSecretsEncryptedAtRestAndDecryptedOnLoad(t *testing.T) {
	configureTestMasterKey(t)
	path := filepath.Join(t.TempDir(), "config.json")
	if err := Init(path); err != nil {
		t.Fatalf("Init: %v", err)
	}
	account := Account{
		ID: "encrypted-account", Enabled: true,
		AccessToken: "access-plain-secret", RefreshToken: "refresh-plain-secret",
		ClientSecret: "client-plain-secret",
		ProxyURL:     "socks5://user:proxy-plain-secret@127.0.0.1:1080",
	}
	if err := AddAccount(account); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	if err := AddAccount(Account{ID: "encrypted-api-account", Enabled: true, AuthMethod: "api_key", KiroApiKey: "kiro-plain-secret"}); err != nil {
		t.Fatalf("AddAccount API key: %v", err)
	}
	if _, err := AddApiKey(ApiKeyEntry{Name: "encrypted", Key: "public-api-plain-secret", Enabled: true}); err != nil {
		t.Fatalf("AddApiKey: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	for _, secret := range []string{
		"access-plain-secret", "refresh-plain-secret", "kiro-plain-secret",
		"client-plain-secret", "proxy-plain-secret", "public-api-plain-secret",
	} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("plaintext secret %q was written to config", secret)
		}
	}
	if !strings.Contains(string(raw), encryptedSecretPrefix) {
		t.Fatal("expected encrypted values in config")
	}

	if err := Init(path); err != nil {
		t.Fatalf("reload encrypted config: %v", err)
	}
	accounts := GetAccounts()
	if len(accounts) != 2 || accounts[0].RefreshToken != account.RefreshToken || accounts[0].ClientSecret != account.ClientSecret || accounts[1].KiroApiKey != "kiro-plain-secret" {
		t.Fatalf("encrypted account did not round-trip: %+v", accounts)
	}
	keys := ListApiKeys()
	if len(keys) != 1 || keys[0].Key != "public-api-plain-secret" {
		t.Fatalf("encrypted API key did not round-trip: %+v", keys)
	}
}

func TestEncryptedConfigRequiresMasterKey(t *testing.T) {
	configureTestMasterKey(t)
	path := filepath.Join(t.TempDir(), "config.json")
	if err := Init(path); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := AddAccount(Account{ID: "a", Enabled: true, RefreshToken: "secret-refresh"}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}

	t.Setenv("KIRO_MASTER_KEY", "")
	if err := Init(path); err == nil || !strings.Contains(err.Error(), "encrypted configuration requires") {
		t.Fatalf("expected missing-key error, got %v", err)
	}
}
