package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"kiro-go/config"
	"net/http"
	"sort"
	"strings"
	"time"
)

const defaultModelHealthTimeout = 90 * time.Second

type modelCapabilityHealth struct {
	Tested    bool   `json:"tested"`
	Supported bool   `json:"supported"`
	LatencyMs int64  `json:"latencyMs,omitempty"`
	AccountID string `json:"accountId,omitempty"`
	Endpoint  string `json:"endpoint,omitempty"`
	Error     string `json:"error,omitempty"`
}

type modelHealthState struct {
	Model       string                `json:"model"`
	KiroModelID string                `json:"kiroModelId"`
	Status      string                `json:"status"`
	Advertised  bool                  `json:"advertised"`
	TestedAt    int64                 `json:"testedAt"`
	Text        modelCapabilityHealth `json:"text"`
	Thinking    modelCapabilityHealth `json:"thinking"`
	Tools       modelCapabilityHealth `json:"tools"`
}

type modelHealthTestRequest struct {
	Model          string `json:"model"`
	TestThinking   *bool  `json:"testThinking,omitempty"`
	TestTools      *bool  `json:"testTools,omitempty"`
	TimeoutSeconds int    `json:"timeoutSeconds,omitempty"`
}

type modelHealthProbeKind string

const (
	modelHealthProbeText     modelHealthProbeKind = "text"
	modelHealthProbeThinking modelHealthProbeKind = "thinking"
	modelHealthProbeTools    modelHealthProbeKind = "tools"
)

func (h *Handler) apiGetModelHealth(w http.ResponseWriter, _ *http.Request) {
	json.NewEncoder(w).Encode(map[string]interface{}{
		"running": h != nil && h.modelHealthRunning.Load(),
		"results": h.modelHealthSnapshot(),
	})
}

func (h *Handler) apiTestModelHealth(w http.ResponseWriter, r *http.Request) {
	if h == nil || !h.modelHealthRunning.CompareAndSwap(false, true) {
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(map[string]string{"error": "a model capability test is already running"})
		return
	}
	defer h.modelHealthRunning.Store(false)

	var req modelHealthTestRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = "claude-sonnet-4.6"
	}
	actualModel, _ := ParseModelAndThinking(model, config.GetThinkingConfig().Suffix)
	if !h.requestedModelAvailable(model, actualModel) {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "The requested model is not available"})
		return
	}

	testThinking := req.TestThinking == nil || *req.TestThinking
	testTools := req.TestTools == nil || *req.TestTools
	timeout := time.Duration(req.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = defaultModelHealthTimeout
	}
	if timeout < 10*time.Second || timeout > 5*time.Minute {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "timeoutSeconds must be between 10 and 300"})
		return
	}

	state := modelHealthState{
		Model:       model,
		KiroModelID: MapModel(actualModel),
		Advertised:  h.modelAdvertised(actualModel),
		TestedAt:    time.Now().Unix(),
	}
	state.Text = h.runModelHealthProbe(r.Context(), actualModel, modelHealthProbeText, timeout)
	if testThinking {
		state.Thinking = h.runModelHealthProbe(r.Context(), actualModel, modelHealthProbeThinking, timeout)
	}
	if testTools {
		state.Tools = h.runModelHealthProbe(r.Context(), actualModel, modelHealthProbeTools, timeout)
	}
	state.Status = summarizeModelHealthStatus(state)

	h.modelHealthMu.Lock()
	if h.modelHealth == nil {
		h.modelHealth = make(map[string]modelHealthState)
	}
	h.modelHealth[strings.ToLower(model)] = state
	h.modelHealthMu.Unlock()

	statusCode := http.StatusOK
	if state.Status == "unavailable" {
		statusCode = http.StatusBadGateway
	}
	w.WriteHeader(statusCode)
	json.NewEncoder(w).Encode(map[string]interface{}{"success": state.Status == "healthy", "result": state})
}

