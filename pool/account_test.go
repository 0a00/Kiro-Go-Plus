package pool

import (
	"errors"
	"kiro-go/config"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestOverLimitAccountsAreSkippedByDefault(t *testing.T) {
	p := &AccountPool{}
	normal := config.Account{ID: "normal"}
	overLimit := config.Account{ID: "over", UsageCurrent: 10, UsageLimit: 10}

	p.accounts = []config.Account{normal, overLimit}

	for i := 0; i < 5; i++ {
		acc := p.GetNext()
		if acc == nil {
			t.Fatalf("expected an account")
		}
		if acc.ID == "over" {
			t.Fatalf("expected over-limit account to be skipped when upstream OverageStatus is empty")
		}
	}
}

func TestOverLimitAccountsCanBeSelectedWhenUpstreamOverageEnabled(t *testing.T) {
	p := &AccountPool{}
	overLimit := config.Account{
		ID:            "over",
		UsageCurrent:  10,
		UsageLimit:    10,
		OverageStatus: "ENABLED",
	}

	p.accounts = []config.Account{overLimit}

	acc := p.GetNext()
	if acc == nil {
		t.Fatalf("expected upstream-enabled overage account to be selectable")
	}
	if acc.ID != "over" {
		t.Fatalf("expected overage account, got %q", acc.ID)
	}
}

func TestOverLimitAccountsRemainSkippedWhenUpstreamOverageDisabled(t *testing.T) {
	p := &AccountPool{}
	overLimit := config.Account{
		ID:            "over",
		UsageCurrent:  10,
		UsageLimit:    10,
		OverageStatus: "DISABLED",
	}

	p.accounts = []config.Account{overLimit}

	if acc := p.GetNext(); acc != nil {
		t.Fatalf("expected nil when upstream OverageStatus=DISABLED, got %q", acc.ID)
	}
}

func TestGetNextKeepsFiveMinuteTokenAvailable(t *testing.T) {
	p := &AccountPool{}
	account := config.Account{
		ID:          "acct-1",
		AccessToken: "access-token",
		ExpiresAt:   time.Now().Unix() + 300,
	}

	p.accounts = []config.Account{account}

	got := p.GetNext()
	if got == nil {
		t.Fatalf("expected five-minute token to be available")
	}
	if got.ID != account.ID {
		t.Fatalf("expected account %q, got %q", account.ID, got.ID)
	}
}

func TestGetNextReturnsAccountSnapshot(t *testing.T) {
	p := newTestPool(config.Account{ID: "snapshot", AccessToken: "original"})
	got := p.GetNext()
	if got == nil {
		t.Fatal("expected an account")
	}
	got.AccessToken = "mutated"
	got.RequestCount = 99

	p.mu.RLock()
	stored := p.accounts[0]
	p.mu.RUnlock()
	if stored.AccessToken != "original" || stored.RequestCount != 0 {
		t.Fatalf("selection leaked internal account pointer: %+v", stored)
	}
}

func TestAccountStatsAreCoalescedAndPersisted(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	account := config.Account{ID: "stats", Enabled: true}
	if err := config.AddAccount(account); err != nil {
		t.Fatalf("config.AddAccount: %v", err)
	}
	p := &AccountPool{
		accounts:         []config.Account{account, account},
		dirtyStats:       make(map[string]struct{}),
		configGeneration: config.GetGeneration(),
	}
	t.Cleanup(func() {
		p.statsSaveMu.Lock()
		if p.statsSaveTimer != nil {
			p.statsSaveTimer.Stop()
		}
		p.statsSaveMu.Unlock()
		p.stateSaveMu.Lock()
		if p.stateSaveTimer != nil {
			p.stateSaveTimer.Stop()
		}
		p.stateSaveMu.Unlock()
	})

	p.UpdateStats(account.ID, 10, 0.1)
	p.UpdateStats(account.ID, 20, 0.2)
	p.RecordError(account.ID, false)
	p.flushAccountStats()

	accounts := config.GetAccounts()
	if len(accounts) != 1 {
		t.Fatalf("expected one persisted account, got %d", len(accounts))
	}
	got := accounts[0]
	if got.RequestCount != 2 || got.ErrorCount != 1 || got.TotalTokens != 30 || got.TotalCredits < 0.299 || got.TotalCredits > 0.301 || got.LastUsed == 0 {
		t.Fatalf("unexpected persisted stats: %+v", got)
	}
	for _, weighted := range p.GetAllAccounts() {
		if weighted.RequestCount != 2 || weighted.ErrorCount != 1 || weighted.TotalTokens != 30 {
			t.Fatalf("weighted account copies diverged: %+v", weighted)
		}
	}
}

// ---------------------------------------------------------------------------
// IsAuthFailure
// ---------------------------------------------------------------------------

func TestIsAuthFailureRecognizes401And403(t *testing.T) {
	positives := []string{
		"HTTP 401 from server",
		"received 403 Forbidden",
		"bad credentials",
		"invalid_grant",
		"invalid_token",
		"token expired",
		"token has expired",
		"unauthorized",
	}
	for _, msg := range positives {
		if !IsAuthFailure(errors.New(msg)) {
			t.Errorf("IsAuthFailure(%q) = false, want true", msg)
		}
	}
}

func TestIsAuthFailureIgnoresFalsePositives(t *testing.T) {
	// hasStatusToken only excludes digit boundaries; e.g. "4011" contains "401"
	// but the trailing '1' is a digit so it does NOT match.
	negatives := []string{
		"status code 4011 found", // digit immediately after 401 → not a standalone token
		"error 14013 exceeded",   // digit before and after 401
		"some random error",
		"status 200 OK",
	}
	for _, msg := range negatives {
		if IsAuthFailure(errors.New(msg)) {
			t.Errorf("IsAuthFailure(%q) = true, want false", msg)
		}
	}
}

func TestIsAuthFailureNilError(t *testing.T) {
	if IsAuthFailure(nil) {
		t.Fatal("IsAuthFailure(nil) = true, want false")
	}
}

// ---------------------------------------------------------------------------
// IsSuspensionError
// ---------------------------------------------------------------------------

func TestIsSuspensionErrorDetectsKnownMessages(t *testing.T) {
	positives := []string{
		"account temporarily_suspended",
		"account temporarily suspended",
		"no available kiro profile",
		"No Available Kiro Profile", // case-insensitive
	}
	for _, msg := range positives {
		if !IsSuspensionError(errors.New(msg)) {
			t.Errorf("IsSuspensionError(%q) = false, want true", msg)
		}
	}
}

func TestIsSuspensionErrorIgnoresUnrelatedErrors(t *testing.T) {
	negatives := []string{
		"some other error",
		"unauthorized",
		"429 too many requests",
	}
	for _, msg := range negatives {
		if IsSuspensionError(errors.New(msg)) {
			t.Errorf("IsSuspensionError(%q) = true, want false", msg)
		}
	}
}

func TestIsSuspensionErrorNilError(t *testing.T) {
	if IsSuspensionError(nil) {
		t.Fatal("IsSuspensionError(nil) = true, want false")
	}
}

// ---------------------------------------------------------------------------
// GetNextForModelExcluding
// ---------------------------------------------------------------------------

func newTestPool(accounts ...config.Account) *AccountPool {
	p := &AccountPool{
		cooldowns:   make(map[string]time.Time),
		errorCounts: make(map[string]int),
		modelLists:  make(map[string]map[string]bool),
	}
	p.accounts = accounts
	return p
}

func initWeightedRoutingTest(t *testing.T) {
	t.Helper()
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.UpdateRoutingConfig(config.RoutingConfig{LoadBalancingMode: "weighted"}); err != nil {
		t.Fatalf("UpdateRoutingConfig: %v", err)
	}
}

func TestSmoothWeightedRoutingDistribution(t *testing.T) {
	initWeightedRoutingTest(t)

	tests := []struct {
		name      string
		weightA   int
		weightB   int
		requests  int
		expectedA int
		expectedB int
	}{
		{name: "one-to-one", weightA: 1, weightB: 1, requests: 100, expectedA: 50, expectedB: 50},
		{name: "two-to-one", weightA: 2, weightB: 1, requests: 300, expectedA: 200, expectedB: 100},
		{name: "ten-to-one", weightA: 10, weightB: 1, requests: 1100, expectedA: 1000, expectedB: 100},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := newTestPool(
				config.Account{ID: "a", Weight: tt.weightA},
				config.Account{ID: "b", Weight: tt.weightB},
			)
			counts := map[string]int{}
			for i := 0; i < tt.requests; i++ {
				account := p.GetNext()
				if account == nil {
					t.Fatal("expected an account")
				}
				counts[account.ID]++
			}
			if counts["a"] != tt.expectedA || counts["b"] != tt.expectedB {
				t.Fatalf("unexpected distribution: %+v", counts)
			}
		})
	}
}

