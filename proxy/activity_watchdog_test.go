package proxy

import (
	"testing"
	"time"
)

func TestActivityWatchdogWaitsWhileEventsKeepArriving(t *testing.T) {
	fired := make(chan struct{}, 1)
	const timeout = 200 * time.Millisecond
	watchdog := newActivityWatchdog(timeout, func() {
		fired <- struct{}{}
	})
	defer watchdog.Stop()

	for i := 0; i < 5; i++ {
		time.Sleep(30 * time.Millisecond)
		watchdog.Touch()
		select {
		case <-fired:
			t.Fatal("watchdog fired while activity was still arriving")
		default:
		}
	}

	select {
	case <-fired:
	case <-time.After(350 * time.Millisecond):
		t.Fatal("watchdog did not fire after activity stopped")
	}
}

func TestActivityWatchdogStopPreventsCallback(t *testing.T) {
	fired := make(chan struct{}, 1)
	watchdog := newActivityWatchdog(40*time.Millisecond, func() {
		fired <- struct{}{}
	})
	watchdog.Stop()

	select {
	case <-fired:
		t.Fatal("stopped watchdog fired")
	case <-time.After(100 * time.Millisecond):
	}
}
