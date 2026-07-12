package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"kiro-go/config"
	"net/http"
	"strings"
	"time"
)

const defaultResponsesModel = "claude-sonnet-4.5"

func (h *Handler) handleOpenAIResponses(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method Not Allowed", 405)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		h.sendOpenAIError(w, requestBodyErrorStatus(err), "invalid_request_error", "Failed to read request body")
		return
	}

	var req ResponsesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		h.sendOpenAIError(w, 400, "invalid_request_error", "Invalid JSON")
		return
	}

	if strings.TrimSpace(req.Model) == "" {
		req.Model = defaultResponsesModel
	}

	storedInputCopy := append(json.RawMessage(nil), req.Input...)

	storeResponse := config.GetResponsesStorageConfig().DefaultStore
	if req.Store != nil {
		storeResponse = *req.Store
	}
	if storeResponse && !responsesStorageEncryptionEnabled() {
		h.sendOpenAIError(w, http.StatusServiceUnavailable, "server_error", "Responses storage requires KIRO_MASTER_KEY or KIRO_MASTER_KEY_FILE")
		return
	}

	var historyMessages []OpenAIMessage
	if req.PreviousResponseID != "" {
		prev, loadErr := loadResponseForOwner(req.PreviousResponseID, apiKeyIDFromContext(r.Context()))
		if loadErr != nil {
			h.sendOpenAIError(w, 404, "invalid_request_error", "previous_response_id not found")
			return
		}
		historyMessages = expandPreviousResponseHistory(prev)
	}

	inputMessages, err := parseResponsesInput(req.Input)
	if err != nil {
		h.sendOpenAIError(w, 400, "invalid_request_error", err.Error())
		return
	}

	finalMessages := make([]OpenAIMessage, 0, len(historyMessages)+len(inputMessages)+1)
	finalMessages = append(finalMessages, historyMessages...)
	if strings.TrimSpace(req.Instructions) != "" {
		// New instructions on this turn always take effect, even when
		// continuing from previous_response_id. Place them after the
		// expanded history so they apply to the current and future turns,
		// while ancestor instructions (re-emitted by expandPreviousResponseHistory)
		// stay in scope for the historical exchanges they shaped.
		finalMessages = append(finalMessages, OpenAIMessage{
			Role:    "system",
			Content: req.Instructions,
		})
	}
	finalMessages = append(finalMessages, inputMessages...)

	if len(finalMessages) == 0 {
		h.sendOpenAIError(w, 400, "invalid_request_error", "input must contain at least one message")
		return
	}

	hasUser := false
	for _, m := range finalMessages {
		if m.Role == "user" {
			hasUser = true
			break
		}
	}
	if !hasUser {
		h.sendOpenAIError(w, 400, "invalid_request_error", "input must contain at least one user message")
		return
	}

	openaiReq := &OpenAIRequest{
		Model:      req.Model,
		Messages:   finalMessages,
		Stream:     req.Stream,
		Tools:      req.Tools,
		ToolChoice: req.ToolChoice,
	}
	if req.Temperature != nil {
		value := *req.Temperature
		openaiReq.Temperature = &value
	}
	if req.MaxOutputTokens != nil {
		openaiReq.MaxTokens = *req.MaxOutputTokens
	}
	if req.ContextWindow != nil {
		openaiReq.ContextWindow = *req.ContextWindow
	}
	if req.MaxInputTokens != nil {
		openaiReq.MaxInputTokens = *req.MaxInputTokens
	}
	if err := applyOpenAIToolChoice(openaiReq); err != nil {
		h.sendOpenAIError(w, 400, "invalid_request_error", err.Error())
		return
	}

	thinkingCfg := config.GetThinkingConfig()
	contextWindowTokens := applyOpenAITokenBudgetDefaults(openaiReq)
	actualModel, thinking := ParseModelAndThinking(req.Model, thinkingCfg.Suffix)
	openaiReq.Model = actualModel

	estimatedInputTokens := estimateOpenAIRequestInputTokens(openaiReq)
	if admissionErr := reserveAPIKeyTokens(r.Context(), estimatedInputTokens); admissionErr != nil {
		applyAuthErrorHeaders(w, admissionErr)
		h.sendOpenAIError(w, admissionErr.status, admissionErr.code, admissionErr.message)
		return
	}
	kiroPayload := OpenAIToKiro(openaiReq, thinking)
	kiroPayload.requestContext = r.Context()
	kiroPayload.contextWindowTokens = contextWindowTokens

	apiKeyID := apiKeyIDFromContext(r.Context())
	namespaceConversationID(kiroPayload, requestConversationNamespace(r, apiKeyID))
	respID := generateResponseID()
	routeKey := responsesRouteKey(kiroPayload, req.PreviousResponseID, respID)

	if req.Stream {
		h.handleResponsesStream(w, kiroPayload, actualModel, thinking, estimatedInputTokens,
			apiKeyID, respID, routeKey, &req, storedInputCopy, storeResponse)
		return
	}

	h.handleResponsesNonStream(w, kiroPayload, actualModel, thinking, estimatedInputTokens,
		apiKeyID, respID, routeKey, &req, storedInputCopy, storeResponse)
}

