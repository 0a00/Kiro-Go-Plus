package proxy

import (
	"encoding/json"
	"kiro-go/config"
	"kiro-go/logger"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
)

func (h *Handler) apiGetModelFallback(w http.ResponseWriter, _ *http.Request) {
	_ = json.NewEncoder(w).Encode(config.GetModelFallbackConfig())
}

func (h *Handler) apiUpdateModelFallback(w http.ResponseWriter, r *http.Request) {
	var request config.ModelFallbackConfig
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	if err := config.UpdateModelFallbackConfig(request); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"config":  config.GetModelFallbackConfig(),
	})
}

// modelFallbackDecision is intentionally kept outside the public config API.
// It carries the normalized upstream model ID selected for one request.
type modelFallbackDecision struct {
	Rule        config.ModelFallbackRule
	TargetModel string
}

// exposedRequestModel keeps public response and request-log model metadata
// stable when an internal fallback route is used.
func exposedRequestModel(payload *KiroPayload, model string) string {
	if payload != nil {
		if publicModel := exposedRequestModelForContext(payload.requestContext, model); publicModel != exposedModelID(model) {
			return publicModel
		}
		if applied, from, _, _ := payload.modelFallbackInfo(); applied && strings.TrimSpace(from) != "" {
			return exposedModelID(from)
		}
	}
	return exposedModelID(model)
}

func modelFallbackEnabledForKey(apiKeyID string) bool {
	entry := config.GetApiKeyEntry(strings.TrimSpace(apiKeyID))
	if entry == nil || entry.ModelFallbackEnabled == nil {
		return true
	}
	return *entry.ModelFallbackEnabled
}

func fallbackModelNames(model string) (raw, normalized string) {
	raw = strings.ToLower(strings.TrimSpace(model))
	normalized = raw
	if raw != "" {
		if mapped, _ := ParseModelAndThinking(model, config.GetThinkingConfig().Suffix); strings.TrimSpace(mapped) != "" {
			normalized = strings.ToLower(strings.TrimSpace(mapped))
		}
	}
	return raw, normalized
}

func modelFallbackRuleAppliesToKey(rule config.ModelFallbackRule, apiKeyID string) bool {
	if len(rule.APIKeyIDs) == 0 {
		return true
	}
	apiKeyID = strings.TrimSpace(apiKeyID)
	if apiKeyID == "" {
		return false
	}
	for _, allowed := range rule.APIKeyIDs {
		if strings.EqualFold(strings.TrimSpace(allowed), apiKeyID) {
			return true
		}
	}
	return false
}

func modelFallbackRuleMatches(rule config.ModelFallbackRule, model string) bool {
	raw, normalized := fallbackModelNames(model)
	pattern := strings.ToLower(strings.TrimSpace(rule.SourceModel))
	if pattern == "" {
		return false
	}
	match := func(value string) bool {
		switch strings.ToLower(strings.TrimSpace(rule.MatchType)) {
		case config.ModelFallbackMatchPrefix:
			return strings.HasPrefix(value, pattern)
		case config.ModelFallbackMatchContains:
			return strings.Contains(value, pattern)
		case config.ModelFallbackMatchRegex:
			re, err := regexp.Compile(rule.SourceModel)
			return err == nil && re.MatchString(value)
		default:
			return strings.EqualFold(value, pattern)
		}
	}
	return match(raw) || (normalized != raw && match(normalized))
}

func modelFallbackTrigger(rule config.ModelFallbackRule, fallback config.ModelFallbackConfig) string {
	trigger := strings.ToLower(strings.TrimSpace(rule.Trigger))
	if trigger == "" {
		trigger = strings.ToLower(strings.TrimSpace(fallback.DefaultTrigger))
	}
	switch trigger {
	case "unavailable":
		return config.ModelFallbackTriggerModelUnavailable
	case "on_error", "error":
		return config.ModelFallbackTriggerUpstreamError
	default:
		return trigger
	}
}

func modelFallbackTarget(rule config.ModelFallbackRule, current string) string {
	target := strings.TrimSpace(rule.TargetModel)
	if target == "" {
		return ""
	}
	mapped, _ := ParseModelAndThinking(target, config.GetThinkingConfig().Suffix)
	target = strings.TrimSpace(mapped)
	if target == "" || strings.EqualFold(target, strings.TrimSpace(current)) {
		return ""
	}
	return target
}

