package proxy

import (
	"context"
	"errors"
	"kiro-go/config"
	"kiro-go/logger"
)

// maxSameAccountStreamRetries bounds recovery of a transport-clean upstream
// stream that ended before its terminal stop reason. Endpoint retries already
// happen inside CallKiroAPI; this small, separate budget handles the case where
// the endpoint pass itself returned a truncated stream. Keeping it bounded is
// important because account failover may still run after this helper returns.
const maxSameAccountStreamRetries = 2

// runKiroWithIntegrityRetry retries only soft stream-integrity failures on the
// same account. call must contain the complete account/endpoint invocation and
// reset must clear all per-attempt accumulators before a retry. A non-nil
// canRetry is the downstream output boundary: once the client has received a
// semantic event, replaying the request would duplicate content and is unsafe.
// A nil canRetry means the caller is fully buffered and can always retry.
func runKiroWithIntegrityRetry(
	ctx context.Context,
	account *config.Account,
	payload *KiroPayload,
	call func() error,
	reset func(),
	canRetry func() bool,
) error {
	if call == nil {
		return errors.New("stream integrity retry called without request function")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if payload != nil {
		// Never leak this internal probe mode into a later, unrelated account
		// attempt, including when the request exits through an error path.
		payload.allowCoolingEndpointRetry = false
		defer func() { payload.allowCoolingEndpointRetry = false }()
	}

	for retry := 0; ; retry++ {
		if err := ctx.Err(); err != nil {
			return err
		}

		if payload != nil {
			payload.allowCoolingEndpointRetry = retry > 0
		}
		err := call()
		if payload != nil {
			payload.allowCoolingEndpointRetry = false
		}
		if err == nil || !isStreamIntegrityError(err) {
			return err
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}

		if canRetry != nil && !canRetry() {
			logger.Warnf("[StreamIntegrity] %v after client output for %s; replay suppressed", err, accountEmailForLog(account))
			return err
		}
		if retry >= maxSameAccountStreamRetries {
			logger.Warnf("[StreamIntegrity] giving up after %d same-account retries for %s: %v", retry, accountEmailForLog(account), err)
			return err
		}
		if !upstreamIntegrityRetryBudgetAvailable(payload) {
			// Keep the request-level cap authoritative. Returning the existing
			// integrity error preserves its soft-failure semantics (no account
			// penalty); the next account-selection layer will see the same shared
			// budget and stop without issuing another upstream request.
			logger.Warnf("[StreamIntegrity] upstream attempt budget exhausted before retry for %s: %v", accountEmailForLog(account), err)
			return err
		}

		if reset != nil {
			reset()
		}
		logger.Warnf("[StreamIntegrity] retrying same account %s after truncated stream (%d/%d)",
			accountEmailForLog(account), retry+1, maxSameAccountStreamRetries)
	}
}

func upstreamIntegrityRetryBudgetAvailable(payload *KiroPayload) bool {
	if payload == nil || payload.attemptBudget == nil {
		return true
	}
	snapshot := payload.attemptBudget.snapshot()
	if snapshot.MaxAttempts > 0 && snapshot.Attempts >= snapshot.MaxAttempts {
		return false
	}
	if snapshot.MaxDuration > 0 && snapshot.Elapsed >= snapshot.MaxDuration {
		return false
	}
	return true
}
