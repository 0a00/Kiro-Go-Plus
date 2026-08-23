package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"kiro-go/config"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

var errWebSearchBudgetExhausted = errors.New("web_search max_uses exhausted")

type webSearchRoundOutcome struct {
	text            string
	thinking        string
	toolUses        []KiroToolUse
	inputTokens     int
	outputTokens    int
	thinkingTokens  int
	credits         float64
	cacheUsage      promptCacheUsage
	cacheDiagnostic promptCacheDiagnostic
	truncated       bool
	stopReason      string
	account         *config.Account
	payload         *KiroPayload
}

type webSearchExecution struct {
	toolUseID string
	query     string
	results   *webSearchResults
}

type webSearchLoopResult struct {
	content           []ClaudeContentBlock
	stopReason        string
	inputTokens       int
	outputTokens      int
	thinkingTokens    int
	credits           float64
	cacheUsage        promptCacheUsage
	webSearchRequests int
	visibleText       string
	thinkingText      string
	toolUseCount      int
	lastAccount       *config.Account
	lastPayload       *KiroPayload
}

type webSearchRoundFunc func(*ClaudeRequest) (*webSearchRoundOutcome, error)
type webSearchExecFunc func(context.Context, string) (*webSearchExecution, error)

func resolveWebSearchMaxUses(tools []ClaudeTool, serverMax int) int {
	if serverMax < config.MinWebSearchMaxRounds || serverMax > config.MaxWebSearchMaxRounds {
		serverMax = config.DefaultWebSearchMaxRounds
	}
	clientMax := 0
	for _, tool := range tools {
		if !isNativeWebSearchTool(tool) || tool.MaxUses <= 0 {
			continue
		}
		if tool.MaxUses > clientMax {
			clientMax = tool.MaxUses
		}
	}
	if clientMax > 0 && clientMax < serverMax {
		return clientMax
	}
	return serverMax
}

func prepareNativeWebSearchTools(tools []ClaudeTool) []ClaudeTool {
	prepared := append([]ClaudeTool(nil), tools...)
	for i := range prepared {
		if !isNativeWebSearchTool(prepared[i]) {
			continue
		}
		prepared[i].Description = "Search the public web for current information. Provide a concise query in the query field."
		prepared[i].InputSchema = map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"query": map[string]interface{}{"type": "string"},
			},
			"required":             []interface{}{"query"},
			"additionalProperties": false,
		}
	}
	return prepared
}

func executeWebSearchLoop(
	ctx context.Context,
	req *ClaudeRequest,
	maxUses int,
	thinking bool,
	thinkingOpts claudeThinkingResponseOptions,
	callRound webSearchRoundFunc,
	executeSearch webSearchExecFunc,
) (*webSearchLoopResult, error) {
	if req == nil || callRound == nil || executeSearch == nil {
		return nil, fmt.Errorf("invalid web_search loop configuration")
	}
	if maxUses <= 0 {
		return nil, fmt.Errorf("%w: limit=%d", errWebSearchBudgetExhausted, maxUses)
	}

	working := *req
	working.Messages = append([]ClaudeMessage(nil), req.Messages...)
	working.Tools = prepareNativeWebSearchTools(req.Tools)
	presentation := make([]ClaudeContentBlock, 0)
	result := &webSearchLoopResult{}
	searchCount := 0

	for roundIndex := 0; roundIndex <= maxUses; roundIndex++ {
		if err := ctx.Err(); err != nil {
			return nil, classifyRequestCancellation("Kiro WebSearch loop", err)
		}
		round, err := callRound(&working)
		if err != nil {
			return nil, err
		}
		if round == nil {
			return nil, fmt.Errorf("web_search model round returned no outcome")
		}
		accumulateWebSearchRound(result, round)

		searchUses := countWebSearchToolUses(round.toolUses)
		allSearch := searchUses > 0 && searchUses == len(round.toolUses)
		if allSearch {
			executions, err := executeWebSearchUses(ctx, round.toolUses, maxUses-searchCount, executeSearch)
			if err != nil {
				return nil, err
			}
			searchCount += searchUses
			result.webSearchRequests = searchCount
			appendWebSearchRound(&working, round, executions, &presentation)
			continue
		}

		var executions []*webSearchExecution
		if searchUses > 0 {
			executions, err = executeWebSearchUses(ctx, round.toolUses, maxUses-searchCount, executeSearch)
			if err != nil {
				return nil, err
			}
			searchCount += searchUses
			result.webSearchRequests = searchCount
		}

		result.content, result.visibleText, result.thinkingText = buildWebSearchLoopContent(
			presentation, round, executions, thinking, thinkingOpts,
		)
		result.stopReason = resolveWebSearchLoopStopReason(round)
		result.toolUseCount = len(round.toolUses)
		if result.outputTokens <= 0 {
			result.outputTokens = estimateClaudeContentBlockTokens(result.content)
		}
		if result.inputTokens <= 0 {
			result.inputTokens = 1
		}
		return result, nil
	}

	return nil, fmt.Errorf("%w: model did not complete after %d searches", errWebSearchBudgetExhausted, maxUses)
}

