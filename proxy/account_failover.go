package proxy

import (
	"context"
	"errors"
	"fmt"
	"kiro-go/config"
	"kiro-go/logger"
	accountpool "kiro-go/pool"
	"strings"
	"time"
)

const (
	accountRetryInitialDelay = 500 * time.Millisecond
	accountRetryMinimumDelay = 250 * time.Millisecond
	accountRetryMaximumDelay = 5 * time.Second
)

var errAccountSelectionTimeout = errors.New("account selection timed out")

// accountAttemptController keeps finite retry behavior unchanged while making
// unlimited polling bounded and cancellation-aware. Selection time is
// accumulated only while looking for an account, not during upstream calls.
type accountAttemptController struct {
	requestCtx         context.Context
	shutdownCtx        context.Context
	maxAttempts        int
	attempts           int
	rounds             int
	excluded           map[string]bool
	wait               func(time.Duration) bool
	selectionDeadline  time.Time
	selectionTimeout   time.Duration
	selectionStarted   time.Time
	selectionElapsed   time.Duration
	selectionActive    bool
	selectionTimedOut  bool
	queueWaitElapsed   time.Duration
	queueWaitCount     int
	queueWaitReported  time.Duration
	queueCountReported int
	waitQueueFull      bool
}

func newAccountAttemptController(requestCtx, shutdownCtx context.Context, maxAttempts int) *accountAttemptController {
	if requestCtx == nil {
		requestCtx = context.Background()
	}
	controller := &accountAttemptController{
		requestCtx:  requestCtx,
		shutdownCtx: shutdownCtx,
		maxAttempts: maxAttempts,
		excluded:    make(map[string]bool),
	}
	controller.wait = controller.waitForDelay
	return controller
}

func (h *Handler) newAccountAttemptController(requestCtx context.Context) *accountAttemptController {
	var shutdownCtx context.Context
	if h != nil {
		shutdownCtx = h.backgroundCtx
	}
	retry := config.GetRetryConfig()
	controller := newAccountAttemptController(requestCtx, shutdownCtx, retry.MaxAccountAttempts)
	controller.setSelectionTimeout(time.Duration(retry.AccountSelectionTimeoutSeconds) * time.Second)
	// Unlimited account rotation must wait on pool state changes rather than
	// waking every request on a fixed timer. The controller still owns the
	// overall selection deadline; the pool only supplies an efficient wake-up
	// when a slot is released or an account becomes routable.
	if h != nil && h.pool != nil {
		controller.wait = func(delay time.Duration) bool {
			if remaining := controller.selectionTimeRemaining(); remaining > 0 && remaining < delay {
				delay = remaining
			}
			startedAt := time.Now()
			ok, waitErr := h.pool.WaitForAvailabilityWithStatus(requestCtx, delay)
			controller.waitQueueFull = errors.Is(waitErr, accountpool.ErrAvailabilityQueueFull)
			controller.queueWaitElapsed += time.Since(startedAt)
			controller.queueWaitCount++
			if !ok {
				return false
			}
			// RecordError also broadcasts the availability generation. That wake-up
			// can happen while every account is still cooling, so never let it turn
			// unlimited failover into a hot loop. Keep the configured backoff as a
			// floor; a genuinely released slot is still picked up on the next scan.
			if elapsed := time.Since(startedAt); elapsed < delay {
				remaining := delay - elapsed
				if deadline := controller.selectionTimeRemaining(); deadline > 0 && deadline < remaining {
					remaining = deadline
				}
				if remaining > 0 && !controller.waitForDelay(remaining) {
					return false
				}
			}
			return controller.stopErr() == nil
		}
	}
	return controller
}

func (c *accountAttemptController) setSelectionTimeout(timeout time.Duration) {
	if c == nil || timeout <= 0 {
		return
	}
	c.selectionTimeout = timeout
	c.selectionDeadline = time.Time{}
	c.selectionStarted = time.Time{}
	c.selectionElapsed = 0
	c.selectionActive = false
	c.selectionTimedOut = false
}

func (c *accountAttemptController) beginSelection() bool {
	if c == nil || c.selectionTimeout <= 0 {
		return c != nil
	}
	if c.selectionTimedOut || c.selectionActive {
		return !c.selectionTimedOut
	}
	remaining := c.selectionTimeout - c.selectionElapsed
	if remaining <= 0 {
		c.selectionTimedOut = true
		return false
	}
	c.selectionStarted = time.Now()
	c.selectionDeadline = c.selectionStarted.Add(remaining)
	c.selectionActive = true
	return true
}

