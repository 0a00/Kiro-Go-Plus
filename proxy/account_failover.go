package proxy

import (
	"context"
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

// accountAttemptController keeps finite retry behavior unchanged while making
// maxAccountAttempts=0 a context-aware, paced polling mode.
type accountAttemptController struct {
	requestCtx  context.Context
	shutdownCtx context.Context
	maxAttempts int
	attempts    int
	rounds      int
	excluded    map[string]bool
	wait        func(time.Duration) bool
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
	return newAccountAttemptController(requestCtx, shutdownCtx, config.GetRetryConfig().MaxAccountAttempts)
}

func (c *accountAttemptController) next() bool {
	if c == nil || c.stopErr() != nil {
		return false
	}
	if c.maxAttempts > 0 && c.attempts >= c.maxAttempts {
		return false
	}
	c.attempts++
	return true
}

func (c *accountAttemptController) nextRound(retryAfter time.Duration) bool {
	if c == nil || c.maxAttempts != 0 || c.stopErr() != nil {
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
	timer := time.NewTimer(delay)
	defer timer.Stop()

	var shutdownDone <-chan struct{}
	if c.shutdownCtx != nil {
		shutdownDone = c.shutdownCtx.Done()
	}
	select {
	case <-timer.C:
		return true
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
	return nil
}

func (h *Handler) acquireNextAccountForRequest(controller *accountAttemptController, model, routeKey string, payloads ...*KiroPayload) (account *config.Account, guard *accountpool.UpstreamRequestGuard, busyResult *accountpool.UpstreamBusyError) {
	startedAt := time.Now()
	defer func() {
		if len(payloads) == 0 || payloads[0] == nil {
			return
		}
		affinityHit := guard != nil && guard.AffinityHit()
		payloads[0].recordAccountSelection(time.Since(startedAt), controller.attempts, affinityHit)
	}()
	for controller.next() {
		account, guard, busy := h.acquireAccountForModel(model, routeKey, controller.excluded)
		if account != nil {
			return account, guard, nil
		}
		if busy != nil {
			if controller.nextRound(busy.RetryAfter) {
				continue
			}
			return nil, nil, busy
		}
		if !controller.nextRound(0) {
			return nil, nil, nil
		}
	}
	return nil, nil, nil
}

func isQuotaErrorMessage(msg string) bool {
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "429") || strings.Contains(msg, "quota")
}

func isRateLimitErrorMessage(msg string) bool {
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "http 429") ||
		strings.Contains(msg, " 429 ") ||
		strings.Contains(msg, "too many requests") ||
		strings.Contains(msg, "rate limit")
}

func isOverageErrorMessage(msg string) bool {
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "402") && strings.Contains(msg, "overage")
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
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "authentication failed") ||
		strings.Contains(msg, "token invalid") ||
		strings.Contains(msg, "token expired") ||
		strings.Contains(msg, "invalid_grant") ||
		strings.Contains(msg, "access token expired") ||
		strings.Contains(msg, "refresh token expired")
}

func (h *Handler) disableAccount(account *config.Account, banStatus, banReason string) {
	if account == nil {
		return
	}

	updatedAccount := *account
	if !updatedAccount.Enabled && updatedAccount.BanStatus == banStatus && updatedAccount.BanReason == banReason {
		return
	}

	updatedAccount.Enabled = false
	updatedAccount.BanStatus = banStatus
	updatedAccount.BanReason = banReason
	updatedAccount.BanTime = time.Now().Unix()

	if err := config.UpdateAccount(account.ID, updatedAccount); err != nil {
		logger.Warnf("[AccountFailover] Failed to disable %s: %v", account.Email, err)
		return
	}

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
		case UpstreamErrorClientRequest, UpstreamErrorRetryBudget, UpstreamErrorModelUnavailable, UpstreamErrorCanceled:
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
			UpstreamErrorActionableTimeout, UpstreamErrorToolAssemblyTimeout, UpstreamErrorToolOutputTruncated, UpstreamErrorEmptyResponse:
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
	if h != nil && h.pool != nil && account != nil {
		h.pool.RecordAccountOutcome(account.ID, time.Since(startedAt), err == nil)
	}
	return err
}