func TestSmoothWeightedRoutingConcurrentSelection(t *testing.T) {
	initWeightedRoutingTest(t)
	p := newTestPool(
		config.Account{ID: "a", Weight: 2},
		config.Account{ID: "b", Weight: 1},
	)

	counts := map[string]int{}
	var countsMu sync.Mutex
	var wg sync.WaitGroup
	for worker := 0; worker < 30; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				account := p.GetNext()
				if account == nil {
					continue
				}
				countsMu.Lock()
				counts[account.ID]++
				countsMu.Unlock()
			}
		}()
	}
	wg.Wait()
	if counts["a"] != 2000 || counts["b"] != 1000 {
		t.Fatalf("unexpected concurrent distribution: %+v", counts)
	}
}

func TestReloadStoresEachWeightedAccountOnceAndClampsWeight(t *testing.T) {
	initWeightedRoutingTest(t)
	if err := config.AddAccount(config.Account{ID: "a", Enabled: true, Weight: 100000}); err != nil {
		t.Fatalf("AddAccount a: %v", err)
	}
	if err := config.AddAccount(config.Account{ID: "b", Enabled: true, Weight: 2}); err != nil {
		t.Fatalf("AddAccount b: %v", err)
	}

	p := newTestPool()
	p.Reload()
	accounts := p.GetAllAccounts()
	if len(accounts) != 2 {
		t.Fatalf("expected two unique accounts, got %d", len(accounts))
	}
	if accounts[0].ID == "a" && accounts[0].Weight != maxAccountWeight {
		t.Fatalf("expected clamped weight %d, got %d", maxAccountWeight, accounts[0].Weight)
	}
}

