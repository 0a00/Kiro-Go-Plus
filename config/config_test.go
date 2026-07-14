package config

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestVersionMetadataMatchesBinaryVersion(t *testing.T) {
	raw, err := os.ReadFile("../version.json")
	if err != nil {
		t.Fatalf("read version metadata: %v", err)
	}
	var metadata struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(raw, &metadata); err != nil {
		t.Fatalf("parse version metadata: %v", err)
	}
	if metadata.Version != Version {
		t.Fatalf("version metadata %q does not match binary version %q", metadata.Version, Version)
	}
}

func TestRequestLogConfigDefaultsAndPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := Init(path); err != nil {
		t.Fatalf("init config: %v", err)
	}
	if got := GetRequestLogConfig().MaxEntries; got != DefaultRequestLogMaxEntries {
		t.Fatalf("default max entries = %d, want %d", got, DefaultRequestLogMaxEntries)
	}
	defaults := GetRequestLogConfig()
	if defaults.DetailedLogEnabled || defaults.DetailedMaxEntries != DefaultRequestDetailMaxEntries || defaults.MaxDetailBytes != DefaultRequestDetailMaxBytes {
		t.Fatalf("unexpected request detail defaults: %+v", defaults)
	}

	if err := UpdateRequestLogConfig(RequestLogConfig{
		MaxEntries:         5000,
		DetailedLogEnabled: true,
		DetailedMaxEntries: 250,
		MaxDetailBytes:     512 << 10,
	}); err != nil {
		t.Fatalf("update request log config: %v", err)
	}
	if err := Init(path); err != nil {
		t.Fatalf("reload config: %v", err)
	}
	persisted := GetRequestLogConfig()
	if persisted.MaxEntries != 5000 || !persisted.DetailedLogEnabled || persisted.DetailedMaxEntries != 250 || persisted.MaxDetailBytes != 512<<10 {
		t.Fatalf("unexpected persisted request log config: %+v", persisted)
	}

	if err := UpdateRequestLogConfig(RequestLogConfig{
		MaxEntries:         MaxRequestLogMaxEntries + 1,
		DetailedMaxEntries: MaxRequestDetailMaxEntries + 1,
		MaxDetailBytes:     MaxRequestDetailMaxBytes + 1,
	}); err != nil {
		t.Fatalf("clamp request log config: %v", err)
	}
	clamped := GetRequestLogConfig()
	if clamped.MaxEntries != MaxRequestLogMaxEntries || clamped.DetailedMaxEntries != MaxRequestDetailMaxEntries || clamped.MaxDetailBytes != MaxRequestDetailMaxBytes {
		t.Fatalf("unexpected clamped request log config: %+v", clamped)
	}
}

func TestResetStatisticsClearsGlobalAndAccountCounters(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := Init(path); err != nil {
		t.Fatalf("init config: %v", err)
	}
	if err := AddAccount(Account{
		ID:           "stats-reset",
		RequestCount: 12,
		ErrorCount:   4,
		TotalTokens:  345,
		TotalCredits: 6.5,
		LastUsed:     123,
	}); err != nil {
		t.Fatalf("add account: %v", err)
	}
	if err := UpdateStats(16, 12, 4, 345, 6.5); err != nil {
		t.Fatalf("seed global stats: %v", err)
	}

	if err := ResetStatistics(); err != nil {
		t.Fatalf("reset statistics: %v", err)
	}
	total, success, failed, tokens, credits := GetStats()
	if total != 0 || success != 0 || failed != 0 || tokens != 0 || credits != 0 {
		t.Fatalf("global stats were not reset: %d %d %d %d %f", total, success, failed, tokens, credits)
	}
	accounts := GetAccounts()
	if len(accounts) != 1 {
		t.Fatalf("account count = %d, want 1", len(accounts))
	}
	account := accounts[0]
	if account.RequestCount != 0 || account.ErrorCount != 0 || account.TotalTokens != 0 || account.TotalCredits != 0 || account.LastUsed != 0 {
		t.Fatalf("account stats were not reset: %+v", account)
	}
}

func TestThinkingConfigDefaultsAndPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := Init(path); err != nil {
		t.Fatalf("init config: %v", err)
	}
	defaults := GetThinkingConfig()
	if defaults.DefaultBudgetTokens != 4000 || defaults.BudgetCapTokens != 10000 || defaults.DefaultMaxOutputTokens != 0 || defaults.DefaultContextWindowTokens != 0 || defaults.ToolStreamMode != ToolStreamModeSafe || !defaults.BufferToolStreams || !defaults.EnforceAgentToolUse {
		t.Fatalf("unexpected defaults: %+v", defaults)
	}

	if err := UpdateThinkingConfig("-reason", "reasoning_content", "thinking", 5000, 0, 64000, 1000000, false, false); err != nil {
		t.Fatalf("update thinking config: %v", err)
	}
	if err := Init(path); err != nil {
		t.Fatalf("reload config: %v", err)
	}
	got := GetThinkingConfig()
	if got.Suffix != "-reason" || got.DefaultBudgetTokens != 5000 || got.BudgetCapTokens != 0 || got.DefaultMaxOutputTokens != 64000 || got.DefaultContextWindowTokens != 1000000 || got.ToolStreamMode != ToolStreamModeLive || got.BufferToolStreams || got.EnforceAgentToolUse {
		t.Fatalf("unexpected persisted thinking config: %+v", got)
	}

	if err := UpdateThinkingConfigWithToolStreamMode("-reason", "reasoning_content", "thinking", 5000, 0, 64000, 1000000, ToolStreamModeBalanced, false); err != nil {
		t.Fatalf("update balanced thinking config: %v", err)
	}
	if err := Init(path); err != nil {
		t.Fatalf("reload balanced config: %v", err)
	}
	got = GetThinkingConfig()
	if got.ToolStreamMode != ToolStreamModeBalanced || !got.BufferToolStreams {
		t.Fatalf("unexpected balanced thinking config: %+v", got)
	}
	if err := UpdateThinkingConfigWithToolStreamMode("-reason", "reasoning_content", "thinking", 5000, 0, 64000, 1000000, "invalid", false); err == nil {
		t.Fatal("expected invalid tool stream mode to be rejected")
	}
}

