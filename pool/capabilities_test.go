package pool

import (
	"kiro-go/config"
	"path/filepath"
	"testing"
	"time"
)

func TestCapabilityRecordsAreNormalizedAndReturned(t *testing.T) {
	if err := configInitForCapabilities(t); err != nil {
		t.Fatal(err)
	}
	p := &AccountPool{}
	p.RecordCapability(" account-1 ", " Kiro IDE ", " Claude-Sonnet-4.5 ", CapabilityText, CapabilitySupported, "", 120*time.Millisecond)
	records := p.AccountCapabilities("account-1")
	if len(records) != 1 {
		t.Fatalf("records=%d, want 1", len(records))
	}
	got := records[0]
	if got.Endpoint != "kiro" || got.Model != "claude-sonnet-4.5" || got.Feature != CapabilityText || got.Status != CapabilitySupported {
		t.Fatalf("unexpected normalized record: %+v", got)
	}
	if got.LatencyMs != 120 || got.ExpiresAt <= got.CheckedAt {
		t.Fatalf("unexpected timestamps/latency: %+v", got)
	}
}

func TestCapabilityTransientObservationExpiresSooner(t *testing.T) {
	p := &AccountPool{}
	p.RecordCapability("account-1", "runtime", "model", CapabilityTools, CapabilityTransient, "temporary", 0)
	records := p.AccountCapabilities("account-1")
	if len(records) != 1 || records[0].ExpiresAt-records[0].CheckedAt > int64(CapabilityTransientTTL/time.Second) {
		t.Fatalf("transient capability did not use short TTL: %+v", records)
	}
}

func TestCapabilityRecordsAreBoundedPerAccount(t *testing.T) {
	p := &AccountPool{}
	for i := 0; i < maxCapabilitiesPerAccount+10; i++ {
		p.RecordCapability("account-1", "runtime", "model", "feature-"+itoaForCapabilities(i), CapabilitySupported, "", 0)
	}
	if got := len(p.AccountCapabilities("account-1")); got > maxCapabilitiesPerAccount {
		t.Fatalf("records=%d, exceeds per-account bound %d", got, maxCapabilitiesPerAccount)
	}
}

func configInitForCapabilities(t *testing.T) error {
	t.Helper()
	return config.Init(filepath.Join(t.TempDir(), "config.json"))
}

func itoaForCapabilities(value int) string {
	const digits = "0123456789"
	if value == 0 {
		return "0"
	}
	result := ""
	for value > 0 {
		result = string(digits[value%10]) + result
		value /= 10
	}
	return result
}