func accumulateWebSearchRound(result *webSearchLoopResult, round *webSearchRoundOutcome) {
	if result == nil || round == nil {
		return
	}
	result.inputTokens += maxInt(round.inputTokens, 0)
	result.outputTokens += maxInt(round.outputTokens, 0)
	result.thinkingTokens += maxInt(round.thinkingTokens, 0)
	result.credits += round.credits
	result.cacheUsage.CacheCreationInputTokens += round.cacheUsage.CacheCreationInputTokens
	result.cacheUsage.CacheReadInputTokens += round.cacheUsage.CacheReadInputTokens
	result.cacheUsage.CacheCreation5mInputTokens += round.cacheUsage.CacheCreation5mInputTokens
	result.cacheUsage.CacheCreation1hInputTokens += round.cacheUsage.CacheCreation1hInputTokens
	result.lastAccount = round.account
	result.lastPayload = round.payload
}

func countWebSearchToolUses(toolUses []KiroToolUse) int {
	count := 0
	for _, toolUse := range toolUses {
		if strings.TrimSpace(toolUse.Name) == webSearchToolName {
			count++
		}
	}
	return count
}

func executeWebSearchUses(ctx context.Context, toolUses []KiroToolUse, remaining int, executeSearch webSearchExecFunc) ([]*webSearchExecution, error) {
	required := countWebSearchToolUses(toolUses)
	if required > remaining {
		return nil, fmt.Errorf("%w: need %d searches with %d remaining", errWebSearchBudgetExhausted, required, maxInt(remaining, 0))
	}
	executions := make([]*webSearchExecution, len(toolUses))
	for i, toolUse := range toolUses {
		if strings.TrimSpace(toolUse.Name) != webSearchToolName {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, classifyRequestCancellation("Kiro MCP WebSearch", err)
		}
		query := webSearchToolUseQuery(toolUse.Input)
		if query == "" {
			return nil, fmt.Errorf("web_search tool call %q is missing query", toolUse.ToolUseID)
		}
		execution, err := executeSearch(ctx, query)
		if err != nil {
			return nil, err
		}
		if execution == nil {
			return nil, fmt.Errorf("web_search returned no execution result")
		}
		execution.query = query
		if execution.toolUseID == "" {
			execution.toolUseID = newWebSearchServerToolUseID()
		}
		executions[i] = execution
	}
	return executions, nil
}

