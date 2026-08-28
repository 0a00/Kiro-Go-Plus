package proxy

import (
	"sync"
	"time"
)

// activityWatchdog cancels work only after the configured period without an
// activity update. The timer is re-armed by its callback instead of being
// reset for every token, which keeps long, high-volume streams inexpensive.
type activityWatchdog struct {
	mu             sync.Mutex
	timeout        time.Duration
	onTimeout      func()
	lastActivityAt time.Time
	timer          *time.Timer
	generation     uint64
	stopped        bool
	fired          bool
}

func newActivityWatchdog(timeout time.Duration, onTimeout func()) *activityWatchdog {
	watchdog := &activityWatchdog{
		timeout:        timeout,
		onTimeout:      onTimeout,
		lastActivityAt: time.Now(),
	}
	if timeout > 0 {
		watchdog.armLocked()
	}
	return watchdog
}

// Touch records activity. The existing timer is deliberately left in place;
// when it wakes it checks the timestamp and sleeps only for the remaining idle
// interval. This avoids timer churn for every streamed fragment.
func (w *activityWatchdog) Touch() {
	if w == nil || w.timeout <= 0 {
		return
	}
	w.mu.Lock()
	if !w.stopped && !w.fired {
		w.lastActivityAt = time.Now()
	}
	w.mu.Unlock()
}

func (w *activityWatchdog) Stop() {
	if w == nil {
		return
	}
	w.mu.Lock()
	w.stopped = true
	if w.timer != nil {
		w.timer.Stop()
	}
	w.mu.Unlock()
}

func (w *activityWatchdog) armLocked() {
	if w == nil || w.timeout <= 0 || w.stopped || w.fired {
		return
	}
	if w.lastActivityAt.IsZero() {
		w.lastActivityAt = time.Now()
	}
	w.generation++
	generation := w.generation
	delay := w.timeout - time.Since(w.lastActivityAt)
	if delay < 0 {
		delay = 0
	}
	if w.timer != nil {
		w.timer.Stop()
	}
	w.timer = time.AfterFunc(delay, func() {
		w.fire(generation)
	})
}

func (w *activityWatchdog) fire(generation uint64) {
	w.mu.Lock()
	if w.stopped || w.fired || generation != w.generation {
		w.mu.Unlock()
		return
	}
	idleFor := time.Since(w.lastActivityAt)
	if idleFor < 0 {
		idleFor = 0
	}
	if idleFor < w.timeout {
		w.armLocked()
		w.mu.Unlock()
		return
	}
	w.fired = true
	onTimeout := w.onTimeout
	w.mu.Unlock()
	if onTimeout != nil {
		onTimeout()
	}
}