func TestThinkingConfigLoadsLegacyBufferToolStreams(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := Init(path); err != nil {
		t.Fatalf("init config: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var stored map[string]interface{}
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatalf("decode config: %v", err)
	}
	delete(stored, "toolStreamMode")
	stored["bufferToolStreams"] = false
	raw, err = json.MarshalIndent(stored, "", "  ")
	if err != nil {
		t.Fatalf("encode legacy config: %v", err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatalf("write legacy config: %v", err)
	}
	if err := Init(path); err != nil {
		t.Fatalf("reload legacy config: %v", err)
	}
	if got := GetThinkingConfig(); got.ToolStreamMode != ToolStreamModeLive || got.BufferToolStreams {
		t.Fatalf("legacy buffer setting did not migrate to live mode: %+v", got)
	}
}

func TestAdminPasswordIsHashedAtRest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := Init(path); err != nil {
		t.Fatalf("init config: %v", err)
	}
	if !VerifyPassword("changeme") {
		t.Fatal("expected default password to verify")
	}
	if err := UpdateSettingsPatch(nil, nil, "a-longer-production-password"); err != nil {
		t.Fatalf("update password: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if bytes.Contains(raw, []byte("a-longer-production-password")) {
		t.Fatal("config contains the plaintext admin password")
	}
	if !VerifyPassword("a-longer-production-password") {
		t.Fatal("expected persisted password hash to verify")
	}
}

func TestUpdateSettingsPatchPreservesOmittedAPIKeyFields(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	if err := UpdateSettings("proxy-api-key", true, "admin-password"); err != nil {
		t.Fatalf("seed settings: %v", err)
	}

	if err := UpdateSettingsPatch(nil, nil, "new-admin-password"); err != nil {
		t.Fatalf("patch settings: %v", err)
	}

	if got := GetApiKey(); got != "proxy-api-key" {
		t.Fatalf("expected API key to be preserved, got %q", got)
	}
	if !IsApiKeyRequired() {
		t.Fatalf("expected requireApiKey to stay enabled")
	}
	if !VerifyPassword("new-admin-password") {
		t.Fatalf("expected new password to verify")
	}
	if GetPassword() == "new-admin-password" {
		t.Fatalf("expected password to be stored as a hash")
	}
}

func TestUpdateSettingsPatchCanExplicitlyDisableAPIKey(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	if err := UpdateSettings("proxy-api-key", true, "admin-password"); err != nil {
		t.Fatalf("seed settings: %v", err)
	}

	emptyKey := ""
	requireAPIKey := false
	if err := UpdateSettingsPatch(&emptyKey, &requireAPIKey, ""); err != nil {
		t.Fatalf("patch settings: %v", err)
	}

	if got := GetApiKey(); got != "" {
		t.Fatalf("expected API key to be cleared, got %q", got)
	}
	if IsApiKeyRequired() {
		t.Fatalf("expected requireApiKey to be disabled")
	}
	if !VerifyPassword("admin-password") {
		t.Fatalf("expected password to be preserved")
	}
}

// TestAccountAllowOverageMigration verifies that a config.json from before the
// upstream-Overages-switch refactor (which carried `allowOverage: true` per
// account) is migrated into OverageStatus="ENABLED" on first load, and that
// the legacy field is cleared so future saves don't re-emit it.
func TestAccountAllowOverageMigration(t *testing.T) {
	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "config.json")

	seed := map[string]interface{}{
		"password":      "p",
		"port":          8080,
		"host":          "0.0.0.0",
		"requireApiKey": false,
		"accounts": []map[string]interface{}{
			{"id": "acc-allow", "enabled": true, "allowOverage": true},
			{"id": "acc-deny", "enabled": true, "allowOverage": false},
			{"id": "acc-already-set", "enabled": true, "allowOverage": true, "overageStatus": "DISABLED"},
		},
	}
	raw, err := json.MarshalIndent(seed, "", "  ")
	if err != nil {
		t.Fatalf("marshal seed: %v", err)
	}
	if err := os.WriteFile(cfgFile, raw, 0600); err != nil {
		t.Fatalf("write seed: %v", err)
	}

	if err := Init(cfgFile); err != nil {
		t.Fatalf("init: %v", err)
	}

	accounts := GetAccounts()
	byID := map[string]Account{}
	for _, a := range accounts {
		byID[a.ID] = a
	}

	if got := byID["acc-allow"].OverageStatus; got != "ENABLED" {
		t.Fatalf("expected acc-allow to migrate to OverageStatus=ENABLED, got %q", got)
	}
	if byID["acc-allow"].LegacyAllowOverage {
		t.Fatalf("expected legacy allowOverage to be cleared after migration")
	}
	if got := byID["acc-deny"].OverageStatus; got != "" {
		t.Fatalf("expected acc-deny to keep empty OverageStatus, got %q", got)
	}
	// Pre-set OverageStatus must win over the legacy field.
	if got := byID["acc-already-set"].OverageStatus; got != "DISABLED" {
		t.Fatalf("expected acc-already-set OverageStatus to be preserved, got %q", got)
	}
	if byID["acc-already-set"].LegacyAllowOverage {
		t.Fatalf("expected legacy field to still be cleared on acc-already-set")
	}

	// Re-read the file and confirm legacy field is gone (so it doesn't drift
	// back in on later saves).
	on_disk, err := os.ReadFile(cfgFile)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	var reloaded struct {
		Accounts []map[string]interface{} `json:"accounts"`
	}
	if err := json.Unmarshal(on_disk, &reloaded); err != nil {
		t.Fatalf("decode reload: %v", err)
	}
	for _, a := range reloaded.Accounts {
		if _, ok := a["allowOverage"]; ok {
			t.Fatalf("expected allowOverage to be omitted from persisted file, got %+v", a)
		}
	}
}

func TestUpstreamProtectionDefaultsEnabledForLegacyConfig(t *testing.T) {
	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "config.json")
	raw := []byte(`{"password":"p","port":8080,"host":"0.0.0.0","requireApiKey":false,"accounts":[]}`)
	if err := os.WriteFile(cfgFile, raw, 0600); err != nil {
		t.Fatalf("write seed: %v", err)
	}

	if err := Init(cfgFile); err != nil {
		t.Fatalf("init: %v", err)
	}

	got := GetUpstreamProtectionConfig()
	if !got.Enabled {
		t.Fatalf("expected upstream protection to default enabled for legacy config")
	}
	if got.MaxPerAccountModelConcurrency != 5 {
		t.Fatalf("expected default concurrency 5, got %d", got.MaxPerAccountModelConcurrency)
	}
}

