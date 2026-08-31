package pool

import (
	"context"
	"errors"
	"kiro-go/config"
	"path/filepath"
	"testing"
	"time"
)

func newProtectionQueueTestPool(t *testing.T, mode string, queueCapacity int) *AccountPool {
	t.Helper()
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	protection := config.GetUpstreamProtectionConfig()
	protection.Enabled = true
	protection.ConcurrencyMode = mode
	protection.MaxPerAccountConcurrency = 1
	protection.MaxPerAccountModelConcurrency = 1
	protection.SoftMaxPerAccountConcurrency = 1
	protection.SoftMaxPerAccountModelConcurrency = 1
	protection.QueueCapacity = queueCapacity
	if err := config.UpdateUpstreamProtectionConfig(protection); err != nil {
		t.Fatalf("UpdateUpstreamProtectionConfig: %v", err)
	}

	account := config.Account{ID: "queue-account", Enabled: true, AccessToken: "access-token", BanStatus: "ACTIVE"}
	return &AccountPool{
		accounts:           []config.Account{account},
		accountIndex:       map[string]int{account.ID: 0},
		cooldowns:          make(map[string]time.Time),
		cooldownKinds:      make(map[string]accountCooldownKind),
		errorCounts:        make(map[string]int),
		modelLists:         make(map[string]map[string]bool),
		accountUpstream:    make(map[string]upstreamRuntimeState),
		upstream:           make(map[upstreamStateKey]upstreamRuntimeState),
		profiles:           make(map[profileStateKey]upstreamRuntimeState),
		weightedCurrent:    make(map[string]int64),
		affinity:           make(map[string]routeAffinityEntry),
		modelNegative:      make(map[modelAvailabilityKey]time.Time),
		lastSuccess:        make(map[string]time.Time),
		healthStats:        make(map[string]accountHealthState),
		refreshFailures:    make(map[string]time.Time),
		availabilityNotify: make(chan struct{}),
	}
}

func waitForQueueCount(t *testing.T, p *AccountPool, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		waiting, _ := p.AvailabilitySnapshot()
		if waiting == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	waiting, _ := p.AvailabilitySnapshot()
	t.Fatalf("waiting count = %d, want %d", waiting, want)
}

func TestAdaptiveConcurrencyUsesSoftLimitsForLegacyLargeHardLimit(t *testing.T) {
	p := newProtectionQueueTestPool(t, "adaptive", 4)
	protection := config.GetUpstreamProtectionConfig()
	protection.MaxPerAccountConcurrency = 10000
	protection.MaxPerAccountModelConcurrency = 10000
	protection.SoftMaxPerAccountConcurrency = 16
	protection.SoftMaxPerAccountModelConcurrency = 5
	if err := config.UpdateUpstreamProtectionConfig(protection); err != nil {
		t.Fatalf("update protection: %v", err)
	}
	got := accountConcurrencyLimit(config.GetUpstreamProtectionConfig(), &p.accounts[0])
	if got != 16 {
		t.Fatalf("account soft limit = %d, want 16", got)
	}
	if got := accountModelLimit(config.GetUpstreamProtectionConfig(), "claude-opus-5"); got != 5 {
		t.Fatalf("model soft limit = %d, want 5", got)
	}
}

func TestHardConcurrencyModePreservesConfiguredLimit(t *testing.T) {
	p := newProtectionQueueTestPool(t, "hard", 4)
	protection := config.GetUpstreamProtectionConfig()
	protection.MaxPerAccountConcurrency = 10000
	protection.MaxPerAccountModelConcurrency = 10000
	protection.SoftMaxPerAccountConcurrency = 1
	protection.SoftMaxPerAccountModelConcurrency = 1
	if err := config.UpdateUpstreamProtectionConfig(protection); err != nil {
		t.Fatalf("update protection: %v", err)
	}
	if got := accountConcurrencyLimit(config.GetUpstreamProtectionConfig(), &p.accounts[0]); got != 10000 {
		t.Fatalf("hard account limit = %d, want 10000", got)
	}
}

func TestWaitForAvailabilityWakesWhenSlotIsReleased(t *testing.T) {
	p := newProtectionQueueTestPool(t, "hard", 1)
	_, guard, err := p.AcquireForModel("claude-opus-5", "", nil)
	if err != nil || guard == nil {
		t.Fatalf("initial acquire: guard=%v err=%v", guard, err)
	}

	result := make(chan bool, 1)
	go func() { result <- p.WaitForAvailability(context.Background(), 5*time.Second) }()
	waitForQueueCount(t, p, 1)
	guard.Release()
	select {
	case ok := <-result:
		if !ok {
			t.Fatal("waiter did not wake after slot release")
		}
	case <-time.After(time.Second):
		t.Fatal("waiter was not notified promptly")
	}
}

func TestWaitForAvailabilityBoundsQueueAndCancellation(t *testing.T) {
	p := newProtectionQueueTestPool(t, "hard", 1)
	_, guard, err := p.AcquireForModel("claude-opus-5", "", nil)
	if err != nil || guard == nil {
		t.Fatalf("initial acquire: guard=%v err=%v", guard, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan bool, 1)
	go func() { result <- p.WaitForAvailability(ctx, 5*time.Second) }()
	waitForQueueCount(t, p, 1)
	if ok, waitErr := p.WaitForAvailabilityWithStatus(context.Background(), 20*time.Millisecond); ok || !errors.Is(waitErr, ErrAvailabilityQueueFull) {
		t.Fatal("queue admitted a waiter beyond its capacity")
	}
	cancel()
	select {
	case ok := <-result:
		if ok {
			t.Fatal("canceled waiter returned success")
		}
	case <-time.After(time.Second):
		t.Fatal("canceled waiter did not stop")
	}
	guard.Release()
}

func TestWaitForAvailabilityReportsCancellationSeparately(t *testing.T) {
	p := newProtectionQueueTestPool(t, "hard", 1)
	_, guard, err := p.AcquireForModel("claude-opus-5", "", nil)
	if err != nil || guard == nil {
		t.Fatalf("initial acquire: guard=%v err=%v", guard, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, waitErr := p.WaitForAvailabilityWithStatus(ctx, 5*time.Second)
		result <- waitErr
	}()
	waitForQueueCount(t, p, 1)
	cancel()
	select {
	case waitErr := <-result:
		if !errors.Is(waitErr, context.Canceled) {
			t.Fatalf("wait error = %v, want context.Canceled", waitErr)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled status waiter did not stop")
	}
	guard.Release()
}

func TestWaitForAvailabilityRetriesWhenPoolIsEmpty(t *testing.T) {
	p := newProtectionQueueTestPool(t, "adaptive", 1)
	p.accounts = nil
	started := time.Now()
	if !p.WaitForAvailability(context.Background(), 20*time.Millisecond) {
		t.Fatal("empty pool did not allow another selection round")
	}
	if elapsed := time.Since(started); elapsed < 10*time.Millisecond || elapsed > 200*time.Millisecond {
		t.Fatalf("empty pool wait took %s", elapsed)
	}
}
