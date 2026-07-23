package proxy

import (
	"context"
	"fmt"
	"kiro-go/auth"
	"kiro-go/config"
	"kiro-go/logger"
	accountpool "kiro-go/pool"
	"strings"
	"sync"
	"time"
)

type coordinatedRefreshResult struct {
	accessToken  string
	refreshToken string
	expiresAt    int64
	profileArn   string
	err          error
}

type coordinatedRefreshCall struct {
	done   chan struct{}
	result coordinatedRefreshResult
}

type tokenRefreshCoordinator struct {
	mu                   sync.Mutex
	inFlight             map[string]*coordinatedRefreshCall
	active               int
	waiting              int
	notify               chan struct{}
	upstreamBlockedUntil time.Time
}

type credentialPersistRequest struct {
	update config.AccountCredentialUpdate
	done   chan error
}

type credentialPersistenceBatcher struct {
	mu      sync.Mutex
	pending []credentialPersistRequest
}

var sharedCredentialPersistence = &credentialPersistenceBatcher{}

var sharedTokenRefreshCoordinator = &tokenRefreshCoordinator{
	inFlight: make(map[string]*coordinatedRefreshCall),
	notify:   make(chan struct{}),
}

const refreshUpstreamBlockCooldown = 30 * time.Second

func (c *tokenRefreshCoordinator) Refresh(account *config.Account, force bool) error {
	return c.RefreshContext(context.Background(), account, force)
}