func TestUpstreamProtectionCanBeExplicitlyDisabled(t *testing.T) {
	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "config.json")
	raw := []byte(`{"password":"p","port":8080,"host":"0.0.0.0","requireApiKey":false,"accounts":[],"upstreamProtection":{"enabled":false}}`)
	if err := os.WriteFile(cfgFile, raw, 0600); err != nil {
		t.Fatalf("write seed: %v", err)
	}

	if err := Init(cfgFile); err != nil {
		t.Fatalf("init: %v", err)
	}

	if GetUpstreamProtectionConfig().Enabled {
		t.Fatalf("expected explicit upstreamProtection.enabled=false to be preserved")
	}
}

func TestKiroAPIKeyAccountNormalization(t *testing.T) {
	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "config.json")
	raw := []byte(`{
		"password":"p",
		"port":8080,
		"host":"0.0.0.0",
		"requireApiKey":false,
		"accounts":[{
			"id":"api-key-account",
			"enabled":true,
			"authMethod":"apikey",
			"kiroApiKey":"kiro-key",
			"refreshToken":"should-be-cleared",
			"expiresAt":123456
		}]
	}`)
	if err := os.WriteFile(cfgFile, raw, 0600); err != nil {
		t.Fatalf("write seed: %v", err)
	}

	if err := Init(cfgFile); err != nil {
		t.Fatalf("init: %v", err)
	}

	accounts := GetAccounts()
	if len(accounts) != 1 {
		t.Fatalf("expected one account, got %d", len(accounts))
	}
	got := accounts[0]
	if got.AuthMethod != "api_key" {
		t.Fatalf("expected authMethod api_key, got %q", got.AuthMethod)
	}
	if got.AccessToken != "kiro-key" {
		t.Fatalf("expected accessToken to mirror kiroApiKey, got %q", got.AccessToken)
	}
	if got.RefreshToken != "" {
		t.Fatalf("expected refreshToken to be cleared, got %q", got.RefreshToken)
	}
	if got.ExpiresAt != 0 {
		t.Fatalf("expected expiresAt to be cleared, got %d", got.ExpiresAt)
	}
}

func TestPromptCacheDefaultsForLegacyConfig(t *testing.T) {
	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "config.json")
	raw := []byte(`{"password":"p","port":8080,"host":"0.0.0.0","requireApiKey":false,"accounts":[]}`)
	if err := os.WriteFile(cfgFile, raw, 0600); err != nil {
		t.Fatalf("write seed: %v", err)
	}

	if err := Init(cfgFile); err != nil {
		t.Fatalf("init: %v", err)
	}

	got := GetPromptCacheConfig()
	if !got.Enabled || got.NamespaceMode != PromptCacheNamespaceAccount {
		t.Fatalf("expected legacy cache policy enabled/account, got enabled=%v namespace=%q", got.Enabled, got.NamespaceMode)
	}
	if got.CacheReadEfficiency != 0.87 {
		t.Fatalf("expected default cacheReadEfficiency 0.87, got %v", got.CacheReadEfficiency)
	}
	if got.CacheReadEfficiencyMin != 0.87 || got.CacheReadEfficiencyMax != 0.87 {
		t.Fatalf("expected default cache efficiency range 0.87-0.87, got %v-%v", got.CacheReadEfficiencyMin, got.CacheReadEfficiencyMax)
	}
	if got.KvCacheTTLSecs != 3600 {
		t.Fatalf("expected default kvCacheTtlSecs 3600, got %d", got.KvCacheTTLSecs)
	}
}