func webSearchToolUseQuery(input map[string]interface{}) string {
	for _, key := range []string{"query", "search_query", "q"} {
		if value, ok := input[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func appendWebSearchRound(req *ClaudeRequest, round *webSearchRoundOutcome, executions []*webSearchExecution, presentation *[]ClaudeContentBlock) {
	assistantContent := make([]interface{}, 0, len(round.toolUses)+1)
	if strings.TrimSpace(round.text) != "" {
		assistantContent = append(assistantContent, map[string]interface{}{"type": "text", "text": round.text})
	}
	for _, toolUse := range round.toolUses {
		input := toolUse.Input
		if input == nil {
			input = map[string]interface{}{}
		}
		assistantContent = append(assistantContent, map[string]interface{}{
			"type": "tool_use", "id": toolUse.ToolUseID, "name": toolUse.Name, "input": input,
		})
	}
	req.Messages = append(req.Messages, ClaudeMessage{Role: "assistant", Content: assistantContent})

	userContent := make([]interface{}, 0, len(round.toolUses))
	for i, toolUse := range round.toolUses {
		if strings.TrimSpace(toolUse.Name) != webSearchToolName {
			continue
		}
		var execution *webSearchExecution
		if i < len(executions) {
			execution = executions[i]
		}
		if execution == nil {
			continue
		}
		userContent = append(userContent, map[string]interface{}{
			"type":        "tool_result",
			"tool_use_id": toolUse.ToolUseID,
			"content":     webSearchSummary(execution.query, execution.results),
		})
		*presentation = append(*presentation, webSearchExecutionBlocks(execution)...)
	}
	req.Messages = append(req.Messages, ClaudeMessage{Role: "user", Content: userContent})

	if req.RequiredToolName == "" || req.RequiredToolName == webSearchToolName {
		req.RequireToolUse = false
		req.RequiredToolName = ""
		req.ToolUsePolicy = ""
		req.ToolChoice = nil
	}
}

func buildWebSearchLoopContent(
	presentation []ClaudeContentBlock,
	round *webSearchRoundOutcome,
	executions []*webSearchExecution,
	thinking bool,
	thinkingOpts claudeThinkingResponseOptions,
) ([]ClaudeContentBlock, string, string) {
	content := append([]ClaudeContentBlock(nil), presentation...)
	text := round.text
	thinkingText := round.thinking
	if !thinking {
		thinkingText = ""
	}
	if thinkingText != "" {
		switch thinkingOpts.Format {
		case "think":
			text = "<think>" + thinkingText + "</think>" + text
			thinkingText = ""
		case "reasoning_content":
			text = thinkingText + text
			thinkingText = ""
		default:
			displayed := thinkingText
			if thinkingOpts.OmitDisplay {
				displayed = ""
			}
			content = append(content, ClaudeContentBlock{Type: "thinking", Thinking: displayed})
		}
	}
	if strings.TrimSpace(text) != "" {
		content = append(content, ClaudeContentBlock{Type: "text", Text: text})
	}
	for i, toolUse := range round.toolUses {
		if strings.TrimSpace(toolUse.Name) == webSearchToolName {
			if i < len(executions) && executions[i] != nil {
				content = append(content, webSearchExecutionBlocks(executions[i])...)
			}
			continue
		}
		input := toolUse.Input
		if input == nil {
			input = map[string]interface{}{}
		}
		content = append(content, ClaudeContentBlock{
			Type: "tool_use", ID: toolUse.ToolUseID, Name: toolUse.Name, Input: input,
		})
	}
	return content, text, thinkingText
}

func webSearchExecutionBlocks(execution *webSearchExecution) []ClaudeContentBlock {
	if execution == nil {
		return nil
	}
	return []ClaudeContentBlock{
		{
			Type: "server_tool_use", ID: execution.toolUseID, Name: webSearchToolName,
			Input: map[string]interface{}{"query": execution.query},
		},
		{
			Type: "web_search_tool_result", ToolUseID: execution.toolUseID,
			Content: webSearchResultBlocks(execution.results),
		},
	}
}

func resolveWebSearchLoopStopReason(round *webSearchRoundOutcome) string {
	if round != nil && round.truncated {
		return "max_tokens"
	}
	if round != nil {
		for _, toolUse := range round.toolUses {
			if strings.TrimSpace(toolUse.Name) != webSearchToolName {
				return "tool_use"
			}
		}
		return mapClaudeStopReason(round.stopReason, len(round.toolUses))
	}
	return "end_turn"
}

func estimateClaudeContentBlockTokens(content []ClaudeContentBlock) int {
	total := 0
	for _, block := range content {
		switch block.Type {
		case "text":
			total += estimateApproxTokens(block.Text)
		case "thinking":
			total += estimateApproxTokens(block.Thinking)
		case "tool_use", "server_tool_use":
			total += estimateApproxTokens(block.Name)
			total += estimateJSONTokens(block.Input)
		case "web_search_tool_result":
			total += estimateJSONTokens(block.Content)
		}
	}
	return maxInt(total, 1)
}

func newWebSearchServerToolUseID() string {
	return "srvtoolu_" + strings.ReplaceAll(uuid.New().String(), "-", "")
}

func (h *Handler) callClaudeWebSearchRound(
	ctx context.Context,
	req *ClaudeRequest,
	thinking bool,
	thinkingOpts claudeThinkingResponseOptions,
	contextWindowTokens int,
	apiKeyID, namespace string,
	startedAt time.Time,
	attemptBudget *upstreamAttemptBudget,
) (*webSearchRoundOutcome, error) {
	roundReq := *req
	roundReq.Stream = false
	estimatedInputTokens := estimateClaudeRequestInputTokens(cloneClaudeRequestForThinking(&roundReq, thinking))
	cacheProfile := h.promptCache.BuildClaudeProfile(cloneClaudeRequestForThinking(&roundReq, thinking), estimatedInputTokens)
	payload := ClaudeToKiro(&roundReq, thinking)
	payload.requestContext = ctx
	payload.contextWindowTokens = contextWindowTokens
	payload.attemptBudget = attemptBudget
	truncatePayloadToLimit(payload, payload.hasSystemPriming)
	namespaceConversationID(payload, namespace)
	configureClaudeToolStreaming(payload, &roundReq, thinking, thinkingOpts, config.GetThinkingConfig())
	payload.beginStreamMetrics(startedAt)
	routeKey := payload.ConversationState.ConversationID

	attempts := h.newAccountAttemptController(ctx)
	excluded := attempts.excluded
	var lastErr error
	var busyErr error
	for {
		account, guard, busy := h.acquireNextAccountForRequest(attempts, req.Model, routeKey, payload)
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
		if err := h.ensureValidTokenContext(ctx, account); err != nil {
			release()
			lastErr = err
			excluded[account.ID] = true
			h.handleAccountFailure(account, err)
			continue
		}

		cacheScope := h.promptCache.ScopeKey(account.ID, apiKeyID)
		cacheUsage, cacheDiagnostic := h.promptCache.ComputeDetailed(cacheScope, cacheProfile)
		payload.setPromptCacheDiagnostic(cacheDiagnostic)
		var text, thinkingText strings.Builder
		var toolUses []KiroToolUse
		var inputTokens, outputTokens, realInputTokens int
		var credits float64
		var upstreamUsage KiroTokenUsage
		var upstreamStopReason string
		callback := &KiroStreamCallback{
			OnText: func(value string, isThinking bool) {
				if isThinking {
					thinkingText.WriteString(value)
				} else {
					text.WriteString(value)
				}
			},
			OnToolUse: func(toolUse KiroToolUse) {
				toolUses = append(toolUses, toolUse)
			},
			OnComplete: func(inTokens, outTokens int) {
				inputTokens = inTokens
				outputTokens = outTokens
			},
			OnUsage: func(usage KiroTokenUsage) {
				upstreamUsage = usage
			},
			OnStopReason: func(reason string) { upstreamStopReason = reason },
			OnCredits: func(value float64) {
				credits = value
			},
			OnContextUsage: func(percentage float64) {
				realInputTokens = int(percentage * float64(getPayloadContextWindowSize(payload, req.Model)) / 100.0)
			},
		}

		err := h.callKiroAPIWithHealth(account, payload, callback)
		if err == nil {
			h.pool.RecordUpstreamSuccess(account.ID, account.ProfileArn, req.Model)
		}
		release()
		if err != nil {
			lastErr = err
			excluded[account.ID] = true
			h.handleAccountFailureForModel(account, req.Model, err)
			if !shouldRetryAcrossAccounts(err) {
				break
			}
			continue
		}

		finalText, extractedThinking := extractThinkingFromContent(text.String())
		finalThinking := thinkingText.String()
		if thinking && finalThinking == "" {
			finalThinking = extractedThinking
		}
		if !thinking {
			finalThinking = ""
		}
		if realInputTokens > 0 {
			inputTokens = realInputTokens
		} else if inputTokens <= 0 {
			inputTokens = estimatedInputTokens
		}
		cacheUsage, inputTokens = resolvePromptCacheUsage(cacheUsage, upstreamUsage, inputTokens, cacheProfile)
		cacheDiagnostic = finalizePromptCacheDiagnostic(cacheDiagnostic, upstreamUsage, cacheUsage, inputTokens)
		payload.setPromptCacheDiagnostic(cacheDiagnostic)
		thinkingTokens := upstreamUsage.ThinkingTokens
		if thinkingTokens <= 0 {
			thinkingTokens = estimateApproxTokens(finalThinking)
		}
		outputTokens = estimateClaudeOutputTokens(finalText, finalThinking, toolUses)

		h.pool.RecordSuccess(account.ID)
		h.pool.ClearModelUnavailable(account.ID, req.Model)
		h.pool.UpdateStats(account.ID, inputTokens+outputTokens, credits)
		h.promptCache.Update(cacheScope, cacheProfile)
		h.promptCache.RecordDecision(cacheUsage, cacheDiagnostic)
		return &webSearchRoundOutcome{
			text: finalText, thinking: finalThinking, toolUses: toolUses,
			inputTokens: inputTokens, outputTokens: outputTokens, thinkingTokens: thinkingTokens,
			credits: credits, cacheUsage: cacheUsage, cacheDiagnostic: cacheDiagnostic,
			stopReason: upstreamStopReason, account: account, payload: payload,
		}, nil
	}

	if stopErr := attempts.stopErr(); stopErr != nil {
		if isAccountSelectionTimeout(stopErr) {
			return nil, stopErr
		}
		return nil, classifyRequestCancellation("Kiro WebSearch loop", stopErr)
	}
	if lastErr != nil {
		return nil, lastErr
	}
	if busyErr != nil {
		return nil, busyErr
	}
	return nil, fmt.Errorf("no available accounts for web_search model round")
}

func (h *Handler) handleClaudeWebSearchLoop(
	ctx context.Context,
	w http.ResponseWriter,
	req *ClaudeRequest,
	thinking bool,
	thinkingOpts claudeThinkingResponseOptions,
	contextWindowTokens, estimatedInputTokens int,
	apiKeyID, namespace string,
) {
	startedAt := time.Now()
	firstContent := newRequestFirstContentTimer(startedAt)
	var stream *webSearchSSESession
	if req.Stream {
		var err error
		stream, err = newWebSearchSSESession(ctx, h, w, req.Model, estimatedInputTokens, firstContent)
		if err != nil {
			h.sendClaudeError(w, http.StatusInternalServerError, "api_error", err.Error())
			return
		}
		defer stream.close()
	}

	webSearchConfig := config.GetWebSearchConfig()
	maxUses := resolveWebSearchMaxUses(req.Tools, webSearchConfig.MaxRounds)
	attemptBudget := newUpstreamAttemptBudget()
	result, err := executeWebSearchLoop(
		ctx, req, maxUses, thinking, thinkingOpts,
		func(working *ClaudeRequest) (*webSearchRoundOutcome, error) {
			return h.callClaudeWebSearchRound(ctx, working, thinking, thinkingOpts, contextWindowTokens, apiKeyID, namespace, startedAt, attemptBudget)
		},
		func(searchCtx context.Context, query string) (*webSearchExecution, error) {
			results, searchErr := h.callWebSearchMCP(searchCtx, req.Model, query)
			if searchErr != nil {
				return nil, searchErr
			}
			return &webSearchExecution{toolUseID: newWebSearchServerToolUseID(), query: query, results: results}, nil
		},
	)
	if err != nil {
		if upstreamErr, ok := asUpstreamError(err); ok && upstreamErr.Kind == UpstreamErrorCanceled {
			return
		}
		mapped := mapDownstreamError(err)
		if isAccountSelectionTimeout(err) {
			mapped.Status = http.StatusGatewayTimeout
		}
		h.recordFailure()
		if trace := requestDetailTraceFromContext(ctx); trace != nil {
			trace.recordError(err)
		}
		h.recordDiagnosticFailure(diagnosticLogEntry{
			RequestID: requestIDFromContext(ctx), Protocol: "claude.web_search.loop", Model: req.Model,
			StatusCode: mapped.Status, Error: err.Error(), RequestSummary: summarizeClaudeRequest(req),
		})
		entry := requestLogEntry{
			Timestamp: time.Now().Unix(), Protocol: "claude.web_search.loop", Model: req.Model,
			Status: "failed", StatusCode: mapped.Status, Error: err.Error(),
		}
		if stream != nil {
			stream.sendError(mapped.ClaudeType, err.Error())
		} else {
			applyDownstreamErrorHeaders(w, mapped)
			h.sendClaudeError(w, mapped.Status, mapped.ClaudeType, err.Error())
		}
		entry.DurationMs = requestDurationMs(startedAt)
		firstContent.Apply(&entry)
		h.recordRequestLogForContext(ctx, entry)
		return
	}

	if trace := requestDetailTraceFromContext(ctx); trace != nil {
		trace.recordComplete(result.inputTokens, result.outputTokens)
	}
	h.recordSuccessForApiKey(ctx, apiKeyID, result.inputTokens, result.outputTokens, result.credits)
	entry := requestLogEntry{
		Timestamp: time.Now().Unix(), Protocol: "claude.web_search.loop", Model: req.Model,
		Status: "success", StatusCode: http.StatusOK,
		InputTokens: result.inputTokens, OutputTokens: result.outputTokens, ThinkingTokens: result.thinkingTokens,
		CacheReadInputTokens:     result.cacheUsage.CacheReadInputTokens,
		CacheCreationInputTokens: result.cacheUsage.CacheCreationInputTokens,
		WebSearchRequests:        result.webSearchRequests,
		VisibleOutputChars:       outputCharCount(result.visibleText), ThinkingOutputChars: outputCharCount(result.thinkingText),
		ToolUseCount: result.toolUseCount, StopReason: result.stopReason, Credits: result.credits,
	}
	if result.lastAccount != nil {
		entry.AccountID = result.lastAccount.ID
		entry.AccountEmail = result.lastAccount.Email
	}

	if stream != nil {
		stream.finish(result.content, result.stopReason, result.outputTokens, result.webSearchRequests)
	} else {
		markWebSearchFirstContent(firstContent, result.content)
		response := buildWebSearchLoopResponse(req.Model, result)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(response)
	}
	entry.DurationMs = requestDurationMs(startedAt)
	firstContent.Apply(&entry)
	if result.lastPayload != nil {
		h.recordRequestLogForPayload(result.lastPayload, entry)
	} else {
		h.recordRequestLogForContext(ctx, entry)
	}
}

func buildWebSearchLoopResponse(model string, result *webSearchLoopResult) *ClaudeResponse {
	if result == nil {
		return nil
	}
	return &ClaudeResponse{
		ID: "msg_" + uuid.New().String(), Type: "message", Role: "assistant", Content: result.content,
		Model: exposedModelID(model), StopReason: result.stopReason,
		Usage: ClaudeUsage{
			InputTokens:  billedClaudeInputTokens(result.inputTokens, result.cacheUsage),
			OutputTokens: result.outputTokens, ThinkingTokens: result.thinkingTokens,
			CacheCreationInputTokens: result.cacheUsage.CacheCreationInputTokens,
			CacheReadInputTokens:     result.cacheUsage.CacheReadInputTokens,
			CacheCreation: &ClaudeCacheCreationUsage{
				Ephemeral5mInputTokens: result.cacheUsage.CacheCreation5mInputTokens,
				Ephemeral1hInputTokens: result.cacheUsage.CacheCreation1hInputTokens,
			},
			ServerToolUse: &ClaudeServerToolUsage{WebSearchRequests: result.webSearchRequests},
		},
	}
}

func summarizeClaudeRequest(req *ClaudeRequest) string {
	if req == nil {
		return ""
	}
	return fmt.Sprintf("messages=%d tools=%d stream=%t", len(req.Messages), len(req.Tools), req.Stream)
}

func markWebSearchFirstContent(timer *requestFirstContentTimer, content []ClaudeContentBlock) {
	for _, block := range content {
		switch block.Type {
		case "thinking":
			timer.MarkThinking(block.Thinking)
		case "text":
			timer.MarkVisibleText(block.Text)
		case "tool_use", "server_tool_use", "web_search_tool_result":
			timer.MarkToolOutput()
		}
		if timer.Value() != nil {
			return
		}
	}
}

type webSearchSSESession struct {
	ctx       context.Context
	handler   *Handler
	w         http.ResponseWriter
	flusher   http.Flusher
	model     string
	messageID string
	timing    *requestFirstContentTimer
	mu        sync.Mutex
	done      chan struct{}
	stopOnce  sync.Once
	wg        sync.WaitGroup
	finished  bool
}

func newWebSearchSSESession(ctx context.Context, handler *Handler, w http.ResponseWriter, model string, inputTokens int, timing *requestFirstContentTimer) (*webSearchSSESession, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil, fmt.Errorf("streaming not supported")
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	responseModel := exposedModelID(model)
	session := &webSearchSSESession{
		ctx: ctx, handler: handler, w: w, flusher: flusher, model: model,
		messageID: "msg_" + uuid.New().String(), timing: timing, done: make(chan struct{}),
	}
	session.sendLocked("message_start", map[string]interface{}{
		"type": "message_start",
		"message": map[string]interface{}{
			"id": session.messageID, "type": "message", "role": "assistant", "model": responseModel,
			"content": []interface{}{}, "stop_reason": nil, "stop_sequence": nil,
			"usage": buildClaudeUsageMap(inputTokens, 0, 0, promptCacheUsage{}, false),
		},
	}, false)
	session.wg.Add(1)
	go session.heartbeat()
	return session, nil
}

func (s *webSearchSSESession) heartbeat() {
	defer s.wg.Done()
	ticker := time.NewTicker(claudeStreamHeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.mu.Lock()
			if !s.finished {
				s.sendLocked("ping", map[string]string{"type": "ping"}, true)
			}
			s.mu.Unlock()
		case <-s.done:
			return
		case <-s.ctx.Done():
			return
		}
	}
}

func (s *webSearchSSESession) stopHeartbeat() {
	s.stopOnce.Do(func() {
		close(s.done)
		s.wg.Wait()
	})
}

func (s *webSearchSSESession) finish(content []ClaudeContentBlock, stopReason string, outputTokens, webSearchRequests int) {
	s.stopHeartbeat()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finished {
		return
	}
	for index, block := range content {
		s.sendContentBlockLocked(index, block)
	}
	s.sendLocked("message_delta", map[string]interface{}{
		"type":  "message_delta",
		"delta": map[string]interface{}{"stop_reason": stopReason},
		"usage": map[string]interface{}{
			"output_tokens":   outputTokens,
			"server_tool_use": map[string]int{"web_search_requests": webSearchRequests},
		},
	}, false)
	s.sendLocked("message_stop", map[string]interface{}{"type": "message_stop"}, false)
	s.finished = true
}

func (s *webSearchSSESession) sendError(errorType, message string) {
	s.stopHeartbeat()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finished {
		return
	}
	s.sendLocked("error", map[string]interface{}{
		"type": "error", "error": map[string]string{"type": errorType, "message": message},
	}, false)
	s.finished = true
}

func (s *webSearchSSESession) close() {
	s.stopHeartbeat()
}

func (s *webSearchSSESession) sendContentBlockLocked(index int, block ClaudeContentBlock) {
	start := map[string]interface{}{"type": "content_block_start", "index": index}
	switch block.Type {
	case "text":
		start["content_block"] = map[string]interface{}{"type": "text", "text": ""}
		s.sendLocked("content_block_start", start, false)
		if block.Text != "" {
			s.timing.MarkVisibleText(block.Text)
			s.sendLocked("content_block_delta", map[string]interface{}{
				"type": "content_block_delta", "index": index,
				"delta": map[string]interface{}{"type": "text_delta", "text": block.Text},
			}, false)
		}
	case "thinking":
		start["content_block"] = map[string]interface{}{"type": "thinking", "thinking": ""}
		s.sendLocked("content_block_start", start, false)
		if block.Thinking != "" {
			s.timing.MarkThinking(block.Thinking)
			s.sendLocked("content_block_delta", map[string]interface{}{
				"type": "content_block_delta", "index": index,
				"delta": map[string]interface{}{"type": "thinking_delta", "thinking": block.Thinking},
			}, false)
		}
	case "tool_use":
		start["content_block"] = map[string]interface{}{
			"type": "tool_use", "id": block.ID, "name": block.Name, "input": map[string]interface{}{},
		}
		s.timing.MarkToolOutput()
		s.sendLocked("content_block_start", start, false)
		partial, _ := json.Marshal(block.Input)
		s.sendLocked("content_block_delta", map[string]interface{}{
			"type": "content_block_delta", "index": index,
			"delta": map[string]interface{}{"type": "input_json_delta", "partial_json": string(partial)},
		}, false)
	case "server_tool_use", "web_search_tool_result":
		start["content_block"] = block
		s.timing.MarkToolOutput()
		s.sendLocked("content_block_start", start, false)
	default:
		return
	}
	s.sendLocked("content_block_stop", map[string]interface{}{"type": "content_block_stop", "index": index}, false)
}

func (s *webSearchSSESession) sendLocked(event string, data interface{}, heartbeat bool) {
	s.timing.MarkSSEEvent(heartbeat)
	s.handler.sendSSE(s.w, s.flusher, event, data)
}