func TestGetNextForModelExcludingSkipsExcludedAccounts(t *testing.T) {
	p := newTestPool(
		config.Account{ID: "a"},
		config.Account{ID: "b"},
	)
	excluded := map[string]bool{"a": true}
	for i := 0; i < 5; i++ {
		acc := p.GetNextForModelExcluding("model", excluded)
		if acc == nil {
			t.Fatal("expected account b, got nil")
		}
		if acc.ID == "a" {
			t.Fatalf("excluded account a was returned on iteration %d", i)
		}
	}
}

func TestGetNextForModelExcludingReturnsNilWhenAllExcluded(t *testing.T) {
	p := newTestPool(config.Account{ID: "only"})
	acc := p.GetNextForModelExcluding("model", map[string]bool{"only": true})
	if acc != nil {
		t.Fatalf("expected nil when only account is excluded, got %q", acc.ID)
	}
}

func TestGetNextForModelExcludingReturnsNilOnEmptyPool(t *testing.T) {
	p := newTestPool()
	acc := p.GetNextForModelExcluding("model", map[string]bool{})
	if acc != nil {
		t.Fatalf("expected nil for empty pool, got %q", acc.ID)
	}
}

func TestPriorityModePrefersLowerPriority(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.UpdateRoutingConfig(config.RoutingConfig{LoadBalancingMode: "priority"}); err != nil {
		t.Fatalf("UpdateRoutingConfig: %v", err)
	}

	p := newTestPool(
		config.Account{ID: "slow", Priority: 10},
		config.Account{ID: "fast", Priority: 1},
	)
	acc := p.GetNext()
	if acc == nil || acc.ID != "fast" {
		t.Fatalf("expected fast account, got %#v", acc)
	}
}

func TestBalancedModeRoundRobinsWithoutCatchingUpNewAccounts(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.UpdateRoutingConfig(config.RoutingConfig{LoadBalancingMode: "balanced"}); err != nil {
		t.Fatalf("UpdateRoutingConfig: %v", err)
	}

	p := newTestPool(
		config.Account{ID: "established", Priority: 0, RequestCount: 20000},
		config.Account{ID: "new", Priority: 0, RequestCount: 0},
	)
	first := p.GetNext()
	second := p.GetNext()
	third := p.GetNext()
	if first == nil || second == nil || third == nil {
		t.Fatalf("expected three routed accounts, got %#v %#v %#v", first, second, third)
	}
	if first.ID == second.ID || third.ID != first.ID {
		t.Fatalf("expected round-robin order, got %q, %q, %q", first.ID, second.ID, third.ID)
	}
}

func TestBalancedModeKeepsLowerPriorityTierFirst(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.UpdateRoutingConfig(config.RoutingConfig{LoadBalancingMode: "balanced"}); err != nil {
		t.Fatalf("UpdateRoutingConfig: %v", err)
	}
	p := newTestPool(
		config.Account{ID: "fallback", Priority: 10},
		config.Account{ID: "preferred", Priority: 1},
	)
	if got := p.GetNext(); got == nil || got.ID != "preferred" {
		t.Fatalf("expected lower-priority value first, got %#v", got)
	}
}