func TestUpdatePromptCacheConfigClampsValues(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}

	if err := UpdatePromptCacheConfig(1.5, -0.5, 10); err != nil {
		t.Fatalf("update prompt cache config: %v", err)
	}

	got := GetPromptCacheConfig()
	if got.CacheReadEfficiencyMin != 0 || got.CacheReadEfficiencyMax != 1 {
		t.Fatalf("expected cache efficiency range to clamp and sort to 0-1, got %v-%v", got.CacheReadEfficiencyMin, got.CacheReadEfficiencyMax)
	}
	if got.CacheReadEfficiency != 0.5 {
		t.Fatalf("expected cacheReadEfficiency midpoint 0.5, got %v", got.CacheReadEfficiency)
	}
	if got.KvCacheTTLSecs != 60 {
		t.Fatalf("expected kvCacheTtlSecs to clamp to 60, got %d", got.KvCacheTTLSecs)
	}
}

func TestLegacyPromptCacheEfficiencyMigratesToRange(t *testing.T) {
	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "config.json")
	raw := []byte(`{"password":"p","port":8080,"host":"0.0.0.0","requireApiKey":false,"accounts":[],"cacheReadEfficiency":0.55,"kvCacheTtlSecs":1800}`)
	if err := os.WriteFile(cfgFile, raw, 0600); err != nil {
		t.Fatalf("write seed: %v", err)
	}

	if err := Init(cfgFile); err != nil {
		t.Fatalf("init: %v", err)
	}

	got := GetPromptCacheConfig()
	if got.CacheReadEfficiencyMin != 0.55 || got.CacheReadEfficiencyMax != 0.55 {
		t.Fatalf("expected legacy fixed efficiency to migrate to 0.55-0.55, got %v-%v", got.CacheReadEfficiencyMin, got.CacheReadEfficiencyMax)
	}
}

func TestPromptCacheZeroRangePersists(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := Init(cfgFile); err != nil {
		t.Fatalf("init config: %v", err)
	}

	if err := UpdatePromptCacheConfig(0, 0, 120); err != nil {
		t.Fatalf("update prompt cache config: %v", err)
	}

	raw, err := os.ReadFile(cfgFile)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var persisted map[string]interface{}
	if err := json.Unmarshal(raw, &persisted); err != nil {
		t.Fatalf("decode config: %v", err)
	}
	if _, ok := persisted["cacheReadEfficiencyMin"]; !ok {
		t.Fatalf("expected cacheReadEfficiencyMin to be persisted for zero range")
	}
	if _, ok := persisted["cacheReadEfficiencyMax"]; !ok {
		t.Fatalf("expected cacheReadEfficiencyMax to be persisted for zero range")
	}
}

func TestRuntimeConfigUpdate(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}

	if err := UpdateRuntimeConfig(RuntimeConfig{
		Host:          "127.0.0.1",
		Port:          9090,
		LogLevel:      "debug",
		KiroVersion:   "0.12.0",
		SystemVersion: "linux#6.1",
		NodeVersion:   "22.23.0",
	}); err != nil {
		t.Fatalf("update runtime config: %v", err)
	}

	got := GetRuntimeConfig()
	if got.Host != "127.0.0.1" || got.Port != 9090 || got.LogLevel != "debug" {
		t.Fatalf("unexpected runtime config: %+v", got)
	}
	if got.KiroVersion != "0.12.0" || got.SystemVersion != "linux#6.1" || got.NodeVersion != "22.23.0" {
		t.Fatalf("unexpected client versions: %+v", got)
	}
}

func TestOperationalConfigDefaults(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}

	retry := GetRetryConfig()
	if retry.MaxAccountAttempts != 8 || retry.MaxUpstreamAttempts != 12 || retry.MaxRetryDurationSeconds != 900 ||
		retry.FirstTokenTimeoutSeconds != 45 || retry.ToolAssemblyTimeoutSeconds != 180 || retry.EmptyResponseRetries != 2 {
		t.Fatalf("unexpected retry defaults: %+v", retry)
	}
	refresh := GetAutoRefreshConfig()
	if refresh.RefreshQueueCapacity != 1000 || refresh.RefreshTaskTimeoutSeconds != 60 || refresh.RefreshJitterSeconds != 30 {
		t.Fatalf("unexpected refresh coordinator defaults: %+v", refresh)
	}
	models := GetModelRegistryConfig()
	if models.NegativeCacheTTLSeconds != 3600 || len(models.Models) != 0 {
		t.Fatalf("unexpected model registry defaults: %+v", models)
	}
	health := GetHealthConfig()
	if health.MinReadyAccounts != 1 || health.MinReadyRatio != 0 || health.WebhookCooldownSeconds != 300 {
		t.Fatalf("unexpected health defaults: %+v", health)
	}
}

