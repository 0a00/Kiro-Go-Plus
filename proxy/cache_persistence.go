package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"kiro-go/config"
	"os"
	"path/filepath"
	"sort"
	"time"
)

const (
	promptCacheStateVersion      = 1
	promptCachePersistenceFile   = "prompt_cache.json"
	maxPersistedPromptCacheBytes = 64 << 20
)

type persistedPromptCacheState struct {
	Version int                         `json:"version"`
	SavedAt int64                       `json:"savedAt"`
	Entries []persistedPromptCacheEntry `json:"entries"`
}

type persistedPromptCacheEntry struct {
	Scope       string `json:"scope"`
	Fingerprint string `json:"fingerprint"`
	ExpiresAt   int64  `json:"expiresAt"`
	TTLSeconds  int64  `json:"ttlSeconds"`
	LastAccess  int64  `json:"lastAccess"`
}

func promptCachePath() string {
	return filepath.Join(config.GetConfigDir(), promptCachePersistenceFile)
}

func (t *promptCacheTracker) markStateChanged() {
	if t != nil {
		t.stateGeneration.Add(1)
	}
}

func (t *promptCacheTracker) Load(path string) (int, error) {
	if t == nil || path == "" {
		return 0, nil
	}
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("stat prompt cache: %w", err)
	}
	if info.Size() > maxPersistedPromptCacheBytes {
		return 0, fmt.Errorf("prompt cache state exceeds %d bytes", maxPersistedPromptCacheBytes)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("read prompt cache: %w", err)
	}
	var state persistedPromptCacheState
	if err := json.Unmarshal(raw, &state); err != nil {
		return 0, fmt.Errorf("decode prompt cache: %w", err)
	}
	if state.Version != promptCacheStateVersion {
		return 0, fmt.Errorf("unsupported prompt cache version %d", state.Version)
	}

	t.Clear()
	now := time.Now()
	maxTTL, maxPerAccount, maxTotal := t.settingsSnapshot()
	for _, diskEntry := range state.Entries {
		if diskEntry.Scope == "" || diskEntry.ExpiresAt <= now.Unix() || diskEntry.TTLSeconds <= 0 {
			continue
		}
		fingerprintBytes, err := hex.DecodeString(diskEntry.Fingerprint)
		if err != nil || len(fingerprintBytes) != sha256.Size {
			continue
		}
		var fingerprint [sha256.Size]byte
		copy(fingerprint[:], fingerprintBytes)
		ttl := time.Duration(diskEntry.TTLSeconds) * time.Second
		if maxTTL > 0 && ttl > maxTTL {
			ttl = maxTTL
		}
		expiresAt := time.Unix(diskEntry.ExpiresAt, 0)
		if deadline := now.Add(ttl); expiresAt.After(deadline) {
			expiresAt = deadline
		}
		lastAccess := time.Unix(diskEntry.LastAccess, 0)
		if diskEntry.LastAccess <= 0 || lastAccess.After(now) {
			lastAccess = now
		}
		shard := t.shardFor(diskEntry.Scope)
		shard.mu.Lock()
		t.putEntryLocked(shard, diskEntry.Scope, fingerprint, expiresAt, ttl, lastAccess)
		shard.mu.Unlock()
	}
	t.enforceAllAccountLimits(maxPerAccount)
	t.enforceGlobalLimit(maxTotal)
	// Force one normalized rewrite after load so expired, malformed, or excess
	// entries disappear from disk and the current format/limits become canonical.
	t.markStateChanged()
	return t.entryCountValue(), nil
}

func (t *promptCacheTracker) Flush(path string) error {
	if t == nil || path == "" {
		return nil
	}
	t.persistMu.Lock()
	defer t.persistMu.Unlock()

	generation := t.stateGeneration.Load()
	if generation == t.persistedGeneration.Load() {
		return nil
	}
	now := time.Now()
	entries := make([]persistedPromptCacheEntry, 0, t.entryCountValue())
	for i := range t.shards {
		shard := &t.shards[i]
		shard.mu.Lock()
		for scope, scopedEntries := range shard.entriesByAccount {
			for fingerprint, entry := range scopedEntries {
				if entry == nil || !entry.ExpiresAt.After(now) {
					continue
				}
				entries = append(entries, persistedPromptCacheEntry{
					Scope:       scope,
					Fingerprint: hex.EncodeToString(fingerprint[:]),
					ExpiresAt:   entry.ExpiresAt.Unix(),
					TTLSeconds:  int64(entry.TTL.Seconds()),
					LastAccess:  entry.LastAccess.Unix(),
				})
			}
		}
		shard.mu.Unlock()
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].LastAccess != entries[j].LastAccess {
			return entries[i].LastAccess < entries[j].LastAccess
		}
		if entries[i].Scope == entries[j].Scope {
			return entries[i].Fingerprint < entries[j].Fingerprint
		}
		return entries[i].Scope < entries[j].Scope
	})
	raw, err := json.Marshal(persistedPromptCacheState{
		Version: promptCacheStateVersion,
		SavedAt: now.Unix(),
		Entries: entries,
	})
	if err != nil {
		return fmt.Errorf("encode prompt cache: %w", err)
	}
	if len(raw) > maxPersistedPromptCacheBytes {
		return fmt.Errorf("encoded prompt cache exceeds %d bytes", maxPersistedPromptCacheBytes)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create prompt cache directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".prompt-cache-*.tmp")
	if err != nil {
		return fmt.Errorf("create prompt cache temp file: %w", err)
	}
	tmpPath := tmp.Name()
	committed := false
	defer func() {
		_ = tmp.Close()
		if !committed {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return fmt.Errorf("secure prompt cache temp file: %w", err)
	}
	if _, err := tmp.Write(raw); err != nil {
		return fmt.Errorf("write prompt cache: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync prompt cache: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close prompt cache: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("commit prompt cache: %w", err)
	}
	committed = true
	t.persistedGeneration.Store(generation)
	return nil
}

func (t *promptCacheTracker) RemovePersisted(path string) error {
	if t == nil || path == "" {
		return nil
	}
	t.persistMu.Lock()
	defer t.persistMu.Unlock()
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove prompt cache: %w", err)
	}
	t.persistedGeneration.Store(t.stateGeneration.Load())
	return nil
}
