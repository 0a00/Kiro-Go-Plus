package proxy

import (
	"kiro-go/auth"
	"kiro-go/config"
	"path/filepath"
	"testing"
)

func TestKiroSsoAccountUpsertPreservesIdentityAndProfile(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}

	result := &auth.KiroSsoResult{
		AccessToken:   "access-one",
		RefreshToken:  "refresh-one",
		AuthMethod:    "external_idp",
		Provider:      "AzureAD",
		ClientID:      "client-id",
		TokenEndpoint: "https://login.microsoftonline.com/tenant/oauth2/v2.0/token",
		IssuerURL:     "https://login.microsoftonline.com/tenant/v2.0",
		Scopes:        "scope offline_access",
		ProfileArn:    "arn:aws:codewhisperer:eu-central-1:1:profile/primary",
		Region:        "us-east-1",
		Email:         "entra@example.com",
	}
	first, updated, err := config.UpsertAccountByIdentity(buildKiroSsoAccount(result, "machine-one", 1000))
	if err != nil {
		t.Fatalf("insert SSO account: %v", err)
	}
	if updated {
		t.Fatal("first SSO account should be created")
	}

	result.AccessToken = "access-two"
	result.RefreshToken = "refresh-two"
	result.ProfileArn = ""
	second, updated, err := config.UpsertAccountByIdentity(buildKiroSsoAccount(result, "machine-two", 2000))
	if err != nil {
		t.Fatalf("update SSO account: %v", err)
	}
	if !updated {
		t.Fatal("repeated SSO login should update the existing identity")
	}
	if second.ID != first.ID {
		t.Fatalf("account ID changed during upsert: got %q want %q", second.ID, first.ID)
	}
	if second.AccessToken != "access-two" || second.RefreshToken != "refresh-two" || second.ExpiresAt != 2000 {
		t.Fatalf("refreshed credentials were not applied: %+v", second)
	}
	if second.ProfileArn != first.ProfileArn {
		t.Fatalf("empty rediscovery should preserve profile ARN %q, got %q", first.ProfileArn, second.ProfileArn)
	}
	if accounts := config.GetAccounts(); len(accounts) != 1 {
		t.Fatalf("repeated SSO login created %d accounts, want 1", len(accounts))
	}
}
