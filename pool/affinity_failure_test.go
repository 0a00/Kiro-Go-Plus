package pool

import (
	"kiro-go/config"
	"testing"
)

func TestForgetAffinityIsScopedAndDoesNotReleaseSlot(t *testing.T) {
	initProtectionTestConfig(t, config.UpstreamProtectionConfig{Enabled: true, MaxPerAccountModelConcurrency: 5, RouteAffinityTTLSeconds: 3600, RouteAffinityMaxEntries: 20000})
	p := newTestPool(config.Account{ID: "a", Enabled: true}, config.Account{ID: "b", Enabled: true})
	_, guard, err := p.AcquireForModel("claude-sonnet-4.5", "failed-session", nil)
	if err != nil || guard == nil {
		t.Fatalf("acquire: %v", err)
	}
	defer guard.Release()
	_, other, err := p.AcquireForModel("claude-sonnet-4.5", "other-session", nil)
	if err != nil || other == nil {
		t.Fatalf("other acquire: %v", err)
	}
	defer other.Release()
	guard.ForgetAffinity()
	p.mu.Lock()
	_, failedExists := p.affinity["failed-session"]
	_, otherExists := p.affinity["other-session"]
	p.mu.Unlock()
	if failedExists || !otherExists || guard.released.Load() {
		t.Fatal("forgetting affinity affected another session or released the slot")
	}
	guard.ForgetAffinity()
	var nilGuard *UpstreamRequestGuard
	nilGuard.ForgetAffinity()
	_, next, err := p.AcquireForModel("claude-sonnet-4.5", "failed-session", nil)
	if err != nil || next == nil {
		t.Fatalf("next acquire: %v", err)
	}
	defer next.Release()
	if next.AffinityHit() {
		t.Fatal("failed session retained its binding")
	}
}

func TestDelayedFailureCannotEraseNewerSameAccountAffinity(t *testing.T) {
	initProtectionTestConfig(t, config.UpstreamProtectionConfig{Enabled: true, MaxPerAccountModelConcurrency: 5, RouteAffinityTTLSeconds: 3600, RouteAffinityMaxEntries: 20000})
	p := newTestPool(config.Account{ID: "a", Enabled: true})
	_, first, err := p.AcquireForModel("claude-sonnet-4.5", "session", nil)
	if err != nil || first == nil {
		t.Fatal(err)
	}
	defer first.Release()
	_, newer, err := p.AcquireForModel("claude-sonnet-4.5", "session", nil)
	if err != nil || newer == nil {
		t.Fatal(err)
	}
	defer newer.Release()
	if first.routeSeen.Equal(newer.routeSeen) {
		t.Fatal("fixture requires distinct acquisitions")
	}
	first.ForgetAffinity()
	p.mu.Lock()
	entry, ok := p.affinity["session"]
	p.mu.Unlock()
	if !ok || !entry.lastSeen.Equal(newer.routeSeen) {
		t.Fatal("late failure erased newer binding")
	}
	newer.ForgetAffinity()
	p.mu.Lock()
	_, ok = p.affinity["session"]
	p.mu.Unlock()
	if ok {
		t.Fatal("current failing binding was not removed")
	}
}