func (h *Handler) runModelHealthProbe(parent context.Context, model string, kind modelHealthProbeKind, timeout time.Duration) modelCapabilityHealth {
	result := modelCapabilityHealth{Tested: true}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	req := &OpenAIRequest{
		Model:     model,
		Messages:  []OpenAIMessage{{Role: "user", Content: "Reply with exactly OK."}},
		MaxTokens: 64,
	}
	thinking := kind == modelHealthProbeThinking
	if thinking {
		req.Messages[0].Content = "Think briefly, then reply with exactly OK."
		req.MaxTokens = 512
	}
	if kind == modelHealthProbeTools {
		var tool OpenAITool
		tool.Type = "function"
		tool.Function.Name = "echo_status"
		tool.Function.Description = "Return the supplied status value."
		tool.Function.Parameters = map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"value": map[string]string{"type": "string"},
			},
			"required": []string{"value"},
		}
		req.Messages[0].Content = "Call echo_status exactly once with value set to ok."
		req.MaxTokens = 256
		req.Tools = []OpenAITool{tool}
		req.ToolChoice = json.RawMessage(`{"type":"function","function":{"name":"echo_status"}}`)
		if err := applyOpenAIToolChoice(req); err != nil {
			result.Error = err.Error()
			return result
		}
	}

	payload := OpenAIToKiro(req, thinking)
	payload.requestContext = ctx
	payload.requireActionableOutput = kind == modelHealthProbeTools
	payload.requireToolUse = kind == modelHealthProbeTools
	if kind == modelHealthProbeTools {
		payload.toolUsePolicy = toolUsePolicyExplicit
	}
	payload.attemptBudget = newUpstreamAttemptBudget()

	attempts := h.newAccountAttemptController(ctx)
	var lastErr error
	for {
		account, guard, busy := h.acquireNextAccountForRequest(attempts, model, payload.ConversationState.ConversationID)
		if busy != nil {
			lastErr = busy
			break
		}
		if account == nil {
			break
		}
		release := func() {
			if guard != nil {
				guard.Release()
				guard = nil
			}
		}
		if err := h.ensureValidTokenContext(ctx, account); err != nil {
			release()
			lastErr = err
			attempts.excluded[account.ID] = true
			continue
		}

		var visible, reasoning strings.Builder
		var tools []KiroToolUse
		startedAt := time.Now()
		err := CallKiroAPI(account, payload, &KiroStreamCallback{
			OnText: func(text string, isThinking bool) {
				if isThinking {
					reasoning.WriteString(text)
				} else {
					visible.WriteString(text)
				}
			},
			OnToolUse: func(toolUse KiroToolUse) { tools = append(tools, toolUse) },
		})
		release()
		result.LatencyMs = time.Since(startedAt).Milliseconds()
		result.AccountID = account.ID
		result.Endpoint = payload.successfulEndpoint()
		if err != nil {
			lastErr = err
			attempts.excluded[account.ID] = true
			if !shouldRetryAcrossAccounts(err) {
				break
			}
			continue
		}

		switch kind {
		case modelHealthProbeThinking:
			_, taggedReasoning := extractThinkingFromContent(visible.String())
			result.Supported = strings.TrimSpace(reasoning.String()) != "" || strings.TrimSpace(taggedReasoning) != ""
			if !result.Supported {
				result.Error = "no thinking output was observed"
			}
		case modelHealthProbeTools:
			for _, toolUse := range tools {
				if toolUse.Name == "echo_status" {
					result.Supported = true
					break
				}
			}
			if !result.Supported {
				result.Error = "no valid echo_status tool call was observed"
			}
		default:
			result.Supported = strings.TrimSpace(visible.String()) != ""
			if !result.Supported {
				result.Error = "no visible text was observed"
			}
		}
		return result
	}

	if stopErr := attempts.stopErr(); stopErr != nil {
		lastErr = stopErr
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no available accounts")
	}
	result.Error = truncateDiagnosticText(lastErr.Error(), 800)
	return result
}

func summarizeModelHealthStatus(state modelHealthState) string {
	if !state.Text.Tested || !state.Text.Supported {
		return "unavailable"
	}
	for _, capability := range []modelCapabilityHealth{state.Thinking, state.Tools} {
		if capability.Tested && !capability.Supported {
			return "degraded"
		}
	}
	return "healthy"
}

func (h *Handler) modelAdvertised(model string) bool {
	if h == nil {
		return false
	}
	model = normalizeKnownModelID(model)
	h.modelsCacheMu.RLock()
	defer h.modelsCacheMu.RUnlock()
	for _, item := range h.cachedModels {
		if normalizeKnownModelID(item.ModelId) == model {
			return true
		}
	}
	return false
}

func (h *Handler) modelHealthSnapshot() []modelHealthState {
	if h == nil {
		return []modelHealthState{}
	}
	h.modelHealthMu.RLock()
	results := make([]modelHealthState, 0, len(h.modelHealth))
	for _, result := range h.modelHealth {
		results = append(results, result)
	}
	h.modelHealthMu.RUnlock()
	sort.Slice(results, func(i, j int) bool {
		if results[i].TestedAt == results[j].TestedAt {
			return results[i].Model < results[j].Model
		}
		return results[i].TestedAt > results[j].TestedAt
	})
	return results
}