func responsesRouteKey(payload *KiroPayload, previousResponseID, responseID string) string {
	if payload != nil {
		if conversationID := strings.TrimSpace(payload.ConversationState.ConversationID); conversationID != "" {
			return conversationID
		}
	}
	if previousResponseID = strings.TrimSpace(previousResponseID); previousResponseID != "" {
		return previousResponseID
	}
	return strings.TrimSpace(responseID)
}

func (h *Handler) handleResponsesNonStream(
	w http.ResponseWriter, payload *KiroPayload, model string, thinking bool,
	estimatedInputTokens int, apiKeyID, respID, routeKey string,
	req *ResponsesRequest, storedInput json.RawMessage, storeResponse bool,
) {
	startedAt := time.Now()
	attempts := h.newAccountAttemptController(payload.requestContext)
	excluded := attempts.excluded
	var lastErr error
	var busyErr error

	for {
		account, guard, busy := h.acquireNextAccountForRequest(attempts, model, routeKey)
		if busy != nil {
			busyErr = busy
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
		if err := h.ensureValidTokenContext(payload.requestContext, account); err != nil {
			release()
			lastErr = err
			excluded[account.ID] = true
			h.handleAccountFailure(account, err)
			continue
		}

		var content, reasoningContent string
		var toolUses []KiroToolUse
		var inputTokens, outputTokens int
		var credits float64
		var realInputTokens int
		var truncated bool

		callback := &KiroStreamCallback{
			OnText: func(text string, isThinking bool) {
				if isThinking {
					reasoningContent += text
				} else {
					content += text
				}
			},
			OnToolUse:   func(tu KiroToolUse) { toolUses = append(toolUses, tu) },
			OnComplete:  func(inTok, outTok int) { inputTokens = inTok; outputTokens = outTok },
			OnTruncated: func(string) { truncated = true },
			OnCredits:   func(c float64) { credits = c },
			OnContextUsage: func(pct float64) {
				realInputTokens = int(pct * float64(getPayloadContextWindowSize(payload, model)) / 100.0)
			},
		}

		err := CallKiroAPI(account, payload, callback)
		if err == nil {
			h.pool.RecordUpstreamSuccess(account.ID, account.ProfileArn, model)
		}
		release()
		if err != nil {
			lastErr = err
			excluded[account.ID] = true
			h.handleAccountFailureForModel(account, model, err)
			if !shouldRetryAcrossAccounts(err) {
				break
			}
			continue
		}

		finalContent, _ := extractThinkingFromContent(content)
		if !thinking {
			reasoningContent = ""
		}

		if realInputTokens > 0 {
			inputTokens = realInputTokens
		} else if inputTokens <= 0 {
			inputTokens = estimatedInputTokens
		}
		outputTokens = estimateOpenAIOutputTokens(finalContent, reasoningContent, toolUses)

		h.recordSuccessForApiKey(payload.requestContext, apiKeyID, inputTokens, outputTokens, credits)
		h.pool.RecordSuccess(account.ID)
		h.pool.ClearModelUnavailable(account.ID, model)
		h.pool.UpdateStats(account.ID, inputTokens+outputTokens, credits)
		h.recordRequestLogForPayload(payload, requestLogEntry{
			Timestamp:    time.Now().Unix(),
			Protocol:     "openai.responses",
			Model:        model,
			AccountID:    account.ID,
			AccountEmail: account.Email,
			Status:       "success",
			StatusCode:   200,
			DurationMs:   requestDurationMs(startedAt),
			InputTokens:  inputTokens,
			OutputTokens: outputTokens,
			Credits:      credits,
		})

		respObj := buildResponsesObject(respID, model, finalContent, reasoningContent, toolUses, inputTokens, outputTokens, req)
		if truncated {
			markResponseIncomplete(respObj)
		}
		respObj.StoredInput = storedInput
		respObj.Instructions = req.Instructions
		respObj.OwnerAPIKeyID = apiKeyID

		if storeResponse {
			if saveErr := saveResponse(respObj); saveErr != nil {
				logResponsesPersistFailure(respObj.ID, saveErr)
			}
		}

		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(respObj)
		return
	}

	if attempts.stopErr() != nil {
		return
	}
	if lastErr == nil {
		if busyErr != nil {
			h.recordFailure()
			h.recordRequestLogForPayload(payload, requestLogEntry{
				Timestamp:  time.Now().Unix(),
				Protocol:   "openai.responses",
				Model:      model,
				Status:     "failed",
				StatusCode: 429,
				DurationMs: requestDurationMs(startedAt),
				Error:      busyErr.Error(),
			})
			h.recordDiagnosticFailureForPayload("openai.responses", model, nil, 429, busyErr, payload)
			w.Header().Set("Retry-After", "1")
			h.sendOpenAIError(w, 429, "rate_limit_error", busyErr.Error())
			return
		}
		h.sendOpenAIError(w, 503, "server_error", "No available accounts")
		return
	}
	mapped := mapDownstreamError(lastErr)
	h.recordFailure()
	h.recordRequestLogForPayload(payload, requestLogEntry{
		Timestamp:  time.Now().Unix(),
		Protocol:   "openai.responses",
		Model:      model,
		Status:     "failed",
		StatusCode: mapped.Status,
		DurationMs: requestDurationMs(startedAt),
		Error:      lastErr.Error(),
	})
	h.recordDiagnosticFailureForPayload("openai.responses", model, nil, mapped.Status, lastErr, payload)
	applyDownstreamErrorHeaders(w, mapped)
	h.sendOpenAIError(w, mapped.Status, mapped.OpenAIType, lastErr.Error())
}

func buildResponsesObject(
	id, model, content, reasoning string, toolUses []KiroToolUse,
	inputTokens, outputTokens int, req *ResponsesRequest,
) *ResponsesObject {
	output := make([]ResponseOutputItem, 0, 2+len(toolUses))

	if strings.TrimSpace(reasoning) != "" {
		output = append(output, ResponseOutputItem{
			ID:     generateOutputItemID("rs"),
			Type:   "reasoning",
			Status: "completed",
			Summary: []ResponseSummaryPart{{
				Type: "summary_text",
				Text: reasoning,
			}},
		})
	}

	if strings.TrimSpace(content) != "" {
		output = append(output, ResponseOutputItem{
			ID:     generateOutputItemID("msg"),
			Type:   "message",
			Role:   "assistant",
			Status: "completed",
			Content: []ResponseContentPart{{
				Type: "output_text",
				Text: content,
			}},
		})
	}

	for _, tu := range toolUses {
		args, _ := json.Marshal(tu.Input)
		output = append(output, ResponseOutputItem{
			ID:        generateOutputItemID("fc"),
			Type:      "function_call",
			Status:    "completed",
			CallID:    tu.ToolUseID,
			Name:      tu.Name,
			Arguments: string(args),
		})
	}

	if len(output) == 0 {
		output = append(output, ResponseOutputItem{
			ID:     generateOutputItemID("msg"),
			Type:   "message",
			Role:   "assistant",
			Status: "completed",
			Content: []ResponseContentPart{{
				Type: "output_text",
				Text: "",
			}},
		})
	}

	return &ResponsesObject{
		ID:                 id,
		Object:             "response",
		CreatedAt:          time.Now().Unix(),
		Status:             "completed",
		Model:              model,
		Output:             output,
		Usage:              ResponsesUsage{InputTokens: inputTokens, OutputTokens: outputTokens, TotalTokens: inputTokens + outputTokens},
		PreviousResponseID: req.PreviousResponseID,
		Metadata:           req.Metadata,
	}
}

func markResponseIncomplete(response *ResponsesObject) {
	if response == nil {
		return
	}
	response.Status = "incomplete"
	response.IncompleteDetails = &IncompleteDetails{Reason: "max_output_tokens"}
}

func (h *Handler) handleResponsesStream(
	w http.ResponseWriter, payload *KiroPayload, model string, thinking bool,
	estimatedInputTokens int, apiKeyID, respID, routeKey string,
	req *ResponsesRequest, storedInput json.RawMessage, storeResponse bool,
) {
	startedAt := time.Now()
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, ok := w.(http.Flusher)
	if !ok {
		h.sendOpenAIError(w, 500, "server_error", "Streaming not supported")
		return
	}

	send := func(eventName string, payload interface{}) {
		data, err := json.Marshal(payload)
		if err != nil {
			return
		}
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventName, string(data))
		flusher.Flush()
	}

	createdAt := time.Now().Unix()
	initial := &ResponsesObject{
		ID:                 respID,
		Object:             "response",
		CreatedAt:          createdAt,
		Status:             "in_progress",
		Model:              model,
		Output:             []ResponseOutputItem{},
		Usage:              ResponsesUsage{},
		PreviousResponseID: req.PreviousResponseID,
		Metadata:           req.Metadata,
	}
	send("response.created", map[string]interface{}{
		"type":     "response.created",
		"response": initial,
	})
	send("response.in_progress", map[string]interface{}{
		"type":     "response.in_progress",
		"response": initial,
	})

	attempts := h.newAccountAttemptController(payload.requestContext)
	excluded := attempts.excluded
	var lastErr error
	var busyErr error
	responseStarted := false

	for {
		account, guard, busy := h.acquireNextAccountForRequest(attempts, model, routeKey)
		if busy != nil {
			busyErr = busy
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
		if err := h.ensureValidTokenContext(payload.requestContext, account); err != nil {
			release()
			lastErr = err
			excluded[account.ID] = true
			h.handleAccountFailure(account, err)
			continue
		}

		var (
			fullText        strings.Builder
			reasoningText   strings.Builder
			toolUses        []KiroToolUse
			inputTokens     int
			outputTokens    int
			credits         float64
			realInputTokens int
			truncated       bool
		)

		messageItemID := generateOutputItemID("msg")
		reasoningItemID := generateOutputItemID("rs")
		messageStarted := false
		reasoningStarted := false
		outputIndex := 0
		contentIndex := 0

		finishReasoning := func() {
			if !reasoningStarted {
				return
			}
			text := reasoningText.String()
			send("response.reasoning_summary_text.done", map[string]interface{}{
				"type":          "response.reasoning_summary_text.done",
				"item_id":       reasoningItemID,
				"output_index":  outputIndex,
				"summary_index": 0,
				"text":          text,
			})
			send("response.reasoning_summary_part.done", map[string]interface{}{
				"type":          "response.reasoning_summary_part.done",
				"item_id":       reasoningItemID,
				"output_index":  outputIndex,
				"summary_index": 0,
				"part": map[string]interface{}{
					"type": "summary_text",
					"text": text,
				},
			})
			send("response.output_item.done", map[string]interface{}{
				"type":         "response.output_item.done",
				"output_index": outputIndex,
				"item": map[string]interface{}{
					"id":     reasoningItemID,
					"type":   "reasoning",
					"status": "completed",
					"summary": []map[string]interface{}{{
						"type": "summary_text",
						"text": text,
					}},
				},
			})
			reasoningStarted = false
			outputIndex++
		}

		ensureReasoningStarted := func() {
			if reasoningStarted {
				return
			}
			reasoningStarted = true
			send("response.output_item.added", map[string]interface{}{
				"type":         "response.output_item.added",
				"output_index": outputIndex,
				"item": map[string]interface{}{
					"id":      reasoningItemID,
					"type":    "reasoning",
					"status":  "in_progress",
					"summary": []map[string]interface{}{},
				},
			})
			send("response.reasoning_summary_part.added", map[string]interface{}{
				"type":          "response.reasoning_summary_part.added",
				"item_id":       reasoningItemID,
				"output_index":  outputIndex,
				"summary_index": 0,
				"part": map[string]interface{}{
					"type": "summary_text",
					"text": "",
				},
			})
		}

		ensureMessageStarted := func() {
			if messageStarted {
				return
			}
			finishReasoning()
			messageStarted = true
			send("response.output_item.added", map[string]interface{}{
				"type":         "response.output_item.added",
				"output_index": outputIndex,
				"item": map[string]interface{}{
					"id":      messageItemID,
					"type":    "message",
					"role":    "assistant",
					"status":  "in_progress",
					"content": []map[string]interface{}{},
				},
			})
			send("response.content_part.added", map[string]interface{}{
				"type":          "response.content_part.added",
				"item_id":       messageItemID,
				"output_index":  outputIndex,
				"content_index": contentIndex,
				"part": map[string]interface{}{
					"type": "output_text",
					"text": "",
				},
			})
		}

		callback := &KiroStreamCallback{
			OnText: func(text string, isThinking bool) {
				if text == "" {
					return
				}
				if isThinking {
					reasoningText.WriteString(text)
					if thinking {
						ensureReasoningStarted()
						send("response.reasoning_summary_text.delta", map[string]interface{}{
							"type":          "response.reasoning_summary_text.delta",
							"item_id":       reasoningItemID,
							"output_index":  outputIndex,
							"summary_index": 0,
							"delta":         text,
						})
						responseStarted = true
					}
					return
				}
				fullText.WriteString(text)
				ensureMessageStarted()
				send("response.output_text.delta", map[string]interface{}{
					"type":          "response.output_text.delta",
					"item_id":       messageItemID,
					"output_index":  outputIndex,
					"content_index": contentIndex,
					"delta":         text,
				})
				responseStarted = true
			},
			OnToolUse: func(tu KiroToolUse) {
				finishReasoning()
				if messageStarted {
					send("response.content_part.done", map[string]interface{}{
						"type":          "response.content_part.done",
						"item_id":       messageItemID,
						"output_index":  outputIndex,
						"content_index": contentIndex,
						"part": map[string]interface{}{
							"type": "output_text",
							"text": fullText.String(),
						},
					})
					send("response.output_item.done", map[string]interface{}{
						"type":         "response.output_item.done",
						"output_index": outputIndex,
						"item": map[string]interface{}{
							"id":     messageItemID,
							"type":   "message",
							"role":   "assistant",
							"status": "completed",
							"content": []map[string]interface{}{{
								"type": "output_text",
								"text": fullText.String(),
							}},
						},
					})
					messageStarted = false
					outputIndex++
				}

				toolUses = append(toolUses, tu)
				args, _ := json.Marshal(tu.Input)
				fcID := generateOutputItemID("fc")
				send("response.output_item.added", map[string]interface{}{
					"type":         "response.output_item.added",
					"output_index": outputIndex,
					"item": map[string]interface{}{
						"id":        fcID,
						"type":      "function_call",
						"status":    "in_progress",
						"call_id":   tu.ToolUseID,
						"name":      tu.Name,
						"arguments": "",
					},
				})
				send("response.function_call_arguments.delta", map[string]interface{}{
					"type":         "response.function_call_arguments.delta",
					"item_id":      fcID,
					"output_index": outputIndex,
					"delta":        string(args),
				})
				send("response.output_item.done", map[string]interface{}{
					"type":         "response.output_item.done",
					"output_index": outputIndex,
					"item": map[string]interface{}{
						"id":        fcID,
						"type":      "function_call",
						"status":    "completed",
						"call_id":   tu.ToolUseID,
						"name":      tu.Name,
						"arguments": string(args),
					},
				})
				outputIndex++
				responseStarted = true
			},
			OnComplete:  func(inTok, outTok int) { inputTokens = inTok; outputTokens = outTok },
			OnTruncated: func(string) { truncated = true },
			OnCredits:   func(c float64) { credits = c },
			OnContextUsage: func(pct float64) {
				realInputTokens = int(pct * float64(getPayloadContextWindowSize(payload, model)) / 100.0)
			},
		}

		err := CallKiroAPI(account, payload, callback)
		if err == nil {
			h.pool.RecordUpstreamSuccess(account.ID, account.ProfileArn, model)
		}
		release()
		if err != nil {
			if !responseStarted {
				lastErr = err
				excluded[account.ID] = true
				h.handleAccountFailureForModel(account, model, err)
				if !shouldRetryAcrossAccounts(err) {
					break
				}
				continue
			}
			mapped := mapDownstreamError(err)
			send("response.failed", map[string]interface{}{
				"type": "response.failed",
				"response": map[string]interface{}{
					"id":     respID,
					"status": "failed",
					"error": map[string]string{
						"type":    mapped.OpenAIType,
						"message": err.Error(),
					},
				},
			})
			h.recordFailure()
			h.recordRequestLogForPayload(payload, requestLogEntry{
				Timestamp:    time.Now().Unix(),
				Protocol:     "openai.responses.stream",
				Model:        model,
				AccountID:    account.ID,
				AccountEmail: account.Email,
				Status:       "failed",
				StatusCode:   mapped.Status,
				DurationMs:   requestDurationMs(startedAt),
				Error:        err.Error(),
			})
			h.recordDiagnosticFailureForPayload("openai.responses.stream", model, account, mapped.Status, err, payload)
			return
		}

		finalContent, _ := extractThinkingFromContent(fullText.String())
		reasoning := reasoningText.String()
		if !thinking {
			reasoning = ""
		}
		finishReasoning()

		if messageStarted {
			send("response.content_part.done", map[string]interface{}{
				"type":          "response.content_part.done",
				"item_id":       messageItemID,
				"output_index":  outputIndex,
				"content_index": contentIndex,
				"part": map[string]interface{}{
					"type": "output_text",
					"text": finalContent,
				},
			})
			send("response.output_item.done", map[string]interface{}{
				"type":         "response.output_item.done",
				"output_index": outputIndex,
				"item": map[string]interface{}{
					"id":     messageItemID,
					"type":   "message",
					"role":   "assistant",
					"status": "completed",
					"content": []map[string]interface{}{{
						"type": "output_text",
						"text": finalContent,
					}},
				},
			})
		}

		if realInputTokens > 0 {
			inputTokens = realInputTokens
		} else if inputTokens <= 0 {
			inputTokens = estimatedInputTokens
		}
		outputTokens = estimateOpenAIOutputTokens(finalContent, reasoning, toolUses)

		h.recordSuccessForApiKey(payload.requestContext, apiKeyID, inputTokens, outputTokens, credits)
		h.pool.RecordSuccess(account.ID)
		h.pool.ClearModelUnavailable(account.ID, model)
		h.pool.UpdateStats(account.ID, inputTokens+outputTokens, credits)
		h.recordRequestLogForPayload(payload, requestLogEntry{
			Timestamp:    time.Now().Unix(),
			Protocol:     "openai.responses.stream",
			Model:        model,
			AccountID:    account.ID,
			AccountEmail: account.Email,
			Status:       "success",
			StatusCode:   200,
			DurationMs:   requestDurationMs(startedAt),
			InputTokens:  inputTokens,
			OutputTokens: outputTokens,
			Credits:      credits,
		})

		respObj := buildResponsesObject(respID, model, finalContent, reasoning, toolUses, inputTokens, outputTokens, req)
		if truncated {
			markResponseIncomplete(respObj)
		}
		respObj.CreatedAt = createdAt
		respObj.StoredInput = storedInput
		respObj.Instructions = req.Instructions
		respObj.OwnerAPIKeyID = apiKeyID

		if storeResponse {
			if saveErr := saveResponse(respObj); saveErr != nil {
				logResponsesPersistFailure(respObj.ID, saveErr)
			}
		}

		completionEvent := "response.completed"
		if truncated {
			completionEvent = "response.incomplete"
		}
		send(completionEvent, map[string]interface{}{
			"type":     completionEvent,
			"response": respObj,
		})
		fmt.Fprintf(w, "data: [DONE]\n\n")
		flusher.Flush()
		return
	}

	if attempts.stopErr() != nil {
		return
	}
	if lastErr == nil {
		if busyErr != nil {
			h.recordFailure()
			h.recordRequestLogForPayload(payload, requestLogEntry{
				Timestamp:  time.Now().Unix(),
				Protocol:   "openai.responses.stream",
				Model:      model,
				Status:     "failed",
				StatusCode: 429,
				DurationMs: requestDurationMs(startedAt),
				Error:      busyErr.Error(),
			})
			h.recordDiagnosticFailureForPayload("openai.responses.stream", model, nil, 429, busyErr, payload)
			send("response.failed", map[string]interface{}{
				"type": "response.failed",
				"response": map[string]interface{}{
					"id":     respID,
					"status": "failed",
					"error": map[string]string{
						"type":    "rate_limit_error",
						"message": busyErr.Error(),
					},
				},
			})
			return
		}
		send("response.failed", map[string]interface{}{
			"type": "response.failed",
			"response": map[string]interface{}{
				"id":     respID,
				"status": "failed",
				"error": map[string]string{
					"type":    "server_error",
					"message": "No available accounts",
				},
			},
		})
		return
	}
	mapped := mapDownstreamError(lastErr)
	h.recordFailure()
	h.recordRequestLogForPayload(payload, requestLogEntry{
		Timestamp:  time.Now().Unix(),
		Protocol:   "openai.responses.stream",
		Model:      model,
		Status:     "failed",
		StatusCode: mapped.Status,
		DurationMs: requestDurationMs(startedAt),
		Error:      lastErr.Error(),
	})
	h.recordDiagnosticFailureForPayload("openai.responses.stream", model, nil, mapped.Status, lastErr, payload)
	send("response.failed", map[string]interface{}{
		"type": "response.failed",
		"response": map[string]interface{}{
			"id":     respID,
			"status": "failed",
			"error": map[string]string{
				"type":    mapped.OpenAIType,
				"message": lastErr.Error(),
			},
		},
	})
}