func (c *accountAttemptController) finishSelection() {
	if c == nil || !c.selectionActive {
		return
	}
	c.selectionElapsed += time.Since(c.selectionStarted)
	c.selectionStarted = time.Time{}
	c.selectionDeadline = time.Time{}
	c.selectionActive = false
}

func (c *accountAttemptController) next() bool {
	if c == nil || !c.beginSelection() || c.stopErr() != nil {
		return false
	}
	if c.maxAttempts > 0 && c.attempts >= c.maxAttempts {
		return false
	}
	c.attempts++
	return true
}

func (c *accountAttemptController) nextRound(retryAfter time.Duration) bool {
	if c == nil || c.maxAttempts != 0 || !c.beginSelection() || c.stopErr() != nil {
		return false
	}
	delay := c.roundDelay(retryAfter)
	if c.wait == nil || !c.wait(delay) {
		return false
	}
	clear(c.excluded)
	c.rounds++
	return true
}

func (c *accountAttemptController) waitQueueBusy(model string) *accountpool.UpstreamBusyError {
	if c == nil || !c.waitQueueFull {
		return nil
	}
	return &accountpool.UpstreamBusyError{
		Model:       model,
		RetryAfter:  time.Second,
		Description: "account availability wait queue is full",
	}
}

func (c *accountAttemptController) roundDelay(retryAfter time.Duration) time.Duration {
	if retryAfter > 0 {
		if retryAfter < accountRetryMinimumDelay {
			return accountRetryMinimumDelay
		}
		if retryAfter > accountRetryMaximumDelay {
			return accountRetryMaximumDelay
		}
		return retryAfter
	}

	delay := accountRetryInitialDelay
	for i := 0; i < c.rounds && delay < accountRetryMaximumDelay; i++ {
		delay *= 2
		if delay >= accountRetryMaximumDelay {
			return accountRetryMaximumDelay
		}
	}
	return delay
}

func (c *accountAttemptController) waitForDelay(delay time.Duration) bool {
	if remaining := c.selectionTimeRemaining(); remaining <= 0 {
		return false
	} else if remaining > 0 && remaining < delay {
		delay = remaining
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()

	var shutdownDone <-chan struct{}
	if c.shutdownCtx != nil {
		shutdownDone = c.shutdownCtx.Done()
	}
	select {
	case <-timer.C:
		return c.stopErr() == nil
	case <-c.requestCtx.Done():
		return false
	case <-shutdownDone:
		return false
	}
}

func (c *accountAttemptController) stopErr() error {
	if c == nil {
		return nil
	}
	if err := c.requestCtx.Err(); err != nil {
		return err
	}
	if c.shutdownCtx != nil {
		if err := c.shutdownCtx.Err(); err != nil {
			return err
		}
	}
	if c.selectionTimedOut {
		return fmt.Errorf("%w after %s", errAccountSelectionTimeout, c.selectionTimeout)
	}
	if !c.selectionActive || c.selectionDeadline.IsZero() {
		return nil
	}
	if !time.Now().Before(c.selectionDeadline) {
		c.selectionTimedOut = true
		return fmt.Errorf("%w after %s", errAccountSelectionTimeout, c.selectionTimeout)
	}
	return nil
}

func (c *accountAttemptController) selectionTimeRemaining() time.Duration {
	if c == nil || !c.selectionActive || c.selectionDeadline.IsZero() {
		return -1
	}
	remaining := time.Until(c.selectionDeadline)
	if remaining <= 0 {
		return 0
	}
	return remaining
}

func isAccountSelectionTimeout(err error) bool {
	return errors.Is(err, errAccountSelectionTimeout)
}

func upstreamBusyRetryAfter(err error) time.Duration {
	var busy *accountpool.UpstreamBusyError
	if errors.As(err, &busy) && busy != nil && busy.RetryAfter > 0 {
		return busy.RetryAfter
	}
	return time.Second
}

func (h *Handler) acquireNextAccountForRequest(controller *accountAttemptController, model, routeKey string, payloads ...*KiroPayload) (account *config.Account, guard *accountpool.UpstreamRequestGuard, busyResult *accountpool.UpstreamBusyError) {
	startedAt := time.Now()
	defer func() {
		if controller == nil {
			return
		}
		if len(payloads) == 0 || payloads[0] == nil {
			return
		}
		affinityHit := guard != nil && guard.AffinityHit()
		payloads[0].recordAccountSelection(time.Since(startedAt), controller.attempts, affinityHit)
		queueElapsed := controller.queueWaitElapsed - controller.queueWaitReported
		queueCount := controller.queueWaitCount - controller.queueCountReported
		controller.queueWaitReported = controller.queueWaitElapsed
		controller.queueCountReported = controller.queueWaitCount
		payloads[0].recordAccountQueueWait(queueElapsed, queueCount)
	}()
	if controller == nil || !controller.beginSelection() {
		return nil, nil, nil
	}
	defer controller.finishSelection()
	for controller.next() {
		account, guard, busy := h.acquireAccountForModel(model, routeKey, controller.excluded)
		if account != nil {
			return account, guard, nil
		}
		if busy != nil {
			if controller.nextRound(busy.RetryAfter) {
				continue
			}
			if queueBusy := controller.waitQueueBusy(model); queueBusy != nil {
				return nil, nil, queueBusy
			}
			return nil, nil, busy
		}
		if !controller.nextRound(0) {
			if queueBusy := controller.waitQueueBusy(model); queueBusy != nil {
				return nil, nil, queueBusy
			}
			return nil, nil, nil
		}
	}
	return nil, nil, nil
}

func isQuotaErrorMessage(msg string) bool {
	msg = strings.ToLower(msg)
	return accountpool.HasStatusToken(msg, "429") || strings.Contains(msg, "quota")
}

func isRateLimitErrorMessage(msg string) bool {
	msg = strings.ToLower(msg)
	return accountpool.HasStatusToken(msg, "429") ||
		strings.Contains(msg, "too many requests") ||
		strings.Contains(msg, "rate limit")
}

func isOverageErrorMessage(msg string) bool {
	msg = strings.ToLower(msg)
	return accountpool.HasStatusToken(msg, "402") && strings.Contains(msg, "overage")
}

func isSuspensionErrorMessage(msg string) bool {
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "temporarily_suspended") ||
		strings.Contains(msg, "temporarily is suspended") ||
		strings.Contains(msg, "account suspended")
}

