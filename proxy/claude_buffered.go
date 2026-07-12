package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

func (h *Handler) handleClaudeBufferedStream(w http.ResponseWriter, payload *KiroPayload, model string, thinking bool, thinkingOpts claudeThinkingResponseOptions, estimatedInputTokens int, cacheProfile *promptCacheProfile, apiKeyID, routeKey string) {
	startedAt := time.Now()
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, ok := w.(http.Flusher)
	if !ok {
		h.sendClaudeError(w, 500, "api_error", "Streaming not supported")
		return
	}

	var writeMu sync.Mutex
	send := func(event string, data interface{}) {
		writeMu.Lock()
		defer writeMu.Unlock()
		jsonData, _ := json.Marshal(data)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, string(jsonData))
		flusher.Flush()
	}

	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(25 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				send("ping", map[string]string{"type": "ping"})
			case <-done:
				return
			}
		}
	}()
	defer close(done)

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
		cacheScope := h.promptCache.ScopeKey(account.ID, apiKeyID)
		cacheUsage := h.promptCache.Compute(cacheScope, cacheProfile)

		var content string
		var thinkingContent string
		var toolUses []KiroToolUse
		var inputTokens, outputTokens int
		var credits float64
		var realInputTokens int
		var upstreamUsage KiroTokenUsage
		var truncated bool

		callback := &KiroStreamCallback{
			OnText: func(text string, isThinking bool) {
				if isThinking {
					thinkingContent += text
				} else {
					content += text
				}
			},
			OnToolUse: func(tu KiroToolUse) {
				toolUses = append(toolUses, tu)
			},
			OnComplete: func(inTok, outTok int) {
				inputTokens = inTok
				outputTokens = outTok
			},
			OnUsage: func(usage KiroTokenUsage) {
				upstreamUsage = usage
			},
			OnTruncated: func(string) {
				truncated = true
			},
			OnCredits: func(c float64) {
				credits = c
			},
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

		finalContent, extractedReasoning := extractThinkingFromContent(content)
		rawThinkingContent := thinkingContent
		if thinking && rawThinkingContent == "" && extractedReasoning != "" {
			rawThinkingContent = extractedReasoning
		}
		if !thinking {
			rawThinkingContent = ""
		}

		if realInputTokens > 0 {
			inputTokens = realInputTokens
		} else if inputTokens <= 0 {
			inputTokens = estimatedInputTokens
		}
		cacheUsage, inputTokens = resolvePromptCacheUsage(cacheUsage, upstreamUsage, inputTokens, cacheProfile)
		outputTokens = estimateClaudeOutputTokens(finalContent, rawThinkingContent, toolUses)
		visibleContent := finalContent

		responseThinkingContent := rawThinkingContent
		includeEmptyThinkingBlock := thinking && thinkingOpts.OmitDisplay && rawThinkingContent != ""
		if includeEmptyThinkingBlock {
			responseThinkingContent = ""
		}
		if thinking && responseThinkingContent != "" {
			switch thinkingOpts.Format {
			case "think":
				finalContent = "<think>" + responseThinkingContent + "</think>" + finalContent
				responseThinkingContent = ""
			case "reasoning_content":
				finalContent = responseThinkingContent + finalContent
				responseThinkingContent = ""
			}
		}

		resp := KiroToClaudeResponse(finalContent, responseThinkingContent, includeEmptyThinkingBlock, toolUses, inputTokens, outputTokens, model)
		if truncated {
			resp.StopReason = "max_tokens"
		}
		resp.Usage.InputTokens = billedClaudeInputTokens(inputTokens, cacheUsage)
		resp.Usage.CacheCreationInputTokens = cacheUsage.CacheCreationInputTokens
		resp.Usage.CacheReadInputTokens = cacheUsage.CacheReadInputTokens
		if cacheProfile != nil || upstreamUsage.HasCacheBreakdown {
			resp.Usage.CacheCreation = &ClaudeCacheCreationUsage{
				Ephemeral5mInputTokens: cacheUsage.CacheCreation5mInputTokens,
				Ephemeral1hInputTokens: cacheUsage.CacheCreation1hInputTokens,
			}
		}

		h.recordSuccessForApiKey(payload.requestContext, apiKeyID, inputTokens, outputTokens, credits)
		h.pool.RecordSuccess(account.ID)
		h.pool.ClearModelUnavailable(account.ID, model)
		h.pool.UpdateStats(account.ID, inputTokens+outputTokens, credits)
		h.promptCache.Update(cacheScope, cacheProfile)
		h.promptCache.RecordUsage(cacheUsage, cacheProfile != nil || upstreamUsage.HasCacheBreakdown)
		h.recordRequestLogForPayload(payload, requestLogEntry{
			Timestamp:                time.Now().Unix(),
			Protocol:                 "claude.messages.cc.stream",
			Model:                    model,
			AccountID:                account.ID,
			AccountEmail:             account.Email,
			Status:                   "success",
			StatusCode:               200,
			DurationMs:               requestDurationMs(startedAt),
			InputTokens:              inputTokens,
			OutputTokens:             outputTokens,
			CacheReadInputTokens:     cacheUsage.CacheReadInputTokens,
			CacheCreationInputTokens: cacheUsage.CacheCreationInputTokens,
			VisibleOutputChars:       outputCharCount(visibleContent),
			ThinkingOutputChars:      outputCharCount(rawThinkingContent),
			ToolUseCount:             len(toolUses),
			StopReason:               resp.StopReason,
			Credits:                  credits,
		})

		writeClaudeResponseAsSSE(send, resp)
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
				Protocol:   "claude.messages.cc.stream",
				Model:      model,
				Status:     "failed",
				StatusCode: 429,
				DurationMs: requestDurationMs(startedAt),
				Error:      busyErr.Error(),
			})
			h.recordDiagnosticFailureForPayload("claude.messages.cc.stream", model, nil, 429, busyErr, payload)
			w.Header().Set("Retry-After", "1")
			send("error", map[string]interface{}{
				"type":  "error",
				"error": map[string]string{"type": "rate_limit_error", "message": busyErr.Error()},
			})
			return
		}
		h.sendClaudeError(w, 503, "api_error", "No available accounts")
		return
	}

	mapped := mapDownstreamError(lastErr)
	h.recordFailure()
	h.recordRequestLogForPayload(payload, requestLogEntry{
		Timestamp:  time.Now().Unix(),
		Protocol:   "claude.messages.cc.stream",
		Model:      model,
		Status:     "failed",
		StatusCode: mapped.Status,
		DurationMs: requestDurationMs(startedAt),
		Error:      lastErr.Error(),
	})
	h.recordDiagnosticFailureForPayload("claude.messages.cc.stream", model, nil, mapped.Status, lastErr, payload)
	send("error", map[string]interface{}{
		"type":  "error",
		"error": map[string]string{"type": mapped.ClaudeType, "message": lastErr.Error()},
	})
}