func TestModelListsAreAdvisoryAndAdvertisedModelsArePreferred(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	p := newTestPool(
		config.Account{ID: "unknown", Enabled: true},
		config.Account{ID: "advertised", Enabled: true},
	)
	p.SetModelList("unknown", []string{"claude-sonnet-4.5"})
	p.SetModelList("advertised", []string{"future-model"})
	if got := p.GetNextForModel("future-model"); got == nil || got.ID != "advertised" {
		t.Fatalf("expected advertised model account first, got %+v", got)
	}
	p.RecordModelUnavailable("advertised", "future-model")
	if got := p.GetNextForModel("future-model"); got == nil || got.ID != "unknown" {
		t.Fatalf("expected unknown account to remain probeable, got %+v", got)
	}
	p.ClearModelUnavailable("advertised", "future-model")
	if !p.modelLists["advertised"]["future-model"] {
		t.Fatal("expected successful probe to learn positive model support")
	}
}

// ---------------------------------------------------------------------------
// DisableAccount
// ---------------------------------------------------------------------------

func TestDisableAccountSetsCooldown(t *testing.T) {
	// Initialize a temporary config so SetAccountBanStatus can persist safely.
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}

	p := newTestPool()
	p.DisableAccount("test-id", "test reason")

	p.mu.RLock()
	cooldown, ok := p.cooldowns["test-id"]
	p.mu.RUnlock()

	if !ok {
		t.Fatal("expected cooldown to be set after DisableAccount")
	}
	// Safety-net cooldown must be at least 23 hours from now.
	minExpected := time.Now().Add(23 * time.Hour)
	if cooldown.Before(minExpected) {
		t.Fatalf("expected cooldown >= 23h in future, got %v", cooldown)
	}
}

func TestGetNextExcludingSkipsExcludedAccount(t *testing.T) {
	p := &AccountPool{
		accounts: []config.Account{
			{ID: "a", Enabled: true},
			{ID: "b", Enabled: true},
		},
		cooldowns:   make(map[string]time.Time),
		errorCounts: make(map[string]int),
		modelLists:  make(map[string]map[string]bool),
	}
	p.currentIndex.Store(^uint64(0))

	acc := p.GetNextExcluding(map[string]bool{"a": true})
	if acc == nil || acc.ID != "b" {
		t.Fatalf("expected account b, got %#v", acc)
	}
}

func TestGetNextForModelExcludingSkipsExcludedAccount(t *testing.T) {
	p := &AccountPool{
		accounts: []config.Account{
			{ID: "a", Enabled: true},
			{ID: "b", Enabled: true},
		},
		cooldowns:   make(map[string]time.Time),
		errorCounts: make(map[string]int),
		modelLists:  make(map[string]map[string]bool),
	}
	p.currentIndex.Store(^uint64(0))
	p.SetModelList("a", []string{"claude-sonnet-4.5"})
	p.SetModelList("b", []string{"claude-sonnet-4.5"})

	acc := p.GetNextForModelExcluding("claude-sonnet-4.5", map[string]bool{"a": true})
	if acc == nil || acc.ID != "b" {
		t.Fatalf("expected account b, got %#v", acc)
	}
}

func initProtectionTestConfig(t *testing.T, up config.UpstreamProtectionConfig) {
	t.Helper()
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.UpdateUpstreamProtectionConfig(up); err != nil {
		t.Fatalf("UpdateUpstreamProtectionConfig: %v", err)
	}
}

func TestAcquireForModelRespectsPerAccountConcurrency(t *testing.T) {
	initProtectionTestConfig(t, config.UpstreamProtectionConfig{
		Enabled:                       true,
		MaxPerAccountModelConcurrency: 1,
		RateLimitCooldownMs:           10,
		MaxRateLimitCooldownMs:        100,
	})
	p := newTestPool(config.Account{ID: "a", Enabled: true})

	acc, guard, err := p.AcquireForModel("claude-sonnet-4.5", "", nil)
	if err != nil || acc == nil || guard == nil {
		t.Fatalf("expected first acquire, acc=%#v guard=%#v err=%v", acc, guard, err)
	}

	acc2, guard2, err2 := p.AcquireForModel("claude-sonnet-4.5", "", nil)
	if err2 == nil || acc2 != nil || guard2 != nil {
		t.Fatalf("expected busy second acquire, acc=%#v guard=%#v err=%v", acc2, guard2, err2)
	}

	guard.Release()
	acc3, guard3, err3 := p.AcquireForModel("claude-sonnet-4.5", "", nil)
	if err3 != nil || acc3 == nil || guard3 == nil {
		t.Fatalf("expected acquire after release, acc=%#v guard=%#v err=%v", acc3, guard3, err3)
	}
	guard3.Release()
}

