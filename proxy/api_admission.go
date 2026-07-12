package proxy

import (
	"context"
	"kiro-go/config"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

type apiKeyAdmissionState struct {
	windowStarted time.Time
	requests      int
	tokens        int64
	inFlight      int
	waiting       int
	notify        chan struct{}
	lastSeen      time.Time
}

type apiKeyAdmissionManager struct {
	mu     sync.Mutex
	states map[string]*apiKeyAdmissionState
	now    func() time.Time
}

type apiKeyAdmissionLease struct {
	manager           *apiKeyAdmissionManager
	keyID             string
	tokensPerMinute   int64
	reservedTokens    int64
	reservationWindow time.Time
	tokensFinalized   bool
	released          atomic.Bool
}

type apiKeyAdmissionContextKey struct{}

var sharedAPIKeyAdmission = &apiKeyAdmissionManager{states: make(map[string]*apiKeyAdmissionState), now: time.Now}

func (m *apiKeyAdmissionManager) Acquire(ctx context.Context, entry *config.ApiKeyEntry) (*apiKeyAdmissionLease, *authError) {
	if entry == nil || entry.ID == "" {
		return nil, nil
	}
	if entry.RequestsPerMinute <= 0 && entry.TokensPerMinute <= 0 && entry.MaxConcurrency <= 0 {
		return nil, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	queueTimeout := time.Duration(entry.QueueTimeoutMs) * time.Millisecond
	if entry.QueueCapacity > 0 && queueTimeout <= 0 {
		queueTimeout = 5 * time.Second
	}
	var deadline time.Time
	if queueTimeout > 0 {
		deadline = m.now().Add(queueTimeout)
	}
	queued := false

	for {
		m.mu.Lock()
		state := m.stateLocked(entry.ID)
		now := m.now()
		resetAdmissionWindow(state, now)
		state.lastSeen = now

		if entry.RequestsPerMinute > 0 && state.requests >= entry.RequestsPerMinute {
			if queued && state.waiting > 0 {
				state.waiting--
			}
			retryAfter := time.Minute - now.Sub(state.windowStarted)
			m.mu.Unlock()
			return nil, admissionLimitError("requests per minute limit exceeded", retryAfter)
		}
		if entry.MaxConcurrency <= 0 || state.inFlight < entry.MaxConcurrency {
			if queued && state.waiting > 0 {
				state.waiting--
			}
			state.inFlight++
			state.requests++
			m.mu.Unlock()
			return &apiKeyAdmissionLease{manager: m, keyID: entry.ID, tokensPerMinute: entry.TokensPerMinute}, nil
		}
		if entry.QueueCapacity <= 0 || (!queued && state.waiting >= entry.QueueCapacity) {
			m.mu.Unlock()
			return nil, admissionLimitError("API key concurrency limit exceeded", time.Second)
		}
		if !queued {
			state.waiting++
			queued = true
		}
		notify := state.notify
		m.mu.Unlock()

		remaining := time.Until(deadline)
		if deadline.IsZero() || remaining <= 0 {
			m.cancelWait(entry.ID, queued)
			return nil, admissionLimitError("API key request queue timed out", time.Second)
		}
		timer := time.NewTimer(remaining)
		select {
		case <-notify:
			if !timer.Stop() {
				<-timer.C
			}
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			m.cancelWait(entry.ID, queued)
			return nil, &authError{status: 499, code: "request_canceled", message: "request canceled while waiting for API key concurrency"}
		case <-timer.C:
			m.cancelWait(entry.ID, queued)
			return nil, admissionLimitError("API key request queue timed out", time.Second)
		}
	}
}

func (m *apiKeyAdmissionManager) stateLocked(keyID string) *apiKeyAdmissionState {
	state := m.states[keyID]
	if state == nil {
		state = &apiKeyAdmissionState{windowStarted: m.now(), notify: make(chan struct{})}
		m.states[keyID] = state
	}
	if state.notify == nil {
		state.notify = make(chan struct{})
	}
	return state
}

func resetAdmissionWindow(state *apiKeyAdmissionState, now time.Time) {
	if state.windowStarted.IsZero() || now.Sub(state.windowStarted) >= time.Minute {
		state.windowStarted = now
		state.requests = 0
		state.tokens = 0
	}
}

func (m *apiKeyAdmissionManager) cancelWait(keyID string, queued bool) {
	if !queued {
		return
	}
	m.mu.Lock()
	if state := m.states[keyID]; state != nil && state.waiting > 0 {
		state.waiting--
	}
	m.mu.Unlock()
}

func (l *apiKeyAdmissionLease) ReserveTokens(tokens int64) *authError {
	if l == nil || l.manager == nil || tokens <= 0 || l.tokensPerMinute <= 0 {
		return nil
	}
	m := l.manager
	m.mu.Lock()
	defer m.mu.Unlock()
	state := m.stateLocked(l.keyID)
	now := m.now()
	resetAdmissionWindow(state, now)
	if state.tokens+tokens > l.tokensPerMinute {
		retryAfter := time.Minute - now.Sub(state.windowStarted)
		return admissionLimitError("tokens per minute limit exceeded", retryAfter)
	}
	state.tokens += tokens
	if l.reservationWindow.IsZero() || !l.reservationWindow.Equal(state.windowStarted) {
		l.reservationWindow = state.windowStarted
		l.reservedTokens = 0
	}
	l.reservedTokens += tokens
	return nil
}

func (l *apiKeyAdmissionLease) ReconcileTokens(actualTokens int64) {
	if l == nil || l.manager == nil || l.tokensPerMinute <= 0 || actualTokens < 0 {
		return
	}
	m := l.manager
	m.mu.Lock()
	defer m.mu.Unlock()
	if l.tokensFinalized {
		return
	}
	state := m.stateLocked(l.keyID)
	now := m.now()
	resetAdmissionWindow(state, now)
	delta := actualTokens
	if !l.reservationWindow.IsZero() && l.reservationWindow.Equal(state.windowStarted) {
		delta -= l.reservedTokens
	}
	state.tokens += delta
	if state.tokens < 0 {
		state.tokens = 0
	}
	l.tokensFinalized = true
}

func (l *apiKeyAdmissionLease) Release() {
	if l == nil || l.manager == nil || l.released.Swap(true) {
		return
	}
	m := l.manager
	m.mu.Lock()
	if state := m.states[l.keyID]; state != nil {
		if state.inFlight > 0 {
			state.inFlight--
		}
		close(state.notify)
		state.notify = make(chan struct{})
		state.lastSeen = m.now()
	}
	m.mu.Unlock()
}

func (m *apiKeyAdmissionManager) AddTokens(keyID string, tokens int64) {
	if m == nil || keyID == "" || tokens <= 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	state := m.states[keyID]
	if state == nil {
		return
	}
	resetAdmissionWindow(state, m.now())
	state.tokens += tokens
}

func admissionLimitError(message string, retryAfter time.Duration) *authError {
	if retryAfter < time.Second {
		retryAfter = time.Second
	}
	return &authError{status: http.StatusTooManyRequests, code: "rate_limit_error", message: message, retryAfter: retryAfter}
}

func withAPIKeyAdmission(r *http.Request, lease *apiKeyAdmissionLease) *http.Request {
	if lease == nil {
		return r
	}
	return r.WithContext(context.WithValue(r.Context(), apiKeyAdmissionContextKey{}, lease))
}

func apiKeyAdmissionFromContext(ctx context.Context) *apiKeyAdmissionLease {
	if ctx == nil {
		return nil
	}
	lease, _ := ctx.Value(apiKeyAdmissionContextKey{}).(*apiKeyAdmissionLease)
	return lease
}

func releaseAPIKeyAdmission(ctx context.Context) {
	if lease := apiKeyAdmissionFromContext(ctx); lease != nil {
		lease.Release()
	}
}

func reserveAPIKeyTokens(ctx context.Context, tokens int) *authError {
	if lease := apiKeyAdmissionFromContext(ctx); lease != nil {
		return lease.ReserveTokens(int64(tokens))
	}
	return nil
}

func reconcileAPIKeyTokens(ctx context.Context, tokens int) {
	if lease := apiKeyAdmissionFromContext(ctx); lease != nil {
		lease.ReconcileTokens(int64(tokens))
	}
}

func applyAuthErrorHeaders(w http.ResponseWriter, err *authError) {
	if err == nil || err.retryAfter <= 0 {
		return
	}
	seconds := int64((err.retryAfter + time.Second - 1) / time.Second)
	w.Header().Set("Retry-After", strconv.FormatInt(seconds, 10))
}