func TestRetryConfigPersistsUnlimitedAccountAttempts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := Init(path); err != nil {
		t.Fatalf("init config: %v", err)
	}
	retry := GetRetryConfig()
	retry.MaxAccountAttempts = 0
	if err := UpdateRetryConfig(retry); err != nil {
		t.Fatalf("update retry config: %v", err)
	}
	if err := Init(path); err != nil {
		t.Fatalf("reload config: %v", err)
	}
	if got := GetRetryConfig().MaxAccountAttempts; got != 0 {
		t.Fatalf("max account attempts = %d, want unlimited value 0", got)
	}
}

func TestRetryConfigMissingAccountAttemptFieldUsesDefault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := Init(path); err != nil {
		t.Fatalf("init config: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var document map[string]interface{}
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatalf("parse config: %v", err)
	}
	retry, ok := document["retry"].(map[string]interface{})
	if !ok {
		t.Fatalf("retry config has unexpected type %T", document["retry"])
	}
	delete(retry, "maxAccountAttempts")
	raw, err = json.Marshal(document)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if err := Init(path); err != nil {
		t.Fatalf("reload config: %v", err)
	}
	if got := GetRetryConfig().MaxAccountAttempts; got != 8 {
		t.Fatalf("missing max account attempts migrated to %d, want 8", got)
	}
}

func TestRetryConfigMissingNewTimeoutFieldsUsesDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := Init(path); err != nil {
		t.Fatalf("init config: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var document map[string]interface{}
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatalf("parse config: %v", err)
	}
	retry := document["retry"].(map[string]interface{})
	delete(retry, "maxRetryDurationSeconds")
	delete(retry, "toolAssemblyTimeoutSeconds")
	raw, err = json.Marshal(document)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if err := Init(path); err != nil {
		t.Fatalf("reload config: %v", err)
	}
	got := GetRetryConfig()
	if got.MaxRetryDurationSeconds != 900 || got.ToolAssemblyTimeoutSeconds != 180 {
		t.Fatalf("missing timeout fields were not migrated: %+v", got)
	}
}

func TestConfiguredModelResolutionUsesExactThenLongestKeyword(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	registry := ModelRegistryConfig{
		NegativeCacheTTLSeconds: 600,
		Models: []ModelEntry{
			{ID: "fast", KiroModelID: "claude-haiku-4.5", ContextWindow: 200000, MaxTokens: 8192, MatchKeywords: []string{"claude"}},
			{ID: "sonnet-custom", KiroModelID: "claude-sonnet-4.6", ContextWindow: 1000000, MaxTokens: 64000, MatchKeywords: []string{"sonnet-custom", "sonnet"}},
		},
	}
	if err := UpdateModelRegistryConfig(registry); err != nil {
		t.Fatalf("update registry: %v", err)
	}

	if got, ok := ResolveConfiguredModel("fast"); !ok || got.KiroModelID != "claude-haiku-4.5" {
		t.Fatalf("expected exact model match, got %+v ok=%v", got, ok)
	}
	if got, ok := ResolveConfiguredModel("vendor-sonnet-custom-preview"); !ok || got.ID != "sonnet-custom" {
		t.Fatalf("expected longest keyword match, got %+v ok=%v", got, ok)
	}
	if meta, ok := GetConfiguredModelMetadata("claude-sonnet-4.6"); !ok || meta.ContextWindow != 1000000 {
		t.Fatalf("expected upstream model metadata, got %+v ok=%v", meta, ok)
	}
}

