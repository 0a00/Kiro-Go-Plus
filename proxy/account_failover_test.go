package proxy

import (
	"context"
	"errors"
	"kiro-go/config"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAccountFailureClassifiers(t *testing.T) {
	tests := []struct {
		name string
		fn   func(string) bool
		msg  string
	}{
		{name: "quota", fn: isQuotaErrorMessage, msg: "HTTP 429: quota exhausted"},
		{name: "overage", fn: isOverageErrorMessage, msg: "HTTP 402 from Kiro IDE: OVERAGE limit exceeded"},
		{name: "suspension", fn: isSuspensionErrorMessage, msg: "Your User ID temporarily is suspended"},
		{name: "profile", fn: isProfileUnavailableErrorMessage, msg: "no available Kiro profile"},
		{name: "auth", fn: isAuthErrorMessage, msg: "Authentication failed - token invalid or expired"},
	}

	for _, tc := range tests {
		if !tc.fn(tc.msg) {
			t.Fatalf("%s classifier did not match %q", tc.name, tc.msg)
		}
	}
}

func TestAccountAttemptControllerKeepsFiniteLimit(t *testing.T) {
	controller := newAccountAttemptController(context.Background(), nil, 3)
	for attempt := 0; attempt < 3; attempt++ {
		if !controller.next() {
			t.Fatalf("attempt %d was rejected before the configured limit", attempt+1)
		}
	}
	if controller.next() {
		t.Fatal("finite controller allowed an attempt beyond the configured limit")
	}
	controller.excluded["account-a"] = true
	if controller.nextRound(0) {
		t.Fatal("finite controller unexpectedly started another round")
	}
	if !controller.excluded["account-a"] {
		t.Fatal("finite controller cleared exclusions")
	}
}

func TestAccountAttemptControllerUnlimitedRoundsResetExclusionsWithBackoff(t *testing.T) {
	controller := newAccountAttemptController(context.Background(), nil, 0)
	var delays []time.Duration
	controller.wait = func(delay time.Duration) bool {
		delays = append(delays, delay)
		return true
	}

	wantDelays := []time.Duration{
		500 * time.Millisecond,
		time.Second,
		2 * time.Second,
		4 * time.Second,
		5 * time.Second,
		5 * time.Second,
	}
	for round, want := range wantDelays {
		controller.excluded["account-a"] = true
		if !controller.nextRound(0) {
			t.Fatalf("round %d was not started", round+1)
		}
		if len(controller.excluded) != 0 {
			t.Fatalf("round %d did not clear account exclusions", round+1)
		}
		if got := delays[round]; got != want {
			t.Fatalf("round %d delay = %s, want %s", round+1, got, want)
		}
	}

	if got := controller.roundDelay(10 * time.Millisecond); got != accountRetryMinimumDelay {
		t.Fatalf("short Retry-After delay = %s, want %s", got, accountRetryMinimumDelay)
	}
	if got := controller.roundDelay(time.Minute); got != accountRetryMaximumDelay {
		t.Fatalf("long Retry-After delay = %s, want %s", got, accountRetryMaximumDelay)
	}
}

func TestAccountAttemptControllerUnlimitedModeEventuallyContinues(t *testing.T) {
	controller := newAccountAttemptController(context.Background(), nil, 0)
	rounds := 0
	available := false
	controller.wait = func(time.Duration) bool {
		rounds++
		available = rounds == 2
		return true
	}

	found := false
	for controller.next() {
		if available {
			found = true
			break
		}
		controller.excluded["account-a"] = true
		if !controller.nextRound(0) {
			break
		}
	}
	if !found || rounds != 2 {
		t.Fatalf("expected availability after two polling rounds, found=%v rounds=%d", found, rounds)
	}
}

func TestAccountAttemptControllerCancellationInterruptsWait(t *testing.T) {
	requestCtx, cancel := context.WithCancel(context.Background())
	controller := newAccountAttemptController(requestCtx, nil, 0)
	done := make(chan bool, 1)
	go func() {
		done <- controller.nextRound(time.Minute)
	}()

	cancel()
	select {
	case continued := <-done:
		if continued {
			t.Fatal("controller continued after request cancellation")
		}
	case <-time.After(time.Second):
		t.Fatal("controller did not stop promptly after request cancellation")
	}
	if !errors.Is(controller.stopErr(), context.Canceled) {
		t.Fatalf("stop error = %v, want context.Canceled", controller.stopErr())
	}
}

func TestAccountAttemptControllerShutdownStopsNewAttempts(t *testing.T) {
	shutdownCtx, shutdown := context.WithCancel(context.Background())
	controller := newAccountAttemptController(context.Background(), shutdownCtx, 0)
	shutdown()
	if controller.next() {
		t.Fatal("controller started an attempt after shutdown")
	}
	if !errors.Is(controller.stopErr(), context.Canceled) {
		t.Fatalf("stop error = %v, want context.Canceled", controller.stopErr())
	}
}

func TestUnlimitedAccountPollingStillHonorsUpstreamAttemptCap(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	retry := config.GetRetryConfig()
	retry.MaxAccountAttempts = 0
	retry.MaxUpstreamAttempts = 1
	retry.MaxRetryDurationSeconds = 900
	if err := config.UpdateRetryConfig(retry); err != nil {
		t.Fatalf("UpdateRetryConfig: %v", err)
	}

	budget := newUpstreamAttemptBudget()
	if !budget.take() {
		t.Fatal("first upstream attempt was rejected")
	}
	if budget.take() {
		t.Fatal("unlimited account polling bypassed the upstream attempt cap")
	}
	budget.recordFailure("Kiro IDE", errors.New("incomplete tool JSON"))
	err := newRetryBudgetError(budget)
	if !strings.Contains(err.Error(), "after 1 attempts") || !strings.Contains(err.Error(), "budget exhausted") ||
		!strings.Contains(err.Error(), "last failure from Kiro IDE: incomplete tool JSON") {
		t.Fatalf("unexpected retry-budget error: %v", err)
	}
}

func TestFiniteAccountPollingStillHonorsUpstreamAttemptCap(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	retry := config.GetRetryConfig()
	retry.MaxAccountAttempts = 2
	retry.MaxUpstreamAttempts = 1
	if err := config.UpdateRetryConfig(retry); err != nil {
		t.Fatalf("UpdateRetryConfig: %v", err)
	}

	budget := newUpstreamAttemptBudget()
	if !budget.take() || budget.take() {
		t.Fatal("finite account polling did not enforce upstream attempt cap")
	}
}
