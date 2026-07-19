package proxy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPromptCachePersistenceRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), promptCachePersistenceFile)
	tracker := newPromptCacheTrackerWithSettings(time.Hour, 1)
	tracker.ConfigureLimits(10, 20)

	var first, second [32]byte
	first[0] = 1
	second[0] = 2
	tracker.Update("account-a\x00key-a", cachePersistenceProfile(first, 2048, time.Hour))
	tracker.Update("account-b\x00key-b", cachePersistenceProfile(second, 4096, time.Hour))
	if err := tracker.Flush(path); err != nil {
		t.Fatalf("flush prompt cache: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat prompt cache: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("prompt cache mode = %o, want 600", got)
	}

	restored := newPromptCacheTrackerWithSettings(time.Hour, 1)
	restored.ConfigureLimits(10, 20)
	count, err := restored.Load(path)
	if err != nil {
		t.Fatalf("load prompt cache: %v", err)
	}
	if count != 2 || restored.entryCountValue() != 2 {
		t.Fatalf("restored count = %d/%d, want 2", count, restored.entryCountValue())
	}
	if usage := restored.Compute("account-a\x00key-a", cachePersistenceProfile(first, 2048, time.Hour)); usage.CacheReadInputTokens != 2048 {
		t.Fatalf("restored fingerprint did not hit: %+v", usage)
	}
	if usage := restored.Compute("account-a\x00other-key", cachePersistenceProfile(first, 2048, time.Hour)); usage.CacheReadInputTokens != 0 {
		t.Fatalf("restored namespace isolation was lost: %+v", usage)
	}
}

func TestPromptCacheLoadDropsExpiredMalformedAndExcessEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), promptCachePersistenceFile)
	now := time.Now()
	state := persistedPromptCacheState{
		Version: promptCacheStateVersion,
		SavedAt: now.Unix(),
		Entries: []persistedPromptCacheEntry{
			{Scope: "account-a", Fingerprint: "not-hex", ExpiresAt: now.Add(time.Hour).Unix(), TTLSeconds: 3600, LastAccess: now.Unix()},
			{Scope: "account-a", Fingerprint: "0100000000000000000000000000000000000000000000000000000000000000", ExpiresAt: now.Add(-time.Minute).Unix(), TTLSeconds: 3600, LastAccess: now.Unix()},
			{Scope: "account-a", Fingerprint: "0200000000000000000000000000000000000000000000000000000000000000", ExpiresAt: now.Add(time.Hour).Unix(), TTLSeconds: 3600, LastAccess: now.Add(-time.Minute).Unix()},
			{Scope: "account-a", Fingerprint: "0300000000000000000000000000000000000000000000000000000000000000", ExpiresAt: now.Add(time.Hour).Unix(), TTLSeconds: 3600, LastAccess: now.Unix()},
		},
	}
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("marshal state: %v", err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write state: %v", err)
	}

	tracker := newPromptCacheTrackerWithSettings(time.Hour, 1)
	tracker.ConfigureLimits(1, 1)
	count, err := tracker.Load(path)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if count != 1 || tracker.entryCountValue() != 1 || tracker.accountEntryCount("account-a") != 1 {
		t.Fatalf("invalid entries were not dropped: count=%d total=%d account=%d", count, tracker.entryCountValue(), tracker.accountEntryCount("account-a"))
	}
	var newest [32]byte
	newest[0] = 3
	if _, ok := tracker.entry("account-a", newest); !ok {
		t.Fatal("LRU restore did not retain the newest valid entry")
	}
}

func TestPromptCacheRemovePersisted(t *testing.T) {
	path := filepath.Join(t.TempDir(), promptCachePersistenceFile)
	tracker := newPromptCacheTrackerWithSettings(time.Hour, 1)
	var fingerprint [32]byte
	tracker.Update("account-a", cachePersistenceProfile(fingerprint, 2048, time.Hour))
	if err := tracker.Flush(path); err != nil {
		t.Fatalf("flush state: %v", err)
	}
	if err := tracker.RemovePersisted(path); err != nil {
		t.Fatalf("remove state: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("persisted file still exists: %v", err)
	}
}

func cachePersistenceProfile(fingerprint [32]byte, tokens int, ttl time.Duration) *promptCacheProfile {
	return &promptCacheProfile{
		Model:            "claude-sonnet-4.6",
		TotalInputTokens: tokens,
		Breakpoints: []promptCacheBreakpoint{{
			Fingerprint:      fingerprint,
			CumulativeTokens: tokens,
			TTL:              ttl,
		}},
	}
}