func isProfileUnavailableErrorMessage(msg string) bool {
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "no available kiro profile")
}

func isAuthErrorMessage(msg string) bool {
	lower := strings.ToLower(msg)
	return accountpool.IsAuthFailure(errors.New(msg)) ||
		strings.Contains(lower, "authentication failed") ||
		strings.Contains(lower, "token invalid") ||
		strings.Contains(lower, "access token expired") ||
		strings.Contains(lower, "refresh token expired")
}

func (h *Handler) disableAccount(account *config.Account, banStatus, banReason string) {
	if account == nil {
		return
	}

	if !account.Enabled && account.BanStatus == banStatus && account.BanReason == banReason {
		return
	}

	enabled := false
	banTime := time.Now().Unix()
	updatedAccount, err := config.PatchAccountStatus(account.ID, config.AccountStatusPatch{
		Enabled: &enabled, BanStatus: &banStatus, BanReason: &banReason, BanTime: &banTime,
	})
	if err != nil {
		logger.Warnf("[AccountFailover] Failed to disable %s: %v", account.Email, err)
		return
	}
	account.Enabled = updatedAccount.Enabled
	account.BanStatus = updatedAccount.BanStatus
	account.BanReason = updatedAccount.BanReason
	account.BanTime = updatedAccount.BanTime

	logger.Warnf("[AccountFailover] Disabled %s: %s", account.Email, banReason)
	h.pool.Reload()
	if h.alerts != nil {
		h.alerts.Notify("account_disabled", map[string]interface{}{
			"accountId": account.ID, "email": account.Email, "banStatus": banStatus, "reason": banReason,
		})
	}
}

func (h *Handler) disableAccountOverage(account *config.Account) {
	if account == nil {
		return
	}

	snap, fetchErr := FetchOverageStatus(account)
	if fetchErr != nil {
		logger.Warnf("[AccountFailover] Failed to refresh overage status for %s: %v", account.Email, fetchErr)
		return
	}
	if persistErr := PersistOverageSnapshot(account.ID, snap); persistErr != nil {
		logger.Warnf("[AccountFailover] Failed to persist overage snapshot for %s: %v", account.Email, persistErr)
		return
	}

	logger.Warnf("[AccountFailover] Refreshed overage status for %s after upstream overage limit error: %s", account.Email, snap.Status)
	h.pool.Reload()
}