func TestAcquireForModelFallsBackToRefreshableExpiredAccount(t *testing.T) {
	initProtectionTestConfig(t, config.UpstreamProtectionConfig{
		Enabled:                       true,
		MaxPerAccountModelConcurrency: 1,
	})
	p := newTestPool(config.Account{
		ID:           "expired",
		Enabled:      true,
		AccessToken:  "old-access",
		RefreshToken: "refreshable",
		ExpiresAt:    time.Now().Add(-time.Minute).Unix(),
	})

	account, guard, err := p.AcquireForModel("claude-sonnet-4.5", "", nil)
	if err != nil || account == nil || guard == nil {
		t.Fatalf("expected refreshable fallback, account=%#v guard=%#v err=%v", account, guard, err)
	}
	if account.ID != "expired" {
		t.Fatalf("expected expired refreshable account, got %q", account.ID)
	}
	guard.Release()
}

func TestAcquireForModelPrefersReadyAccountOverRefreshableExpiredAccount(t *testing.T) {
	initProtectionTestConfig(t, config.UpstreamProtectionConfig{
		Enabled:                       true,
		MaxPerAccountModelConcurrency: 1,
	})
	p := newTestPool(
		config.Account{
			ID:           "expired",
			Enabled:      true,
			AccessToken:  "old-access",
			RefreshToken: "refreshable",
			ExpiresAt:    time.Now().Add(-time.Minute).Unix(),
			Weight:       100,
		},
		config.Account{
			ID:          "ready",
			Enabled:     true,
			AccessToken: "valid-access",
			ExpiresAt:   time.Now().Add(time.Hour).Unix(),
			Weight:      1,
		},
	)

	account, guard, err := p.AcquireForModel("claude-sonnet-4.5", "", nil)
	if err != nil || account == nil || guard == nil {
		t.Fatalf("expected ready account, account=%#v guard=%#v err=%v", account, guard, err)
	}
	if account.ID != "ready" {
		t.Fatalf("expected ready account to win, got %q", account.ID)
	}
	guard.Release()
}

func TestAcquireForModelSkipsExpiredAccountInRefreshFailureCooldown(t *testing.T) {
	initProtectionTestConfig(t, config.UpstreamProtectionConfig{
		Enabled:                       true,
		MaxPerAccountModelConcurrency: 1,
	})
	p := newTestPool(config.Account{
		ID:           "expired",
		Enabled:      true,
		AccessToken:  "old-access",
		RefreshToken: "refreshable",
		ExpiresAt:    time.Now().Add(-time.Minute).Unix(),
	})
	p.SetRefreshFailureCooldown("expired", time.Now().Add(time.Minute))

	account, guard, err := p.AcquireForModel("claude-sonnet-4.5", "", nil)
	if err != nil || account != nil || guard != nil {
		t.Fatalf("expected refresh cooldown to suppress fallback, account=%#v guard=%#v err=%v", account, guard, err)
	}
}

func TestAcquireForModelUsesAccountConcurrencyOverride(t *testing.T) {
	initProtectionTestConfig(t, config.UpstreamProtectionConfig{
		Enabled:                       true,
		MaxPerAccountModelConcurrency: 5,
		RateLimitCooldownMs:           10,
		MaxRateLimitCooldownMs:        100,
	})
	p := newTestPool(config.Account{ID: "a", Enabled: true, MaxConcurrency: 2})

	_, guard1, err1 := p.AcquireForModel("claude-sonnet-4.5", "", nil)
	if err1 != nil || guard1 == nil {
		t.Fatalf("expected first acquire, guard=%#v err=%v", guard1, err1)
	}
	defer guard1.Release()

	_, guard2, err2 := p.AcquireForModel("claude-sonnet-4.5", "", nil)
	if err2 != nil || guard2 == nil {
		t.Fatalf("expected second acquire, guard=%#v err=%v", guard2, err2)
	}
	defer guard2.Release()

	acc3, guard3, err3 := p.AcquireForModel("claude-sonnet-4.5", "", nil)
	if err3 == nil || acc3 != nil || guard3 != nil {
		t.Fatalf("expected account override to reject third acquire, acc=%#v guard=%#v err=%v", acc3, guard3, err3)
	}
	if !strings.Contains(err3.Error(), "2/2") {
		t.Fatalf("expected busy error to mention 2/2, got %v", err3)
	}
}

