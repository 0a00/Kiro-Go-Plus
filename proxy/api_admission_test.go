package proxy

import (
	"context"
	"kiro-go/config"
	"net/http"
	"testing"
	"time"
)

func newAdmissionManager() *apiKeyAdmissionManager {
	return &apiKeyAdmissionManager{states: make(map[string]*apiKeyAdmissionState), now: time.Now}
}

func TestAPIKeyAdmissionEnforcesConcurrencyAndReleases(t *testing.T) {
	manager := newAdmissionManager()
	entry := &config.ApiKeyEntry{ID: "key", MaxConcurrency: 1}
	first, err := manager.Acquire(context.Background(), entry)
	if err != nil || first == nil {
		t.Fatalf("first acquire failed: lease=%v err=%v", first, err)
	}
	if second, err := manager.Acquire(context.Background(), entry); second != nil || err == nil || err.status != http.StatusTooManyRequests {
		t.Fatalf("expected concurrency rejection, lease=%v err=%v", second, err)
	}
	first.Release()
	third, err := manager.Acquire(context.Background(), entry)
	if err != nil || third == nil {
		t.Fatalf("expected acquire after release, lease=%v err=%v", third, err)
	}
	third.Release()
}

func TestAPIKeyAdmissionQueueWakesAfterRelease(t *testing.T) {
	manager := newAdmissionManager()
	entry := &config.ApiKeyEntry{ID: "key", MaxConcurrency: 1, QueueCapacity: 1, QueueTimeoutMs: 1000}
	first, err := manager.Acquire(context.Background(), entry)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	type result struct {
		lease *apiKeyAdmissionLease
		err   *authError
	}
	resultCh := make(chan result, 1)
	go func() {
		lease, acquireErr := manager.Acquire(context.Background(), entry)
		resultCh <- result{lease: lease, err: acquireErr}
	}()

	deadline := time.Now().Add(time.Second)
	for {
		manager.mu.Lock()
		waiting := 0
		if state := manager.states[entry.ID]; state != nil {
			waiting = state.waiting
		}
		manager.mu.Unlock()
		if waiting == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("queued request did not start waiting")
		}
		time.Sleep(time.Millisecond)
	}
	first.Release()
	queued := <-resultCh
	if queued.err != nil || queued.lease == nil {
		t.Fatalf("queued acquire failed: %+v", queued)
	}
	queued.lease.Release()
}

func TestAPIKeyAdmissionEnforcesRPMAndTPM(t *testing.T) {
	manager := newAdmissionManager()
	entry := &config.ApiKeyEntry{ID: "key", RequestsPerMinute: 1, TokensPerMinute: 100}
	first, err := manager.Acquire(context.Background(), entry)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if reserveErr := first.ReserveTokens(60); reserveErr != nil {
		t.Fatalf("first token reservation: %v", reserveErr)
	}
	if reserveErr := first.ReserveTokens(41); reserveErr == nil || reserveErr.status != http.StatusTooManyRequests {
		t.Fatalf("expected TPM rejection, got %v", reserveErr)
	}
	first.Release()
	if second, err := manager.Acquire(context.Background(), entry); second != nil || err == nil || err.status != http.StatusTooManyRequests || err.retryAfter <= 0 {
		t.Fatalf("expected RPM rejection with retry-after, lease=%v err=%v", second, err)
	}
}

func TestAPIKeyAdmissionReconcilesEstimatedTokens(t *testing.T) {
	manager := newAdmissionManager()
	entry := &config.ApiKeyEntry{ID: "key", TokensPerMinute: 100}
	first, err := manager.Acquire(context.Background(), entry)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if reserveErr := first.ReserveTokens(60); reserveErr != nil {
		t.Fatalf("reserve estimate: %v", reserveErr)
	}
	first.ReconcileTokens(40)
	first.Release()

	second, err := manager.Acquire(context.Background(), entry)
	if err != nil {
		t.Fatalf("second acquire: %v", err)
	}
	if reserveErr := second.ReserveTokens(60); reserveErr != nil {
		t.Fatalf("expected reconciled budget to allow 60 tokens: %v", reserveErr)
	}
	second.ReconcileTokens(80)
	second.Release()

	third, err := manager.Acquire(context.Background(), entry)
	if err != nil {
		t.Fatalf("third acquire: %v", err)
	}
	if reserveErr := third.ReserveTokens(1); reserveErr == nil {
		t.Fatal("expected actual usage above TPM to backpressure the next request")
	}
	third.Release()
}