func (h *Handler) handleAccountFailure(account *config.Account, err error) {
	if account == nil || err == nil {
		return
	}

	if upstreamErr, ok := asUpstreamError(err); ok {
		switch upstreamErr.Kind {
		case UpstreamErrorClientRequest, UpstreamErrorRetryBudget, UpstreamErrorModelUnavailable, UpstreamErrorCanceled,
			UpstreamErrorLocalConfiguration:
			return
		case UpstreamErrorQuota:
			h.disableAccountOverage(account)
			h.pool.RecordError(account.ID, true)
			return
		case UpstreamErrorRateLimit:
			h.pool.RecordError(account.ID, false)
			return
		case UpstreamErrorSuspended:
			h.disableAccount(account, "BANNED", "AWS temporarily suspended - unusual user activity detected")
			return
		case UpstreamErrorAuthRevoked:
			h.disableAccount(account, "BANNED", "Authentication credentials were revoked")
			return
		case UpstreamErrorTokenExpired, UpstreamErrorForbidden:
			h.pool.RecordError(account.ID, false)
			return
		case UpstreamErrorTransient:
			// A transport failure tied to an account-specific proxy is local to
			// that route. Shared endpoint/global-proxy failures must not poison
			// every account during an upstream outage.
			if proxyURL := strings.TrimSpace(account.ProxyURL); proxyURL != "" && !strings.EqualFold(proxyURL, "direct") {
				h.pool.RecordError(account.ID, false)
			}
			return
		case UpstreamErrorEndpointUnavailable, UpstreamErrorFirstTokenTimeout,
			UpstreamErrorActionableTimeout, UpstreamErrorToolAssemblyTimeout, UpstreamErrorToolOutputTruncated, UpstreamErrorEmptyResponse,
			UpstreamErrorStreamTruncated:
			return
		}
	}

	errMsg := err.Error()
	switch {
	case isOverageErrorMessage(errMsg):
		h.disableAccountOverage(account)
		h.pool.RecordError(account.ID, false)
	case isQuotaErrorMessage(errMsg):
		h.pool.RecordError(account.ID, true)
	case isSuspensionErrorMessage(errMsg):
		h.disableAccount(account, "BANNED", "AWS temporarily suspended - unusual user activity detected")
	case isProfileUnavailableErrorMessage(errMsg):
		// Profile ARN may be transiently unresolvable (upstream blip, stale token).
		// Treat as a soft failure: short cooldown so the next request rotates account,
		// but never auto-disable — operators can still investigate via warn logs.
		h.pool.RecordError(account.ID, false)
	case isAuthErrorMessage(errMsg):
		h.disableAccount(account, "BANNED", "Authentication failed - token invalid or expired")
	default:
		h.pool.RecordError(account.ID, false)
	}
}

func (h *Handler) handleAccountFailureForModel(account *config.Account, model string, err error) {
	if account == nil || err == nil {
		return
	}
	if upstreamErr, ok := asUpstreamError(err); ok {
		switch upstreamErr.Kind {
		case UpstreamErrorModelUnavailable:
			until := h.pool.RecordModelUnavailable(account.ID, model)
			logger.Warnf("[ModelRouting] Account %s does not support %s; excluded until %s", account.ID, model, until.Format(time.RFC3339))
			return
		case UpstreamErrorRateLimit:
			cooldown := h.pool.RecordUpstreamRateLimitedWithRetryAfter(account.ID, account.ProfileArn, model, upstreamErr.RetryAfter)
			if cooldown > 0 {
				logger.Warnf("[UpstreamProtection] Account %s model %s cooling down for %s after 429", account.ID, model, cooldown)
			}
			return
		}
	}
	if isRateLimitErrorMessage(err.Error()) {
		cooldown := h.pool.RecordUpstreamRateLimited(account.ID, account.ProfileArn, model)
		if cooldown > 0 {
			logger.Warnf("[UpstreamProtection] Account %s model %s cooling down for %s after 429", account.ID, model, cooldown)
		}
		return
	}
	h.handleAccountFailure(account, err)
}

func (h *Handler) acquireAccountForModel(model, routeKey string, excluded map[string]bool) (*config.Account, *accountpool.UpstreamRequestGuard, *accountpool.UpstreamBusyError) {
	account, guard, err := h.pool.AcquireForModel(model, routeKey, excluded)
	if err == nil {
		return account, guard, nil
	}
	if busy, ok := err.(*accountpool.UpstreamBusyError); ok {
		return nil, nil, busy
	}
	return nil, nil, &accountpool.UpstreamBusyError{Model: model, RetryAfter: time.Second, Description: err.Error()}
}

func (h *Handler) callKiroAPIWithHealth(account *config.Account, payload *KiroPayload, callback *KiroStreamCallback) error {
	startedAt := time.Now()
	err := CallKiroAPI(account, payload, callback)
	if h != nil && h.pool != nil && account != nil && !isLocalConfigurationError(err) && !isStreamIntegrityError(err) {
		h.pool.RecordAccountOutcome(account.ID, time.Since(startedAt), err == nil)
	}
	return err
}