func TestAccountConcurrencyOverrideAppliesAcrossModels(t *testing.T) {
	initProtectionTestConfig(t, config.UpstreamProtectionConfig{
		Enabled:                       true,
		MaxPerAccountConcurrency:      10,
		MaxPerAccountModelConcurrency: 5,
		RateLimitCooldownMs:           10,
		MaxRateLimitCooldownMs:        100,
	})
	p := newTestPool(config.Account{ID: "a", Enabled: true, MaxConcurrency: 1})

	_, sonnetGuard, err := p.AcquireForModel("claude-sonnet-4.5", "", nil)
	if err != nil || sonnetGuard == nil {
		t.Fatalf("expected first model acquire, guard=%#v err=%v", sonnetGuard, err)
	}

	opus, opusGuard, opusErr := p.AcquireForModel("claude-opus-4.5", "", nil)
	if opusErr == nil || opus != nil || opusGuard != nil {
		t.Fatalf("expected account-wide limit across models, acc=%#v guard=%#v err=%v", opus, opusGuard, opusErr)
	}

	sonnetGuard.Release()
	opus, opusGuard, opusErr = p.AcquireForModel("claude-opus-4.5", "", nil)
	if opusErr != nil || opus == nil || opusGuard == nil {
		t.Fatalf("expected acquire after total slot release, acc=%#v guard=%#v err=%v", opus, opusGuard, opusErr)
	}
	opusGuard.Release()
}

func TestCircuitBreakerAllowsOnlyOneHalfOpenProbe(t *testing.T) {
	initProtectionTestConfig(t, config.UpstreamProtectionConfig{
		Enabled:                       true,
		MaxPerAccountConcurrency:      10,
		MaxPerAccountModelConcurrency: 5,
		RateLimitCooldownMs:           10,
		MaxRateLimitCooldownMs:        100,
	})
	p := newTestPool(config.Account{ID: "a", Enabled: true})
	model := "claude-sonnet-4.5"
	p.RecordUpstreamRateLimited("a", "", model)

	p.mu.Lock()
	state := p.upstream[upstreamStateKey{accountID: "a", model: model}]
	state.cooldownUntil = time.Now().Add(-time.Millisecond)
	p.upstream[upstreamStateKey{accountID: "a", model: model}] = state
	p.mu.Unlock()

	account, probe, err := p.AcquireForModel(model, "", nil)
	if err != nil || account == nil || probe == nil {
		t.Fatalf("expected one half-open probe, acc=%#v guard=%#v err=%v", account, probe, err)
	}

	second, secondGuard, secondErr := p.AcquireForModel(model, "", nil)
	if secondErr == nil || second != nil || secondGuard != nil || !strings.Contains(secondErr.Error(), "half-open") {
		t.Fatalf("expected second half-open request to be blocked, acc=%#v guard=%#v err=%v", second, secondGuard, secondErr)
	}

	p.RecordUpstreamSuccess("a", "", model)
	probe.Release()
	third, thirdGuard, thirdErr := p.AcquireForModel(model, "", nil)
	if thirdErr != nil || third == nil || thirdGuard == nil {
		t.Fatalf("expected closed circuit after successful probe, acc=%#v guard=%#v err=%v", third, thirdGuard, thirdErr)
	}
	thirdGuard.Release()
}

func TestFailedHalfOpenProbeReopensCircuit(t *testing.T) {
	initProtectionTestConfig(t, config.UpstreamProtectionConfig{
		Enabled:                       true,
		MaxPerAccountConcurrency:      10,
		MaxPerAccountModelConcurrency: 5,
		RateLimitCooldownMs:           20,
		MaxRateLimitCooldownMs:        100,
	})
	p := newTestPool(config.Account{ID: "a", Enabled: true})
	model := "claude-sonnet-4.5"
	p.RecordUpstreamRateLimited("a", "", model)

	key := upstreamStateKey{accountID: "a", model: model}
	p.mu.Lock()
	state := p.upstream[key]
	state.cooldownUntil = time.Now().Add(-time.Millisecond)
	p.upstream[key] = state
	p.mu.Unlock()

	_, probe, err := p.AcquireForModel(model, "", nil)
	if err != nil || probe == nil {
		t.Fatalf("expected half-open probe, guard=%#v err=%v", probe, err)
	}
	probe.Release()

	p.mu.RLock()
	state = p.upstream[key]
	p.mu.RUnlock()
	if state.rateLimitCount != 1 || !state.cooldownUntil.After(time.Now()) || state.halfOpenProbe {
		t.Fatalf("failed probe did not reopen circuit correctly: %+v", state)
	}
}