func (c *tokenRefreshCoordinator) RefreshContext(ctx context.Context, account *config.Account, force bool) error {
	if account == nil {
		return fmt.Errorf("account is required for token refresh")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if isKiroAPIKeyAccount(account) {
		return nil
	}
	if strings.TrimSpace(account.RefreshToken) == "" {
		return fmt.Errorf("refresh token is missing")
	}

	if latest := accountpool.GetPool().GetByID(account.ID); latest != nil {
		copyRuntimeCredentialFields(account, latest)
	}
	if !force && tokenStillValid(account, config.GetAutoRefreshConfig().TokenRefreshBeforeSeconds) {
		return nil
	}
	if err := c.refreshUpstreamBlockError(); err != nil {
		return err
	}

	key := strings.TrimSpace(account.ID)
	if key == "" {
		key = "refresh:" + account.RefreshToken
	}

	c.mu.Lock()
	if call := c.inFlight[key]; call != nil {
		c.mu.Unlock()
		return c.wait(ctx, account, call)
	}
	autoRefresh := config.GetAutoRefreshConfig()
	if c.waiting >= autoRefresh.RefreshQueueCapacity {
		c.mu.Unlock()
		return fmt.Errorf("token refresh queue is full")
	}
	call := &coordinatedRefreshCall{done: make(chan struct{})}
	c.inFlight[key] = call
	c.waiting++
	c.mu.Unlock()

	accountCopy := *account
	go c.run(key, call, &accountCopy, autoRefresh)
	return c.wait(ctx, account, call)
}

func (c *tokenRefreshCoordinator) run(key string, call *coordinatedRefreshCall, account *config.Account, refreshConfig config.AutoRefreshConfig) {
	result := c.execute(account, refreshConfig)

	c.mu.Lock()
	call.result = result
	delete(c.inFlight, key)
	close(call.done)
	c.mu.Unlock()
}

func (c *tokenRefreshCoordinator) wait(ctx context.Context, account *config.Account, call *coordinatedRefreshCall) error {
	timeout := time.Duration(config.GetAutoRefreshConfig().RefreshTaskTimeoutSeconds) * time.Second
	if timeout < 10*time.Second {
		timeout = time.Minute
	}
	timer := time.NewTimer(timeout + time.Second)
	defer timer.Stop()
	select {
	case <-call.done:
		applyCoordinatedRefreshResult(account, call.result)
		return call.result.err
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return fmt.Errorf("timed out waiting for concurrent token refresh")
	}
}

func (c *tokenRefreshCoordinator) execute(account *config.Account, refreshConfig config.AutoRefreshConfig) coordinatedRefreshResult {
	if err := c.refreshUpstreamBlockError(); err != nil {
		c.dropQueuedRefresh()
		return coordinatedRefreshResult{err: err}
	}
	timeout := time.Duration(refreshConfig.RefreshTaskTimeoutSeconds) * time.Second
	if timeout < 10*time.Second {
		timeout = time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := c.acquireSlot(ctx); err != nil {
		return coordinatedRefreshResult{err: err}
	}
	defer c.releaseSlot()

	accessToken, refreshToken, expiresAt, profileArn, err := auth.RefreshTokenContext(ctx, account)
	if auth.IsRefreshUpstreamBlocked(err) {
		c.markRefreshUpstreamBlocked(refreshUpstreamBlockCooldown)
	}
	result := coordinatedRefreshResult{
		accessToken: accessToken, refreshToken: refreshToken,
		expiresAt: expiresAt, profileArn: profileArn, err: err,
	}
	if result.err != nil {
		if ctx.Err() != nil {
			result.err = fmt.Errorf("token refresh timed out after %s: %w", timeout, ctx.Err())
		}
		return result
	}
	if err := persistCoordinatedRefresh(account, result); err != nil {
		result.err = err
	}
	return result
}

func (c *tokenRefreshCoordinator) dropQueuedRefresh() {
	c.mu.Lock()
	if c.waiting > 0 {
		c.waiting--
	}
	c.mu.Unlock()
}

func (c *tokenRefreshCoordinator) refreshUpstreamBlockError() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	remaining := time.Until(c.upstreamBlockedUntil)
	if remaining <= 0 {
		c.upstreamBlockedUntil = time.Time{}
		return nil
	}
	return fmt.Errorf("%w; retry in %s", auth.ErrRefreshUpstreamBlocked, remaining.Round(time.Second))
}

func (c *tokenRefreshCoordinator) markRefreshUpstreamBlocked(duration time.Duration) {
	if duration <= 0 {
		duration = refreshUpstreamBlockCooldown
	}
	c.mu.Lock()
	until := time.Now().Add(duration)
	if until.After(c.upstreamBlockedUntil) {
		c.upstreamBlockedUntil = until
	}
	c.mu.Unlock()
}

func (c *tokenRefreshCoordinator) acquireSlot(ctx context.Context) error {
	for {
		limit := config.GetAutoRefreshConfig().RefreshConcurrency
		if limit < 1 {
			limit = 1
		}
		c.mu.Lock()
		if c.notify == nil {
			c.notify = make(chan struct{})
		}
		if c.active < limit {
			c.active++
			if c.waiting > 0 {
				c.waiting--
			}
			c.mu.Unlock()
			return nil
		}
		notify := c.notify
		c.mu.Unlock()
		select {
		case <-notify:
		case <-ctx.Done():
			c.mu.Lock()
			if c.waiting > 0 {
				c.waiting--
			}
			c.mu.Unlock()
			return fmt.Errorf("token refresh queue wait timed out: %w", ctx.Err())
		}
	}
}

func (c *tokenRefreshCoordinator) releaseSlot() {
	c.mu.Lock()
	if c.active > 0 {
		c.active--
	}
	c.signalWaitersLocked()
	c.mu.Unlock()
}

func (c *tokenRefreshCoordinator) signalWaitersLocked() {
	if c.notify == nil {
		c.notify = make(chan struct{})
		return
	}
	close(c.notify)
	c.notify = make(chan struct{})
}

func tokenStillValid(account *config.Account, refreshBeforeSeconds int64) bool {
	if account == nil || account.AccessToken == "" {
		return false
	}
	if account.ExpiresAt == 0 {
		return true
	}
	if refreshBeforeSeconds <= 0 {
		refreshBeforeSeconds = tokenRefreshSkewSeconds
	}
	return time.Now().Unix() < account.ExpiresAt-refreshBeforeSeconds
}

func copyRuntimeCredentialFields(dst, src *config.Account) {
	if dst == nil || src == nil {
		return
	}
	dst.AccessToken = src.AccessToken
	dst.RefreshToken = src.RefreshToken
	dst.ExpiresAt = src.ExpiresAt
	dst.ProfileArn = src.ProfileArn
}

func applyCoordinatedRefreshResult(account *config.Account, result coordinatedRefreshResult) {
	if account == nil || result.err != nil {
		return
	}
	account.AccessToken = result.accessToken
	if result.refreshToken != "" {
		account.RefreshToken = result.refreshToken
	}
	account.ExpiresAt = result.expiresAt
	if result.profileArn != "" {
		account.ProfileArn = result.profileArn
	}
}

func persistCoordinatedRefresh(account *config.Account, result coordinatedRefreshResult) error {
	if account == nil || result.err != nil {
		return result.err
	}
	update := config.AccountCredentialUpdate{
		ID: account.ID, AccessToken: result.accessToken, RefreshToken: result.refreshToken,
		ExpiresAt: result.expiresAt, ProfileArn: result.profileArn,
	}
	if err := sharedCredentialPersistence.Persist(update); err != nil {
		logger.Errorf("[TokenRefresh] Failed to persist refreshed token for %s: %v", account.Email, err)
		return fmt.Errorf("persist refreshed credentials: %w", err)
	}
	accountpool.GetPool().UpdateCredentials(account.ID, result.accessToken, result.refreshToken, result.expiresAt, result.profileArn)
	return nil
}

func (b *credentialPersistenceBatcher) Persist(update config.AccountCredentialUpdate) error {
	request := credentialPersistRequest{update: update, done: make(chan error, 1)}
	b.mu.Lock()
	b.pending = append(b.pending, request)
	if len(b.pending) == 1 {
		go b.flushSoon()
	}
	b.mu.Unlock()
	return <-request.done
}

func (b *credentialPersistenceBatcher) flushSoon() {
	time.Sleep(20 * time.Millisecond)
	b.mu.Lock()
	pending := b.pending
	b.pending = nil
	b.mu.Unlock()

	updates := make([]config.AccountCredentialUpdate, len(pending))
	for i := range pending {
		updates[i] = pending[i].update
	}
	err := config.UpdateAccountCredentialsBatch(updates)
	for i := range pending {
		pending[i].done <- err
		close(pending[i].done)
	}
}

func classifyRefreshFailure(endpoint string, err error) *UpstreamError {
	message := "token refresh failed"
	if err != nil {
		message += ": " + err.Error()
	}
	kind := UpstreamErrorTokenExpired
	lower := strings.ToLower(message)
	if auth.IsRefreshUpstreamBlocked(err) {
		kind = UpstreamErrorEndpointUnavailable
	} else if strings.Contains(lower, "invalid_grant") || strings.Contains(lower, "bad credentials") || strings.Contains(lower, "revoked") {
		kind = UpstreamErrorAuthRevoked
	}
	return &UpstreamError{
		Kind:                kind,
		Endpoint:            endpoint,
		Message:             message,
		Cause:               err,
		RetryAcrossAccounts: true,
	}
}