func writeClaudeResponseAsSSE(send func(string, interface{}), resp *ClaudeResponse) {
	if resp == nil {
		return
	}
	send("message_start", map[string]interface{}{
		"type": "message_start",
		"message": map[string]interface{}{
			"id":            resp.ID,
			"type":          "message",
			"role":          "assistant",
			"content":       []interface{}{},
			"model":         resp.Model,
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         claudeUsageMapFromResponse(resp, 0),
		},
	})

	for idx, block := range resp.Content {
		switch block.Type {
		case "thinking":
			send("content_block_start", map[string]interface{}{
				"type":  "content_block_start",
				"index": idx,
				"content_block": map[string]string{
					"type":     "thinking",
					"thinking": "",
				},
			})
			if block.Thinking != "" {
				send("content_block_delta", map[string]interface{}{
					"type":  "content_block_delta",
					"index": idx,
					"delta": map[string]string{"type": "thinking_delta", "thinking": block.Thinking},
				})
			}
			send("content_block_stop", map[string]interface{}{"type": "content_block_stop", "index": idx})
		case "tool_use":
			send("content_block_start", map[string]interface{}{
				"type":  "content_block_start",
				"index": idx,
				"content_block": map[string]interface{}{
					"type":  "tool_use",
					"id":    block.ID,
					"name":  block.Name,
					"input": map[string]interface{}{},
				},
			})
			inputJSON, _ := json.Marshal(block.Input)
			send("content_block_delta", map[string]interface{}{
				"type":  "content_block_delta",
				"index": idx,
				"delta": map[string]string{"type": "input_json_delta", "partial_json": string(inputJSON)},
			})
			send("content_block_stop", map[string]interface{}{"type": "content_block_stop", "index": idx})
		default:
			send("content_block_start", map[string]interface{}{
				"type":  "content_block_start",
				"index": idx,
				"content_block": map[string]string{
					"type": "text",
					"text": "",
				},
			})
			if strings.TrimSpace(block.Text) != "" {
				send("content_block_delta", map[string]interface{}{
					"type":  "content_block_delta",
					"index": idx,
					"delta": map[string]string{"type": "text_delta", "text": block.Text},
				})
			}
			send("content_block_stop", map[string]interface{}{"type": "content_block_stop", "index": idx})
		}
	}

	send("message_delta", map[string]interface{}{
		"type": "message_delta",
		"delta": map[string]interface{}{
			"stop_reason": resp.StopReason,
		},
		"usage": claudeUsageMapFromResponse(resp, resp.Usage.OutputTokens),
	})
	send("message_stop", map[string]string{"type": "message_stop"})
}

func claudeUsageMapFromResponse(resp *ClaudeResponse, outputTokens int) map[string]interface{} {
	usage := map[string]interface{}{
		"input_tokens":  resp.Usage.InputTokens,
		"output_tokens": outputTokens,
	}
	if resp.Usage.CacheCreationInputTokens > 0 {
		usage["cache_creation_input_tokens"] = resp.Usage.CacheCreationInputTokens
	}
	if resp.Usage.CacheReadInputTokens > 0 {
		usage["cache_read_input_tokens"] = resp.Usage.CacheReadInputTokens
	}
	if resp.Usage.CacheCreation != nil {
		usage["cache_creation"] = map[string]int{
			"ephemeral_5m_input_tokens": resp.Usage.CacheCreation.Ephemeral5mInputTokens,
			"ephemeral_1h_input_tokens": resp.Usage.CacheCreation.Ephemeral1hInputTokens,
		}
	}
	return usage
}