func TestAcquireForModelRespectsProfileConcurrency(t *testing.T) {
	profileArn := "arn:profile/shared"
	initProtectionTestConfig(t, config.UpstreamProtectionConfig{
		Enabled:                       true,
		MaxPerAccountModelConcurrency: 5,
		PerProfileModelConcurrency: map[string]map[string]int{
			profileArn: map[string]int{"claude-sonnet-4.5": 1},
		},
		RateLimitCooldownMs:    10,
		MaxRateLimitCooldownMs: 100,
	})
	p := newTestPool(
		config.Account{ID: "a", Enabled: true, ProfileArn: profileArn},
		config.Account{ID: "b", Enabled: true, ProfileArn: profileArn},
	)

	_, guard, err := p.AcquireForModel("claude-sonnet-4.5", "", nil)
	if err != nil || guard == nil {
		t.Fatalf("expected first profile acquire, guard=%#v err=%v", guard, err)
	}
	defer guard.Release()

	acc2, guard2, err2 := p.AcquireForModel("claude-sonnet-4.5", "", nil)
	if err2 == nil || acc2 != nil || guard2 != nil {
		t.Fatalf("expected profile busy, acc=%#v guard=%#v err=%v", acc2, guard2, err2)
	}
}

func TestRecordUpstreamRateLimitedSkipsCoolingAccountForModel(t *testing.T) {
	initProtectionTestConfig(t, config.UpstreamProtectionConfig{
		Enabled:                       true,
		MaxPerAccountModelConcurrency: 5,
		RateLimitCooldownMs:           1000,
		MaxRateLimitCooldownMs:        1000,
	})
	p := newTestPool(
		config.Account{ID: "a", Enabled: true},
		config.Account{ID: "b", Enabled: true},
	)

	p.RecordUpstreamRateLimited("a", "", "claude-sonnet-4.5")
	for i := 0; i < 3; i++ {
		acc, guard, err := p.AcquireForModel("claude-sonnet-4.5", "", nil)
		if err != nil {
			t.Fatalf("unexpected acquire error: %v", err)
		}
		if acc == nil || acc.ID != "b" {
			t.Fatalf("expected cooling account a to be skipped, got %#v", acc)
		}
		guard.Release()
	}
}

func TestRecordUpstreamRateLimitedHonorsRetryAfter(t *testing.T) {
	initProtectionTestConfig(t, config.UpstreamProtectionConfig{
		Enabled:                       true,
		MaxPerAccountConcurrency:      10,
		MaxPerAccountModelConcurrency: 5,
		RateLimitCooldownMs:           100,
		MaxRateLimitCooldownMs:        500,
	})
	p := newTestPool(config.Account{ID: "a", Enabled: true})

	cooldown := p.RecordUpstreamRateLimitedWithRetryAfter("a", "", "claude-sonnet-4.5", 3*time.Second)
	if cooldown < 3*time.Second {
		t.Fatalf("expected Retry-After to override local maximum, got %s", cooldown)
	}
}

func TestAcquireForModelRouteAffinityPrefersRememberedAccount(t *testing.T) {
	initProtectionTestConfig(t, config.UpstreamProtectionConfig{
		Enabled:                       true,
		MaxPerAccountModelConcurrency: 5,
		RateLimitCooldownMs:           10,
		MaxRateLimitCooldownMs:        100,
		RouteAffinityTTLSeconds:       3600,
		RouteAffinityMaxEntries:       20000,
	})
	p := newTestPool(
		config.Account{ID: "a", Enabled: true},
		config.Account{ID: "b", Enabled: true},
	)

	first, guard, err := p.AcquireForModel("claude-sonnet-4.5", "conversation-1", nil)
	if err != nil || first == nil || guard == nil {
		t.Fatalf("expected first acquire, first=%#v guard=%#v err=%v", first, guard, err)
	}
	guard.Release()

	second, guard2, err2 := p.AcquireForModel("claude-sonnet-4.5", "conversation-1", nil)
	if err2 != nil || second == nil || guard2 == nil {
		t.Fatalf("expected affinity acquire, second=%#v guard=%#v err=%v", second, guard2, err2)
	}
	defer guard2.Release()
	if second.ID != first.ID {
		t.Fatalf("expected route affinity to keep account %q, got %q", first.ID, second.ID)
	}
}

// ---------------------------------------------------------------------------
// Reload over-usage filtering
// ---------------------------------------------------------------------------