func resolveModelFallback(model, apiKeyID, trigger string, used map[string]bool) (modelFallbackDecision, bool) {
	fallback := config.GetModelFallbackConfig()
	if !fallback.Enabled || !modelFallbackEnabledForKey(apiKeyID) {
		return modelFallbackDecision{}, false
	}
	trigger = strings.ToLower(strings.TrimSpace(trigger))
	if trigger == "" {
		trigger = fallback.DefaultTrigger
	}
	if used == nil {
		used = make(map[string]bool)
	}
	indices := make([]int, len(fallback.Rules))
	for i := range fallback.Rules {
		indices[i] = i
	}
	sort.SliceStable(indices, func(i, j int) bool {
		left, right := fallback.Rules[indices[i]], fallback.Rules[indices[j]]
		if left.Priority != right.Priority {
			return left.Priority > right.Priority
		}
		return indices[i] < indices[j]
	})
	for _, index := range indices {
		rule := fallback.Rules[index]
		if !rule.Enabled || used[strings.ToLower(strings.TrimSpace(rule.ID))] || !modelFallbackRuleAppliesToKey(rule, apiKeyID) || !modelFallbackRuleMatches(rule, model) {
			continue
		}
		if modelFallbackTrigger(rule, fallback) != trigger {
			continue
		}
		target := modelFallbackTarget(rule, model)
		if target == "" {
			continue
		}
		return modelFallbackDecision{Rule: rule, TargetModel: target}, true
	}
	return modelFallbackDecision{}, false
}

func fallbackDecisionForUnavailableModel(model, apiKeyID string) (modelFallbackDecision, bool) {
	return resolveModelFallback(model, apiKeyID, config.ModelFallbackTriggerModelUnavailable, nil)
}

func fallbackDecisionForAlways(model, apiKeyID string) (modelFallbackDecision, bool) {
	return resolveModelFallback(model, apiKeyID, config.ModelFallbackTriggerAlways, nil)
}

// resolveRequestModelRoute applies routes that can be decided before an
// upstream request. The normal mode only activates when the requested model is
// not part of the configured protocol surface; runtime upstream failures are
// handled by CallKiroAPI.
func (h *Handler) resolveRequestModelRoute(requested, actual, apiKeyID string) (string, modelFallbackDecision, bool) {
	if decision, ok := fallbackDecisionForAlways(requested, apiKeyID); ok && h.fallbackTargetIsUsable(decision.TargetModel) {
		return decision.TargetModel, decision, true
	}
	if h != nil && !h.requestedModelAvailable(requested, actual) {
		if decision, ok := fallbackDecisionForUnavailableModel(requested, apiKeyID); ok && h.fallbackTargetIsUsable(decision.TargetModel) {
			return decision.TargetModel, decision, true
		}
	}
	return actual, modelFallbackDecision{}, false
}

func (h *Handler) fallbackTargetIsUsable(target string) bool {
	target = strings.TrimSpace(target)
	if target == "" {
		return false
	}
	if h == nil {
		return true
	}
	return h.requestedModelAvailable(target, target)
}

func fallbackTriggerForError(err error) (string, bool) {
	if err == nil {
		return "", false
	}
	upstreamErr, ok := asUpstreamError(err)
	if !ok {
		return "", false
	}
	switch upstreamErr.Kind {
	case UpstreamErrorModelUnavailable, UpstreamErrorEmptyResponse:
		return config.ModelFallbackTriggerModelUnavailable, true
	case UpstreamErrorRetryBudget:
		// Empty/telemetry-only requests are represented as a retry-budget error
		// after the request-level empty-response cap. They remain eligible for a
		// model fallback; unrelated retry exhaustion uses the general trigger.
		if strings.Contains(strings.ToLower(upstreamErr.Error()), "without actionable") ||
			strings.Contains(strings.ToLower(upstreamErr.Error()), "empty responses") {
			return config.ModelFallbackTriggerModelUnavailable, true
		}
		return config.ModelFallbackTriggerUpstreamError, true
	case UpstreamErrorRateLimit, UpstreamErrorQuota, UpstreamErrorEndpointUnavailable,
		UpstreamErrorTransient, UpstreamErrorFirstTokenTimeout, UpstreamErrorActionableTimeout,
		UpstreamErrorToolAssemblyTimeout, UpstreamErrorToolOutputTruncated, UpstreamErrorStreamTruncated:
		return config.ModelFallbackTriggerUpstreamError, true
	default:
		return "", false
	}
}

