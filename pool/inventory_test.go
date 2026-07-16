package pool

import (
	"kiro-go/config"
	"path/filepath"
	"testing"
	"time"
)

func TestInventorySnapshotPartitionsAndDeduplicatesAccounts(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	accounts := []config.Account{
		{ID: "live", UserId: "user-live", Provider: "AzureAD", Enabled: true, AccessToken: "token"},
		{ID: "cool", UserId: "user-cool", Enabled: true, AccessToken: "token"},
		{ID: "quota", UserId: "user-quota", Enabled: true, AccessToken: "token", UsageCurrent: 100, UsageLimit: 100},
		{ID: "expired", UserId: "user-expired", Enabled: true, AccessToken: "old", ExpiresAt: time.Now().Add(-time.Hour).Unix()},
		{ID: "profile", UserId: "user-profile", Enabled: false, BanStatus: "DISABLED", BanReason: "no available Kiro profile"},
		{ID: "off", UserId: "user-off", Enabled: false, BanStatus: "DISABLED"},
		{ID: "banned", UserId: "user-banned", Enabled: false, BanStatus: "BANNED"},
		// A healthy row for the same identity wins over a stale banned row.
		{ID: "live-stale", UserId: "user-live", Provider: "API Key", Enabled: false, BanStatus: "BANNED"},
	}
	for _, account := range accounts {
		if err := config.AddAccount(account); err != nil {
			t.Fatalf("AddAccount(%s): %v", account.ID, err)
		}
	}
	p := &AccountPool{
		accounts:        []config.Account{accounts[0], accounts[1]},
		cooldowns:       map[string]time.Time{"cool": time.Now().Add(time.Minute)},
		cooldownKinds:   map[string]accountCooldownKind{"cool": accountCooldownTransient},
		refreshFailures: make(map[string]time.Time),
	}

	got := p.InventorySnapshot()
	if got.Total != 7 || got.ConfiguredRows != 8 {
		t.Fatalf("unexpected inventory totals: %+v", got)
	}
	if got.Routable != 1 || got.Cooling != 1 || got.QuotaBlocked != 1 || got.CredentialIssue != 1 || got.ProfileIssue != 1 || got.Disabled != 1 || got.Banned != 1 {
		t.Fatalf("unexpected inventory partition: %+v", got)
	}
	if sum := got.Routable + got.Cooling + got.QuotaBlocked + got.CredentialIssue + got.ProfileIssue + got.Disabled + got.Banned; sum != got.Total {
		t.Fatalf("inventory does not partition total: sum=%d total=%d", sum, got.Total)
	}
}

func TestClearAccountCooldownsRemovesHardBackoff(t *testing.T) {
	p := &AccountPool{
		cooldowns:       map[string]time.Time{"a": time.Now().Add(24 * time.Hour)},
		cooldownKinds:   map[string]accountCooldownKind{"a": accountCooldownDisabled},
		errorCounts:     map[string]int{"a": 3},
		refreshFailures: map[string]time.Time{"a": time.Now().Add(time.Hour)},
	}
	p.ClearAccountCooldowns(map[string]bool{"a": true})
	if _, ok := p.cooldowns["a"]; ok {
		t.Fatal("account cooldown was not cleared")
	}
	if _, ok := p.refreshFailures["a"]; ok {
		t.Fatal("refresh cooldown was not cleared")
	}
	if _, ok := p.errorCounts["a"]; ok {
		t.Fatal("error count was not cleared")
	}
}
