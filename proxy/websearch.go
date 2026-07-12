package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"kiro-go/config"
	"kiro-go/internal/httpbody"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
)

type mcpRequest struct {
	ID      string      `json:"id"`
	JSONRPC string      `json:"jsonrpc"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params"`
}

type mcpResponse struct {
	Error  *mcpError  `json:"error,omitempty"`
	Result *mcpResult `json:"result,omitempty"`
}

type mcpError struct {
	Code    int    `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

type mcpResult struct {
	Content []mcpContent `json:"content,omitempty"`
	IsError bool         `json:"isError,omitempty"`
}

type mcpContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type webSearchResults struct {
	Results []webSearchResult `json:"results"`
	Query   string            `json:"query,omitempty"`
	Error   string            `json:"error,omitempty"`
}

type webSearchResult struct {
	Title       string `json:"title"`
	URL         string `json:"url"`
	Snippet     string `json:"snippet,omitempty"`
	PublishedAt int64  `json:"publishedDate,omitempty"`
	Domain      string `json:"domain,omitempty"`
}

func hasPureWebSearchTool(req *ClaudeRequest) bool {
	if req == nil || len(req.Tools) != 1 {
		return false
	}
	return isWebSearchToolName(req.Tools[0].Name)
}

func isWebSearchToolName(name string) bool {
	normalized := strings.ToLower(strings.TrimSpace(name))
	return normalized == "web_search" || strings.HasPrefix(normalized, "web_search_")
}

func extractWebSearchQuery(req *ClaudeRequest) string {
	if req == nil || len(req.Messages) == 0 {
		return ""
	}
	if query := extractWebSearchQueryFromToolChoice(req.ToolChoice); query != "" {
		return query
	}
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if strings.TrimSpace(req.Messages[i].Role) != "user" {
			continue
		}
		if query := extractWebSearchQueryFromContent(req.Messages[i].Content); query != "" {
			return query
		}
	}
	return ""
}

func extractWebSearchQueryFromToolChoice(toolChoice interface{}) string {
	m, ok := interfaceToStringMap(toolChoice)
	if !ok {
		return ""
	}
	if name, _ := m["name"].(string); name != "" && !isWebSearchToolName(name) {
		return ""
	}
	for _, key := range []string{"input", "arguments"} {
		if query := extractQueryFromValue(m[key]); query != "" {
			return query
		}
	}
	return extractQueryFromValue(m)
}

func extractWebSearchQueryFromContent(content interface{}) string {
	if query := extractQueryFromValue(content); query != "" {
		return query
	}
	text, _, _ := extractClaudeUserContent(content)
	return normalizeWebSearchQuery(text)
}

func extractQueryFromValue(v interface{}) string {
	switch val := v.(type) {
	case nil:
		return ""
	case string:
		return normalizeWebSearchQuery(val)
	case map[string]interface{}:
		for _, key := range []string{"query", "search_query", "q"} {
			if query := extractQueryFromValue(val[key]); query != "" {
				return query
			}
		}
	case []interface{}:
		for _, item := range val {
			if query := extractQueryFromValue(item); query != "" {
				return query
			}
		}
	case []ClaudeContentBlock:
		for _, item := range val {
			if query := extractQueryFromValue(item.Input); query != "" {
				return query
			}
			if query := normalizeWebSearchQuery(item.Text); query != "" {
				return query
			}
		}
	}
	return ""
}

func interfaceToStringMap(v interface{}) (map[string]interface{}, bool) {
	m, ok := v.(map[string]interface{})
	if ok {
		return m, true
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, false
	}
	var decoded map[string]interface{}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, false
	}
	return decoded, true
}

func normalizeWebSearchQuery(text string) string {
	text = strings.TrimSpace(text)
	const prefix = "Perform a web search for the query: "
	if strings.HasPrefix(strings.ToLower(text), strings.ToLower(prefix)) {
		text = strings.TrimSpace(text[len(prefix):])
	}
	return text
}

func (h *Handler) handleClaudeWebSearch(ctx context.Context, w http.ResponseWriter, req *ClaudeRequest, estimatedInputTokens int, apiKeyID string) {
	startedAt := time.Now()
	query := extractWebSearchQuery(req)
	if query == "" {
		h.sendClaudeError(w, 400, "invalid_request_error", "Unable to extract web_search query")
		return
	}
	results, err := h.callWebSearchMCP(ctx, req.Model, query)
	if err != nil {
		if upstreamErr, ok := asUpstreamError(err); ok && upstreamErr.Kind == UpstreamErrorCanceled {
			return
		}
		h.recordFailure()
		h.recordDiagnosticFailure(diagnosticLogEntry{
			RequestID:      requestIDFromContext(ctx),
			Protocol:       "claude.web_search",
			Model:          req.Model,
			StatusCode:     502,
			Error:          err.Error(),
			RequestSummary: query,
		})
		h.sendClaudeError(w, 502, "api_error", err.Error())
		return
	}

	output := webSearchSummary(query, results)
	outputTokens := estimateApproxTokens(output)
	h.recordSuccessForApiKey(ctx, apiKeyID, estimatedInputTokens, outputTokens, 0)
	h.recordRequestLog(requestLogEntry{
		Timestamp:    time.Now().Unix(),
		RequestID:    requestIDFromContext(ctx),
		Protocol:     "claude.web_search",
		Model:        req.Model,
		Status:       "success",
		StatusCode:   200,
		DurationMs:   requestDurationMs(startedAt),
		InputTokens:  estimatedInputTokens,
		OutputTokens: outputTokens,
	})

	if req.Stream {
		h.sendWebSearchSSE(w, req.Model, query, results, estimatedInputTokens, outputTokens)
		return
	}
	resp := KiroToClaudeResponse(output, "", false, nil, estimatedInputTokens, outputTokens, req.Model)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(resp)
}

func (h *Handler) callWebSearchMCP(ctx context.Context, model, query string) (*webSearchResults, error) {
	attempts := h.newAccountAttemptController(ctx)
	excluded := attempts.excluded
	var lastErr error
	for {
		if err := ctx.Err(); err != nil {
			return nil, classifyRequestCancellation("Kiro MCP WebSearch", err)
		}
		account, guard, busy := h.acquireNextAccountForRequest(attempts, model, "")
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
			excluded[account.ID] = true
			lastErr = err
			h.handleAccountFailure(account, err)
			if !shouldRetryAcrossAccounts(err) {
				break
			}
			continue
		}
		results, err := callMCPWebSearchContext(ctx, account, query)
		if upstreamErr, ok := asUpstreamError(err); ok && upstreamErr.RefreshToken && account.RefreshToken != "" {
			if refreshErr := sharedTokenRefreshCoordinator.RefreshContext(ctx, account, true); refreshErr == nil {
				results, err = callMCPWebSearchContext(ctx, account, query)
			} else {
				err = classifyRefreshFailure("Kiro MCP WebSearch", refreshErr)
			}
		}
		if err == nil {
			h.pool.RecordUpstreamSuccess(account.ID, account.ProfileArn, model)
		}
		release()
		if err == nil {
			h.pool.RecordSuccess(account.ID)
			return results, nil
		}
		excluded[account.ID] = true
		lastErr = err
		h.handleAccountFailureForModel(account, model, err)
		if !shouldRetryAcrossAccounts(err) {
			break
		}
	}
	if err := attempts.stopErr(); err != nil {
		return nil, classifyRequestCancellation("Kiro MCP WebSearch", err)
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("no available accounts for web_search")
}

func callMCPWebSearch(account *config.Account, query string) (*webSearchResults, error) {
	return callMCPWebSearchContext(context.Background(), account, query)
}

func webSearchRegionCandidates(account *config.Account) []string {
	regions := make([]string, 0, 2)
	seen := make(map[string]bool, 2)
	add := func(region string) {
		region = strings.TrimSpace(region)
		key := strings.ToLower(region)
		if region == "" || seen[key] {
			return
		}
		seen[key] = true
		regions = append(regions, region)
	}

	// The profile ARN identifies the Kiro data-plane region. account.Region is
	// the OIDC region and can legitimately differ for external IdP accounts.
	add(kiroRegion(account))
	add("us-east-1")
	return regions
}

func callMCPWebSearchContext(ctx context.Context, account *config.Account, query string) (*webSearchResults, error) {
	body, err := json.Marshal(mcpRequest{
		ID:      "web_search_tooluse_" + uuid.New().String(),
		JSONRPC: "2.0",
		Method:  "tools/call",
		Params: map[string]interface{}{
			"name":      "web_search",
			"arguments": map[string]string{"query": query},
		},
	})
	if err != nil {
		return nil, err
	}
	var lastErr error
	for _, region := range webSearchRegionCandidates(account) {
		results, err := callMCPWebSearchURL(ctx, account, "https://q."+region+".amazonaws.com/mcp", body, query)
		if err == nil {
			return results, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

func callMCPWebSearchURL(ctx context.Context, account *config.Account, rawURL string, body []byte, query string) (*webSearchResults, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", rawURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connection", "close")
	host := ""
	if parsed, parseErr := url.Parse(rawURL); parseErr == nil {
		host = parsed.Host
	}
	applyKiroBaseHeaders(req, account, buildRuntimeHeaderValues(account, host))
	req.Header.Set("Amz-Sdk-Request", "attempt=1; max=3")
	req.Header.Set("Amz-Sdk-Invocation-Id", uuid.New().String())
	if profileArn := strings.TrimSpace(account.ProfileArn); profileArn != "" {
		req.Header.Set("x-amzn-kiro-profile-arn", profileArn)
	}
	if isKiroAPIKeyAccount(account) {
		req.Header.Set("tokentype", "API_KEY")
	}

	client, err := GetRestClientForProxy(ResolveAccountProxyURL(account))
	if err != nil {
		return nil, classifyTransportError("Kiro MCP WebSearch", fmt.Errorf("configure outbound proxy: %w", err))
	}
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, classifyRequestCancellation("Kiro MCP WebSearch", ctx.Err())
		}
		return nil, classifyTransportError("Kiro MCP WebSearch", err)
	}
	defer resp.Body.Close()
	respBody, readErr := httpbody.ReadAll(resp.Body, httpbody.DefaultLimit)
	if readErr != nil {
		return nil, fmt.Errorf("web search response: %w", readErr)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, classifyUpstreamHTTPError(resp.StatusCode, "Kiro MCP WebSearch", respBody)
	}
	var mcp mcpResponse
	if err := json.Unmarshal(respBody, &mcp); err != nil {
		return nil, err
	}
	if mcp.Error != nil {
		return nil, fmt.Errorf("MCP error %d: %s", mcp.Error.Code, mcp.Error.Message)
	}
	if mcp.Result == nil {
		return &webSearchResults{Query: query}, nil
	}
	for _, item := range mcp.Result.Content {
		if item.Type != "text" || strings.TrimSpace(item.Text) == "" {
			continue
		}
		var results webSearchResults
		if err := json.Unmarshal([]byte(item.Text), &results); err == nil {
			if results.Query == "" {
				results.Query = query
			}
			return &results, nil
		}
	}
	return &webSearchResults{Query: query}, nil
}

func webSearchSummary(query string, results *webSearchResults) string {
	if results == nil || len(results.Results) == 0 {
		return "No web search results found for: " + query
	}
	lines := []string{"Web search results for: " + query}
	for i, item := range results.Results {
		if i >= 8 {
			break
		}
		title := strings.TrimSpace(item.Title)
		if title == "" {
			title = item.URL
		}
		line := fmt.Sprintf("%d. %s", i+1, title)
		if item.URL != "" {
			line += " - " + item.URL
		}
		if item.Snippet != "" {
			line += "\n   " + item.Snippet
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

func (h *Handler) sendWebSearchSSE(w http.ResponseWriter, model, query string, results *webSearchResults, inputTokens, outputTokens int) {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, ok := w.(http.Flusher)
	if !ok {
		h.sendClaudeError(w, 500, "api_error", "Streaming not supported")
		return
	}
	msgID := "msg_" + uuid.New().String()
	toolUseID := "web_search_tooluse_" + uuid.New().String()
	h.sendSSE(w, flusher, "message_start", map[string]interface{}{
		"type": "message_start",
		"message": map[string]interface{}{
			"id":            msgID,
			"type":          "message",
			"role":          "assistant",
			"model":         model,
			"content":       []interface{}{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         map[string]int{"input_tokens": inputTokens, "output_tokens": 0},
		},
	})
	h.sendSSE(w, flusher, "content_block_start", map[string]interface{}{
		"type":  "content_block_start",
		"index": 0,
		"content_block": map[string]interface{}{
			"type":  "tool_use",
			"id":    toolUseID,
			"name":  "web_search",
			"input": map[string]string{"query": query},
		},
	})
	h.sendSSE(w, flusher, "content_block_stop", map[string]interface{}{"type": "content_block_stop", "index": 0})
	h.sendSSE(w, flusher, "content_block_start", map[string]interface{}{
		"type":  "content_block_start",
		"index": 1,
		"content_block": map[string]interface{}{
			"type":        "web_search_tool_result",
			"tool_use_id": toolUseID,
			"content":     webSearchResultBlocks(results),
		},
	})
	h.sendSSE(w, flusher, "content_block_stop", map[string]interface{}{"type": "content_block_stop", "index": 1})
	h.sendSSE(w, flusher, "message_delta", map[string]interface{}{
		"type":  "message_delta",
		"delta": map[string]interface{}{"stop_reason": "end_turn"},
		"usage": map[string]int{"output_tokens": outputTokens},
	})
	h.sendSSE(w, flusher, "message_stop", map[string]interface{}{"type": "message_stop"})
}

func webSearchResultBlocks(results *webSearchResults) []map[string]interface{} {
	out := []map[string]interface{}{}
	if results == nil {
		return out
	}
	for _, item := range results.Results {
		block := map[string]interface{}{
			"type":  "web_search_result",
			"title": item.Title,
			"url":   item.URL,
		}
		if item.Snippet != "" {
			block["snippet"] = item.Snippet
		}
		if item.PublishedAt > 0 {
			block["published_date"] = item.PublishedAt
		}
		out = append(out, block)
	}
	return out
}