func fallbackOutputTracker(callback *KiroStreamCallback, output *atomic.Bool) *KiroStreamCallback {
	if callback == nil {
		return nil
	}
	wrapped := *callback
	if callback.OnText != nil {
		onText := callback.OnText
		wrapped.OnText = func(text string, isThinking bool) {
			if strings.TrimSpace(text) != "" {
				output.Store(true)
			}
			onText(text, isThinking)
		}
	}
	if callback.OnToolUse != nil {
		onToolUse := callback.OnToolUse
		wrapped.OnToolUse = func(toolUse KiroToolUse) {
			output.Store(true)
			onToolUse(toolUse)
		}
	}
	if callback.OnToolUseActivity != nil {
		onActivity := callback.OnToolUseActivity
		wrapped.OnToolUseActivity = func() {
			output.Store(true)
			onActivity()
		}
	}
	if callback.OnToolUseStart != nil {
		onStart := callback.OnToolUseStart
		wrapped.OnToolUseStart = func(toolUseID, name string) {
			output.Store(true)
			onStart(toolUseID, name)
		}
	}
	if callback.OnToolUseDelta != nil {
		onDelta := callback.OnToolUseDelta
		wrapped.OnToolUseDelta = func(toolUseID, input string) {
			if input != "" {
				output.Store(true)
			}
			onDelta(toolUseID, input)
		}
	}
	if callback.OnToolUseStop != nil {
		onStop := callback.OnToolUseStop
		wrapped.OnToolUseStop = func(toolUseID string) {
			output.Store(true)
			onStop(toolUseID)
		}
	}
	return &wrapped
}

func modelFallbackMayRetry(err error, outputSeen bool) bool {
	if outputSeen {
		return false
	}
	_, ok := fallbackTriggerForError(err)
	return ok
}

// CallKiroAPI routes every protocol
// path, including Responses and web-search rounds, receives the same policy.
func CallKiroAPI(account *config.Account, payload *KiroPayload, callback *KiroStreamCallback) error {
	return callKiroAPIWithModelFallback(account, payload, callback)
}

func callKiroAPIWithModelFallback(account *config.Account, payload *KiroPayload, callback *KiroStreamCallback) error {
	if payload == nil || payload.modelFallbackInProgress {
		return callKiroAPISingleModel(account, payload, callback)
	}

	originalModel := strings.TrimSpace(payload.ConversationState.CurrentMessage.UserInputMessage.ModelID)
	if originalModel == "" {
		return callKiroAPISingleModel(account, payload, callback)
	}
	apiKeyID := apiKeyIDFromContext(payload.requestContext)
	used := make(map[string]bool)
	currentModel := originalModel
	maxHops := config.GetModelFallbackConfig().MaxHops
	if maxHops < 1 {
		maxHops = 1
	}
	var lastErr error

	defer func() {
		payload.ConversationState.CurrentMessage.UserInputMessage.ModelID = originalModel
		payload.modelFallbackInProgress = false
	}()
	for hop := 0; ; hop++ {
		payload.ConversationState.CurrentMessage.UserInputMessage.ModelID = currentModel
		var outputSeen atomic.Bool
		attemptCallback := fallbackOutputTracker(callback, &outputSeen)
		payload.modelFallbackInProgress = true
		err := callKiroAPISingleModel(account, payload, attemptCallback)
		payload.modelFallbackInProgress = false
		if err == nil {
			return nil
		}
		lastErr = err
		trigger, eligible := fallbackTriggerForError(err)
		if !eligible || !modelFallbackMayRetry(err, outputSeen.Load()) || hop >= maxHops {
			return lastErr
		}
		decision, ok := resolveModelFallback(currentModel, apiKeyID, trigger, used)
		if !ok {
			return lastErr
		}
		ruleID := strings.ToLower(strings.TrimSpace(decision.Rule.ID))
		if ruleID != "" {
			used[ruleID] = true
		}
		logger.Warnf("[ModelFallback] %s -> %s after %s (rule=%s)", currentModel, decision.TargetModel, trigger, decision.Rule.ID)
		payload.recordModelFallback(originalModel, decision.TargetModel, decision.Rule.ID)
		currentModel = decision.TargetModel
	}
}