func TestReloadKeepsOverQuotaAccountWhenAllowOverUsage(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.AddAccount(config.Account{
		ID:           "over",
		Enabled:      true,
		UsageCurrent: 10,
		UsageLimit:   10,
	}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	if err := config.UpdateAllowOverUsage(true); err != nil {
		t.Fatalf("UpdateAllowOverUsage: %v", err)
	}

	p := newTestPool()
	p.Reload()

	if got := p.GetNext(); got == nil || got.ID != "over" {
		t.Fatalf("expected over-quota account to remain routable when allowOverUsage=true, got %#v", got)
	}
}

func TestReloadDropsOverQuotaAccountWhenAllowOverUsageDisabled(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.AddAccount(config.Account{
		ID:           "over",
		Enabled:      true,
		UsageCurrent: 10,
		UsageLimit:   10,
	}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}

	p := newTestPool()
	p.Reload()

	if got := p.GetNext(); got != nil {
		t.Fatalf("expected over-quota account to be dropped, got %q", got.ID)
	}
}

func TestModelNegativeCacheOnlyBlocksRejectedAccountModel(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	p := &AccountPool{
		accounts:        []config.Account{{ID: "a", Enabled: true}},
		totalAccounts:   1,
		cooldowns:       make(map[string]time.Time),
		errorCounts:     make(map[string]int),
		modelLists:      make(map[string]map[string]bool),
		upstream:        make(map[upstreamStateKey]upstreamRuntimeState),
		profiles:        make(map[profileStateKey]upstreamRuntimeState),
		affinity:        make(map[string]routeAffinityEntry),
		modelNegative:   make(map[modelAvailabilityKey]time.Time),
		lastSuccess:     make(map[string]time.Time),
		refreshFailures: make(map[string]time.Time),
	}
	p.RecordModelUnavailable("a", "claude-opus-4.6")
	if got := p.GetNextForModel("claude-opus-4.6"); got != nil {
		t.Fatalf("expected rejected account-model pair to be skipped, got %+v", got)
	}
	if got := p.GetNextForModel("claude-sonnet-4.6"); got == nil || got.ID != "a" {
		t.Fatalf("expected other models to remain routable, got %+v", got)
	}
	p.ClearModelUnavailable("a", "claude-opus-4.6")
	if got := p.GetNextForModel("claude-opus-4.6"); got == nil || got.ID != "a" {
		t.Fatalf("expected success clearing negative cache to restore routing, got %+v", got)
	}
}

func TestRuntimeStatePersistsCooldownModelsAndRefreshCursor(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	newRuntimePool := func() *AccountPool {
		return &AccountPool{
			accounts:        []config.Account{{ID: "a", Enabled: true}},
			totalAccounts:   1,
			cooldowns:       make(map[string]time.Time),
			errorCounts:     make(map[string]int),
			modelLists:      make(map[string]map[string]bool),
			upstream:        make(map[upstreamStateKey]upstreamRuntimeState),
			profiles:        make(map[profileStateKey]upstreamRuntimeState),
			affinity:        make(map[string]routeAffinityEntry),
			modelNegative:   make(map[modelAvailabilityKey]time.Time),
			lastSuccess:     make(map[string]time.Time),
			refreshFailures: make(map[string]time.Time),
		}
	}

	original := newRuntimePool()
	original.RecordError("a", true)
	original.SetModelList("a", []string{"claude-sonnet-4.6"})
	original.RecordModelUnavailable("a", "claude-opus-4.6")
	original.SetRefreshCursor(37)
	original.SetRefreshFailureCooldown("a", time.Now().Add(10*time.Minute))
	original.saveRuntimeState()

	restored := newRuntimePool()
	restored.loadRuntimeState()
	if restored.errorCounts["a"] != 1 || !restored.cooldowns["a"].After(time.Now()) {
		t.Fatalf("account cooldown was not restored: errors=%d cooldown=%v", restored.errorCounts["a"], restored.cooldowns["a"])
	}
	if restored.RefreshCursor() != 37 {
		t.Fatalf("expected refresh cursor 37, got %d", restored.RefreshCursor())
	}
	if !restored.modelLists["a"]["claude-sonnet-4.6"] {
		t.Fatalf("model list was not restored")
	}
	if restored.accountHasModel("a", "claude-opus-4.6") {
		t.Fatalf("model negative cache was not restored")
	}
	if restored.RefreshFailureCooldowns()["a"] <= time.Now().Unix() {
		t.Fatalf("refresh failure cooldown was not restored")
	}
}