func TestModelRegistryRejectsConflictingKeywords(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	err := UpdateModelRegistryConfig(ModelRegistryConfig{
		NegativeCacheTTLSeconds: 600,
		Models: []ModelEntry{
			{ID: "one", KiroModelID: "claude-sonnet-4.6", ContextWindow: 200000, MaxTokens: 1000, MatchKeywords: []string{"shared"}},
			{ID: "two", KiroModelID: "claude-opus-4.6", ContextWindow: 200000, MaxTokens: 1000, MatchKeywords: []string{"shared"}},
		},
	})
	if err == nil {
		t.Fatalf("expected conflicting keyword validation error")
	}
}

func TestAccountStatsBatchIgnoresStaleConfigGeneration(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init old config: %v", err)
	}
	oldGeneration := GetGeneration()

	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init new config: %v", err)
	}
	if err := AddAccount(Account{ID: "same-id", Enabled: true}); err != nil {
		t.Fatalf("add account: %v", err)
	}
	if err := UpdateAccountStatsBatch(oldGeneration, map[string]AccountStatsSnapshot{
		"same-id": {RequestCount: 99, TotalTokens: 999},
	}); err != nil {
		t.Fatalf("stale batch: %v", err)
	}

	accounts := GetAccounts()
	if len(accounts) != 1 || accounts[0].RequestCount != 0 || accounts[0].TotalTokens != 0 {
		t.Fatalf("stale stats modified new config: %+v", accounts)
	}
}

func TestBatchAccountStatusAndInfoUpdatesPreserveCredentials(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	accounts := []Account{
		{ID: "a", AccessToken: "token-a", Enabled: false, BanStatus: "BANNED", BanReason: "old"},
		{ID: "b", AccessToken: "token-b", Enabled: true},
	}
	for _, account := range accounts {
		if err := AddAccount(account); err != nil {
			t.Fatalf("add account: %v", err)
		}
	}
	if err := SetAccountsEnabled(map[string]bool{"a": true}, true); err != nil {
		t.Fatalf("enable batch: %v", err)
	}
	if err := UpdateAccountInfoBatch(map[string]AccountInfo{
		"a": {SubscriptionType: "POWER", UsageCurrent: 10, UsageLimit: 100, LastRefresh: 123},
		"b": {SubscriptionType: "PRO", UsageCurrent: 5, UsageLimit: 50, LastRefresh: 456},
	}); err != nil {
		t.Fatalf("info batch: %v", err)
	}

	got := GetAccounts()
	if len(got) != 2 {
		t.Fatalf("expected two accounts, got %d", len(got))
	}
	byID := map[string]Account{got[0].ID: got[0], got[1].ID: got[1]}
	if a := byID["a"]; !a.Enabled || a.BanStatus != "ACTIVE" || a.AccessToken != "token-a" || a.UsageCurrent != 10 {
		t.Fatalf("unexpected account a: %+v", a)
	}
	if b := byID["b"]; b.AccessToken != "token-b" || b.SubscriptionType != "PRO" || b.LastRefresh != 456 {
		t.Fatalf("unexpected account b: %+v", b)
	}
}

func TestPrivacySensitiveDefaultsAndProxyValidation(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	if GetResponsesStorageConfig().DefaultStore {
		t.Fatal("Responses storage must default to off")
	}
	if got := GetProxyURL(); got != "direct" {
		t.Fatalf("new configs must explicitly select direct egress, got %q", got)
	}
	if err := UpdateProxySettings("http://proxy-without-port"); err == nil {
		t.Fatal("expected malformed global proxy to be rejected")
	}
	if err := AddAccount(Account{ID: "bad-proxy", ProxyURL: "socks5://:1080"}); err == nil {
		t.Fatal("expected malformed account proxy to be rejected")
	}
}

func TestResolveListenAddressUsesDeploymentOverrides(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	t.Setenv("KIRO_LISTEN_HOST", "127.0.0.1")
	t.Setenv("KIRO_LISTEN_PORT", "9090")
	host, port, managed, err := ResolveListenAddress()
	if err != nil || host != "127.0.0.1" || port != 9090 || !managed {
		t.Fatalf("unexpected active listen address: %s:%d managed=%v err=%v", host, port, managed, err)
	}
	t.Setenv("KIRO_LISTEN_PORT", "invalid")
	if _, _, _, err := ResolveListenAddress(); err == nil {
		t.Fatal("expected invalid deployment listen port to be rejected")
	}
}
