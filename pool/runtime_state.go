package pool

import (
	"encoding/json"
	"kiro-go/config"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const runtimeStateVersion = 1

type persistedAccountRuntimeState struct {
	CooldownUntil        int64 `json:"cooldownUntil,omitempty"`
	RefreshCooldownUntil int64 `json:"refreshCooldownUntil,omitempty"`
	ErrorCount           int   `json:"errorCount,omitempty"`
	LastSuccessAt        int64 `json:"lastSuccessAt,omitempty"`
}

type persistedModelNegativeState struct {
	AccountID     string `json:"accountId"`
	Model         string `json:"model"`
	CooldownUntil int64  `json:"cooldownUntil"`
}

type persistedUpstreamState struct {
	AccountID      string `json:"accountId,omitempty"`
	ProfileArn     string `json:"profileArn,omitempty"`
	Model          string `json:"model"`
	RateLimitCount int    `json:"rateLimitCount,omitempty"`
	CooldownUntil  int64  `json:"cooldownUntil,omitempty"`
	LastCooldownMs int    `json:"lastCooldownMs,omitempty"`
}

type persistedRuntimeState struct {
	Version       int                                     `json:"version"`
	SavedAt       int64                                   `json:"savedAt"`
	CurrentIndex  uint64                                  `json:"currentIndex"`
	RefreshCursor int                                     `json:"refreshCursor"`
	Accounts      map[string]persistedAccountRuntimeState `json:"accounts,omitempty"`
	ModelLists    map[string][]string                     `json:"modelLists,omitempty"`
	ModelNegative []persistedModelNegativeState           `json:"modelNegative,omitempty"`
	Upstream      []persistedUpstreamState                `json:"upstream,omitempty"`
	Profiles      []persistedUpstreamState                `json:"profiles,omitempty"`
}

func runtimeStatePath() string {
	return filepath.Join(config.GetConfigDir(), "runtime_state.json")
}

func (p *AccountPool) loadRuntimeState() {
	data, err := os.ReadFile(runtimeStatePath())
	if err != nil {
		return
	}
	var state persistedRuntimeState
	if json.Unmarshal(data, &state) != nil || state.Version != runtimeStateVersion {
		return
	}

	now := time.Now()
	p.mu.Lock()
	p.ensureProtectionMapsLocked()
	p.currentIndex.Store(state.CurrentIndex)
	p.refreshCursor = state.RefreshCursor
	for accountID, entry := range state.Accounts {
		p.errorCounts[accountID] = entry.ErrorCount
		if entry.CooldownUntil > now.Unix() {
			p.cooldowns[accountID] = time.Unix(entry.CooldownUntil, 0)
		}
		if entry.LastSuccessAt > 0 {
			p.lastSuccess[accountID] = time.Unix(entry.LastSuccessAt, 0)
		}
		if entry.RefreshCooldownUntil > now.Unix() {
			p.refreshFailures[accountID] = time.Unix(entry.RefreshCooldownUntil, 0)
		}
	}
	for accountID, models := range state.ModelLists {
		set := make(map[string]bool, len(models))
		for _, model := range models {
			if model = strings.ToLower(strings.TrimSpace(model)); model != "" {
				set[model] = true
			}
		}
		if len(set) > 0 {
			p.modelLists[accountID] = set
		}
	}
	for _, entry := range state.ModelNegative {
		if entry.CooldownUntil > now.Unix() {
			p.modelNegative[modelAvailabilityKey{accountID: entry.AccountID, model: strings.ToLower(strings.TrimSpace(entry.Model))}] = time.Unix(entry.CooldownUntil, 0)
		}
	}
	for _, entry := range state.Upstream {
		if entry.CooldownUntil <= now.Unix() {
			continue
		}
		p.upstream[upstreamStateKey{accountID: entry.AccountID, model: entry.Model}] = upstreamRuntimeState{
			rateLimitCount: entry.RateLimitCount,
			cooldownUntil:  time.Unix(entry.CooldownUntil, 0),
			lastCooldownMs: entry.LastCooldownMs,
		}
	}
	for _, entry := range state.Profiles {
		if entry.CooldownUntil <= now.Unix() {
			continue
		}
		p.profiles[profileStateKey{profileArn: entry.ProfileArn, model: entry.Model}] = upstreamRuntimeState{
			rateLimitCount: entry.RateLimitCount,
			cooldownUntil:  time.Unix(entry.CooldownUntil, 0),
			lastCooldownMs: entry.LastCooldownMs,
		}
	}
	p.mu.Unlock()
}

func (p *AccountPool) scheduleRuntimeStateSave() {
	if p == nil {
		return
	}
	path := runtimeStatePath()
	p.stateSaveMu.Lock()
	defer p.stateSaveMu.Unlock()
	if p.stateSaveTimer != nil {
		return
	}
	// Capture the destination when the save is scheduled. Config can be
	// reinitialized in tests and administrative reloads; resolving the path in
	// the callback could otherwise let an old pool overwrite a new instance's
	// runtime state.
	p.stateSaveTimer = time.AfterFunc(750*time.Millisecond, func() {
		p.stateSaveMu.Lock()
		p.stateSaveTimer = nil
		p.stateSaveMu.Unlock()
		p.saveRuntimeStateTo(path)
	})
}

// FlushRuntimeState persists cooldowns, model caches, and refresh cursors now.
func (p *AccountPool) FlushRuntimeState() {
	if p == nil {
		return
	}
	p.stateSaveMu.Lock()
	if p.stateSaveTimer != nil {
		p.stateSaveTimer.Stop()
		p.stateSaveTimer = nil
	}
	p.stateSaveMu.Unlock()
	p.saveRuntimeState()
}

func (p *AccountPool) saveRuntimeState() {
	p.saveRuntimeStateTo(runtimeStatePath())
}

func (p *AccountPool) saveRuntimeStateTo(path string) {
	p.stateSaveMu.Lock()
	defer p.stateSaveMu.Unlock()
	now := time.Now()
	state := persistedRuntimeState{
		Version:       runtimeStateVersion,
		SavedAt:       now.Unix(),
		CurrentIndex:  p.currentIndex.Load(),
		Accounts:      make(map[string]persistedAccountRuntimeState),
		ModelLists:    make(map[string][]string),
		RefreshCursor: 0,
	}

	p.mu.RLock()
	state.RefreshCursor = p.refreshCursor
	for accountID, errorCount := range p.errorCounts {
		entry := persistedAccountRuntimeState{ErrorCount: errorCount}
		if cooldown := p.cooldowns[accountID]; cooldown.After(now) {
			entry.CooldownUntil = cooldown.Unix()
		}
		if success := p.lastSuccess[accountID]; !success.IsZero() {
			entry.LastSuccessAt = success.Unix()
		}
		if refreshCooldown := p.refreshFailures[accountID]; refreshCooldown.After(now) {
			entry.RefreshCooldownUntil = refreshCooldown.Unix()
		}
		state.Accounts[accountID] = entry
	}
	for accountID, refreshCooldown := range p.refreshFailures {
		if _, ok := state.Accounts[accountID]; ok || !refreshCooldown.After(now) {
			continue
		}
		state.Accounts[accountID] = persistedAccountRuntimeState{RefreshCooldownUntil: refreshCooldown.Unix()}
	}
	for accountID, models := range p.modelLists {
		list := make([]string, 0, len(models))
		for model := range models {
			list = append(list, model)
		}
		state.ModelLists[accountID] = list
	}
	for key, until := range p.modelNegative {
		if until.After(now) {
			state.ModelNegative = append(state.ModelNegative, persistedModelNegativeState{AccountID: key.accountID, Model: key.model, CooldownUntil: until.Unix()})
		}
	}
	for key, entry := range p.upstream {
		if entry.cooldownUntil.After(now) {
			state.Upstream = append(state.Upstream, persistedUpstreamState{
				AccountID: key.accountID, Model: key.model, RateLimitCount: entry.rateLimitCount,
				CooldownUntil: entry.cooldownUntil.Unix(), LastCooldownMs: entry.lastCooldownMs,
			})
		}
	}
	for key, entry := range p.profiles {
		if entry.cooldownUntil.After(now) {
			state.Profiles = append(state.Profiles, persistedUpstreamState{
				ProfileArn: key.profileArn, Model: key.model, RateLimitCount: entry.rateLimitCount,
				CooldownUntil: entry.cooldownUntil.Unix(), LastCooldownMs: entry.lastCooldownMs,
			})
		}
	}
	p.mu.RUnlock()

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return
	}
	if os.MkdirAll(filepath.Dir(path), 0755) != nil {
		return
	}
	tmp := path + ".tmp"
	if os.WriteFile(tmp, data, 0600) != nil {
		return
	}
	_ = os.Rename(tmp, path)
}
