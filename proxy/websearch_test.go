package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"kiro-go/config"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHasPureWebSearchToolOnlyMatchesSingleTool(t *testing.T) {
	native := ClaudeTool{Type: "web_search_20250305", Name: "web_search", MaxUses: 3}
	if !hasPureWebSearchTool(&ClaudeRequest{Tools: []ClaudeTool{native}}) {
		t.Fatal("expected single native web_search tool to match")
	}
	if hasPureWebSearchTool(&ClaudeRequest{Tools: []ClaudeTool{{Name: "web_search"}}}) {
		t.Fatal("custom same-name tool must not be intercepted")
	}
	if hasPureWebSearchTool(&ClaudeRequest{Tools: []ClaudeTool{native, ClaudeTool{Name: "other"}}}) {
		t.Fatal("expected multiple tools not to match")
	}
	if !hasMixedWebSearchTools(&ClaudeRequest{Tools: []ClaudeTool{native, ClaudeTool{Name: "other"}}}) {
		t.Fatal("expected native web_search among client tools to use mixed loop")
	}
	if hasMixedWebSearchTools(&ClaudeRequest{Tools: []ClaudeTool{{Name: "web_search"}, {Name: "other"}}}) {
		t.Fatal("custom same-name tool must remain on the normal client-tool path")
	}
}

func TestExtractWebSearchQueryStripsClaudePrefix(t *testing.T) {
	req := &ClaudeRequest{
		Messages: []ClaudeMessage{{
			Role:    "user",
			Content: "Perform a web search for the query: kiro proxy",
		}},
	}
	if got := extractWebSearchQuery(req); got != "kiro proxy" {
		t.Fatalf("expected query, got %q", got)
	}
}

func TestExtractWebSearchQueryPrefersToolChoiceInput(t *testing.T) {
	req := &ClaudeRequest{
		Messages: []ClaudeMessage{{
			Role:    "user",
			Content: "ignore this",
		}},
		ToolChoice: map[string]interface{}{
			"type": "tool",
			"name": "web_search",
			"input": map[string]interface{}{
				"query": "kiro web search",
			},
		},
	}
	if got := extractWebSearchQuery(req); got != "kiro web search" {
		t.Fatalf("expected tool_choice query, got %q", got)
	}
}

func TestBuildWebSearchClaudeResponseUsesServerToolBlocks(t *testing.T) {
	results := &webSearchResults{Results: []webSearchResult{{Title: "OpenAI", URL: "https://openai.com/", Snippet: "Official site", PublishedAt: 1783987200000}}}
	resp := buildWebSearchClaudeResponse("claude-sonnet-5", "openai", "summary", results, 10, 2)
	if len(resp.Content) != 3 || resp.Content[0].Type != "server_tool_use" || resp.Content[1].Type != "web_search_tool_result" || resp.Content[2].Type != "text" {
		t.Fatalf("unexpected web search response blocks: %+v", resp.Content)
	}
	if resp.Content[0].ID == "" || resp.Content[1].ToolUseID != resp.Content[0].ID || resp.Content[2].Text != "summary" {
		t.Fatalf("web search response blocks are not linked: %+v", resp.Content)
	}
	if resp.Usage.ServerToolUse == nil || resp.Usage.ServerToolUse.WebSearchRequests != 1 {
		t.Fatalf("web search usage is missing: %+v", resp.Usage)
	}
	blocks, ok := resp.Content[1].Content.([]map[string]interface{})
	if !ok || len(blocks) != 1 || blocks[0]["encrypted_content"] != "Official site" || blocks[0]["page_age"] != "July 14, 2026" {
		t.Fatalf("unexpected web search result schema: %#v", resp.Content[1].Content)
	}
}

func TestSendWebSearchSSEIncludesServerToolResultAndText(t *testing.T) {
	results := &webSearchResults{Results: []webSearchResult{{Title: "OpenAI", URL: "https://openai.com/", Snippet: "Official site"}}}
	rec := httptest.NewRecorder()
	(&Handler{}).sendWebSearchSSE(rec, "claude-sonnet-5", "openai", results, 10, 2)
	body := rec.Body.String()
	serverTool := strings.Index(body, `"type":"server_tool_use"`)
	toolResult := strings.Index(body, `"type":"web_search_tool_result"`)
	textDelta := strings.Index(body, `"type":"text_delta"`)
	if serverTool < 0 || toolResult <= serverTool || textDelta <= toolResult || !strings.Contains(body, "Web search results for: openai") || !strings.Contains(body, `"web_search_requests":1`) {
		t.Fatalf("unexpected web search SSE: %s", body)
	}
}

func TestExtractWebSearchQueryUsesLastUserMessage(t *testing.T) {
	req := &ClaudeRequest{
		Messages: []ClaudeMessage{
			{Role: "user", Content: "old query"},
			{Role: "assistant", Content: "ok"},
			{Role: "user", Content: "Perform a web search for the query: latest query"},
		},
	}
	if got := extractWebSearchQuery(req); got != "latest query" {
		t.Fatalf("expected last user query, got %q", got)
	}
}

func TestWebSearchSummaryIncludesResults(t *testing.T) {
	summary := webSearchSummary("kiro", &webSearchResults{Results: []webSearchResult{{
		Title:   "Kiro",
		URL:     "https://example.com",
		Snippet: "snippet",
	}}})
	for _, want := range []string{"Web search results for: kiro", "Kiro", "https://example.com", "snippet"} {
		if !strings.Contains(summary, want) {
			t.Fatalf("expected summary to contain %q, got %q", want, summary)
		}
	}
}

func TestWebSearchRegionCandidatesPreferProfileArnRegion(t *testing.T) {
	account := &config.Account{
		Region:     "us-east-1",
		ProfileArn: "arn:aws:codewhisperer:eu-central-1:123456789012:profile/test",
	}

	got := webSearchRegionCandidates(account)
	want := []string{"eu-central-1", "us-east-1"}
	if len(got) != len(want) {
		t.Fatalf("expected regions %v, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("expected regions %v, got %v", want, got)
		}
	}
}

func TestWebSearchRegionCandidatesDeduplicateDefaultRegion(t *testing.T) {
	account := &config.Account{
		Region:     "eu-central-1",
		ProfileArn: "arn:aws:codewhisperer:us-east-1:123456789012:profile/test",
	}

	got := webSearchRegionCandidates(account)
	if len(got) != 1 || got[0] != "us-east-1" {
		t.Fatalf("expected only the profile region, got %v", got)
	}
}

func TestWebSearchRegionCandidatesRejectInjectedProfileRegion(t *testing.T) {
	account := &config.Account{
		ProfileArn: "arn:aws:codewhisperer:attacker.example#:123456789012:profile/test",
	}
	got := webSearchRegionCandidates(account)
	if len(got) != 1 || got[0] != "us-east-1" {
		t.Fatalf("expected safe default region, got %v", got)
	}
}

func TestMCPWebSearchClassifiesRateLimit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"message":"rate limit"}`))
	}))
	defer server.Close()

	_, err := callMCPWebSearchURL(context.Background(), &config.Account{AccessToken: "token"}, server.URL, []byte(`{}`), "query")
	upstreamErr, ok := asUpstreamError(err)
	if !ok || upstreamErr.Kind != UpstreamErrorRateLimit {
		t.Fatalf("expected structured rate-limit error, got %#v", err)
	}
}

func TestMCPWebSearchHonorsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := callMCPWebSearchURL(ctx, &config.Account{AccessToken: "token"}, "http://127.0.0.1:1", []byte(`{}`), "query")
	upstreamErr, ok := asUpstreamError(err)
	if !ok || upstreamErr.Kind != UpstreamErrorCanceled || upstreamErr.RetryAcrossAccounts {
		t.Fatalf("expected non-retryable cancellation, got %#v", err)
	}
}

func TestMCPWebSearchRejectsMalformedPayload(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","result":{"content":[{"type":"text","text":"not-json"}]}}`))
	}))
	defer server.Close()

	_, err := callMCPWebSearchURL(context.Background(), &config.Account{AccessToken: "token"}, server.URL, []byte(`{}`), "query")
	if err == nil || !strings.Contains(err.Error(), "invalid payload") {
		t.Fatalf("expected malformed MCP payload error, got %v", err)
	}
	if !shouldRetryAcrossAccounts(err) {
		t.Fatalf("malformed MCP payload must retry another account: %v", err)
	}
}

func TestMCPWebSearchAcceptsExplicitEmptyResults(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","result":{"content":[{"type":"text","text":"{\"results\":[]}"}]}}`))
	}))
	defer server.Close()

	results, err := callMCPWebSearchURL(context.Background(), &config.Account{AccessToken: "token"}, server.URL, []byte(`{}`), "query")
	if err != nil {
		t.Fatalf("explicit empty results should be valid: %v", err)
	}
	if results == nil || results.Results == nil || len(results.Results) != 0 || results.Query != "query" {
		t.Fatalf("unexpected empty result payload: %#v", results)
	}
}

func TestMCPWebSearchFallsBackFromRuntimeToQAndLearnsRoute(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	_ = config.UpdatePreferredEndpoint("auto")
	_ = config.UpdateEndpointFallback(true)
	sharedAccountEndpointRoutes.reset()
	t.Cleanup(sharedAccountEndpointRoutes.reset)

	var runtimeCalls, qCalls int
	runtimeServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		runtimeCalls++
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"MCP route unavailable"}`))
	}))
	defer runtimeServer.Close()
	qServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		qCalls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","result":{"content":[{"type":"text","text":"{\"results\":[{\"title\":\"Kiro\",\"url\":\"https://kiro.dev/\",\"snippet\":\"Kiro\"}]}"}]}}`))
	}))
	defer qServer.Close()

	oldEndpoints := webSearchRouteEndpoints
	webSearchRouteEndpoints = []kiroEndpoint{
		{Key: "runtime-mcp", URL: runtimeServer.URL, Name: "Kiro Runtime MCP", RequiresProfileArn: true},
		{Key: "q-mcp", URL: qServer.URL, Name: "Kiro Q MCP"},
	}
	t.Cleanup(func() { webSearchRouteEndpoints = oldEndpoints })

	account := &config.Account{
		ID:          "websearch-fallback",
		AccessToken: "token",
		ProfileArn:  "arn:aws:codewhisperer:us-east-1:123456789012:profile/test",
	}
	for attempt := 0; attempt < 2; attempt++ {
		results, err := callMCPWebSearchContext(context.Background(), account, "kiro")
		if err != nil || results == nil || len(results.Results) != 1 || results.Results[0].Title != "Kiro" {
			t.Fatalf("web search fallback attempt %d: results=%+v err=%v", attempt+1, results, err)
		}
	}
	if runtimeCalls != 1 || qCalls != 2 {
		t.Fatalf("unexpected MCP routing calls: runtime=%d q=%d", runtimeCalls, qCalls)
	}
}

func TestMCPWebSearchFixedRuntimeWithoutProfileDoesNotUseQ(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	_ = config.UpdateEndpointFallback(false)
	sharedAccountEndpointRoutes.reset()
	t.Cleanup(sharedAccountEndpointRoutes.reset)

	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","result":{"content":[{"type":"text","text":"{\"results\":[]}"}]}}`))
	}))
	defer server.Close()
	oldEndpoints := webSearchRouteEndpoints
	webSearchRouteEndpoints = []kiroEndpoint{
		{Key: "runtime-mcp", URL: server.URL, Name: "Kiro Runtime MCP", RequiresProfileArn: true},
		{Key: "q-mcp", URL: server.URL, Name: "Kiro Q MCP"},
	}
	t.Cleanup(func() { webSearchRouteEndpoints = oldEndpoints })

	_, err := callMCPWebSearchContext(context.Background(), &config.Account{
		ID:                 "runtime-without-profile",
		AuthMethod:         "api_key",
		KiroApiKey:         "ksk_test",
		AccessToken:        "ksk_test",
		EndpointPreference: "runtime",
	}, "kiro")
	if err == nil || !strings.Contains(err.Error(), "no compatible MCP endpoint") {
		t.Fatalf("expected incompatible runtime MCP error, got %v", err)
	}
	if calls != 0 {
		t.Fatalf("fixed runtime request unexpectedly fell back to Q: calls=%d", calls)
	}
}

func TestResolveWebSearchMaxUses(t *testing.T) {
	native := func(maxUses int) ClaudeTool {
		return ClaudeTool{Type: "web_search_20250305", Name: "web_search", MaxUses: maxUses}
	}
	if got := resolveWebSearchMaxUses(nil, 7); got != 7 {
		t.Fatalf("server default = %d, want 7", got)
	}
	if got := resolveWebSearchMaxUses([]ClaudeTool{native(2)}, 7); got != 2 {
		t.Fatalf("client max_uses = %d, want 2", got)
	}
	if got := resolveWebSearchMaxUses([]ClaudeTool{native(99)}, 7); got != 7 {
		t.Fatalf("server cap = %d, want 7", got)
	}
	if got := resolveWebSearchMaxUses([]ClaudeTool{{Name: "web_search", MaxUses: 1}}, 7); got != 7 {
		t.Fatalf("custom same-name tool affected native budget: %d", got)
	}
}

func TestExecuteWebSearchLoopMixedTools(t *testing.T) {
	req := &ClaudeRequest{
		Model:    "claude-sonnet-5",
		Messages: []ClaudeMessage{{Role: "user", Content: "search, then run the client tool"}},
		Tools: []ClaudeTool{
			{Type: "web_search_20250305", Name: "web_search", MaxUses: 3},
			{Name: "Bash", Description: "run a command", InputSchema: map[string]interface{}{"type": "object"}},
		},
	}
	roundCalls := 0
	searchCalls := 0
	result, err := executeWebSearchLoop(
		context.Background(), req, 3, false, claudeThinkingResponseOptions{},
		func(working *ClaudeRequest) (*webSearchRoundOutcome, error) {
			roundCalls++
			return &webSearchRoundOutcome{
				text: "I found context and need the client action.",
				toolUses: []KiroToolUse{
					{ToolUseID: "ws-1", Name: "web_search", Input: map[string]interface{}{"query": "Kiro release"}},
					{ToolUseID: "bash-1", Name: "Bash", Input: map[string]interface{}{"command": "go test ./..."}},
				},
				inputTokens: 20, outputTokens: 10,
			}, nil
		},
		func(ctx context.Context, query string) (*webSearchExecution, error) {
			searchCalls++
			return &webSearchExecution{
				toolUseID: "srvtoolu_test", query: query,
				results: &webSearchResults{Results: []webSearchResult{{Title: "Kiro", URL: "https://example.com"}}},
			}, nil
		},
	)
	if err != nil {
		t.Fatalf("execute mixed loop: %v", err)
	}
	if roundCalls != 1 || searchCalls != 1 || result.webSearchRequests != 1 || result.stopReason != "tool_use" {
		t.Fatalf("unexpected mixed loop accounting: rounds=%d searches=%d result=%+v", roundCalls, searchCalls, result)
	}
	if len(result.content) != 4 {
		t.Fatalf("unexpected mixed content blocks: %+v", result.content)
	}
	if result.content[1].Type != "server_tool_use" || result.content[2].Type != "web_search_tool_result" || result.content[3].Type != "tool_use" {
		t.Fatalf("mixed tools were not preserved: %+v", result.content)
	}
	if result.content[2].ToolUseID != result.content[1].ID || result.content[3].Name != "Bash" {
		t.Fatalf("tool IDs or client tool were lost: %+v", result.content)
	}
}

func TestExecuteWebSearchLoopMultipleSearchesThenFinalText(t *testing.T) {
	req := &ClaudeRequest{
		Model:    "claude-sonnet-5",
		Messages: []ClaudeMessage{{Role: "user", Content: "compare two topics"}},
		Tools:    []ClaudeTool{{Type: "web_search_20250305", Name: "web_search", MaxUses: 2}},
	}
	roundCalls := 0
	searchCalls := 0
	result, err := executeWebSearchLoop(
		context.Background(), req, 2, false, claudeThinkingResponseOptions{},
		func(working *ClaudeRequest) (*webSearchRoundOutcome, error) {
			roundCalls++
			if roundCalls == 1 {
				return &webSearchRoundOutcome{toolUses: []KiroToolUse{
					{ToolUseID: "ws-a", Name: "web_search", Input: map[string]interface{}{"query": "topic a"}},
					{ToolUseID: "ws-b", Name: "web_search", Input: map[string]interface{}{"query": "topic b"}},
				}, inputTokens: 10, outputTokens: 4}, nil
			}
			if len(working.Messages) != 3 {
				t.Fatalf("search feedback was not appended before final round: %+v", working.Messages)
			}
			return &webSearchRoundOutcome{text: "comparison", inputTokens: 30, outputTokens: 6}, nil
		},
		func(ctx context.Context, query string) (*webSearchExecution, error) {
			searchCalls++
			return &webSearchExecution{
				toolUseID: "srvtoolu_" + strings.ReplaceAll(query, " ", "_"),
				results:   &webSearchResults{Results: []webSearchResult{{Title: query, URL: "https://example.com/" + strings.ReplaceAll(query, " ", "-")}}},
			}, nil
		},
	)
	if err != nil {
		t.Fatalf("execute multi-search loop: %v", err)
	}
	if roundCalls != 2 || searchCalls != 2 || result.webSearchRequests != 2 || result.stopReason != "end_turn" {
		t.Fatalf("unexpected multi-search accounting: rounds=%d searches=%d result=%+v", roundCalls, searchCalls, result)
	}
	if len(result.content) != 5 || result.content[4].Type != "text" || result.content[4].Text != "comparison" {
		t.Fatalf("unexpected final content: %+v", result.content)
	}
	for i := 0; i < 4; i += 2 {
		if result.content[i].Type != "server_tool_use" || result.content[i+1].ToolUseID != result.content[i].ID {
			t.Fatalf("search result linkage broken at %d: %+v", i, result.content)
		}
	}
}

func TestAppendWebSearchRoundSurvivesClaudeTranslation(t *testing.T) {
	req := &ClaudeRequest{
		Model:    "claude-sonnet-5",
		Messages: []ClaudeMessage{{Role: "user", Content: "search"}},
		Tools:    []ClaudeTool{{Type: "web_search_20250305", Name: "web_search"}},
	}
	round := &webSearchRoundOutcome{toolUses: []KiroToolUse{{
		ToolUseID: "toolu_search", Name: "web_search", Input: map[string]interface{}{"query": "kiro"},
	}}}
	executions := []*webSearchExecution{{
		toolUseID: "srvtoolu_search", query: "kiro",
		results: &webSearchResults{Results: []webSearchResult{{Title: "Kiro", URL: "https://example.com"}}},
	}}
	presentation := make([]ClaudeContentBlock, 0)
	appendWebSearchRound(req, round, executions, &presentation)

	payload := ClaudeToKiro(req, false)
	current := payload.ConversationState.CurrentMessage.UserInputMessage
	if current.UserInputMessageContext == nil || len(current.UserInputMessageContext.ToolResults) != 1 {
		t.Fatalf("search tool result was lost during translation: %+v", current)
	}
	if current.UserInputMessageContext.ToolResults[0].ToolUseID != "toolu_search" {
		t.Fatalf("tool result ID changed: %+v", current.UserInputMessageContext.ToolResults)
	}
	history := payload.ConversationState.History
	if len(history) == 0 || history[len(history)-1].AssistantResponseMessage == nil ||
		len(history[len(history)-1].AssistantResponseMessage.ToolUses) != 1 {
		t.Fatalf("matching assistant search tool_use was lost: %+v", history)
	}
	if len(presentation) != 2 || presentation[1].ToolUseID != presentation[0].ID {
		t.Fatalf("client presentation IDs are not linked: %+v", presentation)
	}
}

func TestExecuteWebSearchLoopEnforcesBudget(t *testing.T) {
	req := &ClaudeRequest{
		Model:    "claude-sonnet-5",
		Messages: []ClaudeMessage{{Role: "user", Content: "keep searching"}},
		Tools:    []ClaudeTool{{Type: "web_search_20250305", Name: "web_search", MaxUses: 1}},
	}
	roundCalls := 0
	searchCalls := 0
	_, err := executeWebSearchLoop(
		context.Background(), req, 1, false, claudeThinkingResponseOptions{},
		func(working *ClaudeRequest) (*webSearchRoundOutcome, error) {
			roundCalls++
			return &webSearchRoundOutcome{toolUses: []KiroToolUse{{
				ToolUseID: "ws", Name: "web_search", Input: map[string]interface{}{"query": "query"},
			}}}, nil
		},
		func(ctx context.Context, query string) (*webSearchExecution, error) {
			searchCalls++
			return &webSearchExecution{toolUseID: "srvtoolu", results: &webSearchResults{Results: []webSearchResult{}}}, nil
		},
	)
	if !errors.Is(err, errWebSearchBudgetExhausted) {
		t.Fatalf("expected budget exhaustion, got %v", err)
	}
	if roundCalls != 2 || searchCalls != 1 {
		t.Fatalf("budget allowed extra search: rounds=%d searches=%d", roundCalls, searchCalls)
	}
}

func TestExecuteWebSearchLoopHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := executeWebSearchLoop(
		ctx,
		&ClaudeRequest{Tools: []ClaudeTool{{Type: "web_search_20250305", Name: "web_search"}}},
		1,
		false,
		claudeThinkingResponseOptions{},
		func(*ClaudeRequest) (*webSearchRoundOutcome, error) {
			t.Fatal("round must not run after cancellation")
			return nil, nil
		},
		func(context.Context, string) (*webSearchExecution, error) {
			t.Fatal("search must not run after cancellation")
			return nil, nil
		},
	)
	upstreamErr, ok := asUpstreamError(err)
	if !ok || upstreamErr.Kind != UpstreamErrorCanceled {
		t.Fatalf("expected cancellation error, got %#v", err)
	}
}

func TestBuildWebSearchLoopResponsePreservesUsageAndToolIDs(t *testing.T) {
	result := &webSearchLoopResult{
		content: webSearchExecutionBlocks(&webSearchExecution{
			toolUseID: "srvtoolu_linked", query: "kiro",
			results: &webSearchResults{Results: []webSearchResult{{Title: "Kiro", URL: "https://example.com"}}},
		}),
		stopReason: "end_turn", inputTokens: 100, outputTokens: 20, webSearchRequests: 1,
	}
	response := buildWebSearchLoopResponse("claude-sonnet-5", result)
	if response == nil || response.Usage.ServerToolUse == nil || response.Usage.ServerToolUse.WebSearchRequests != 1 {
		t.Fatalf("missing loop usage: %+v", response)
	}
	if response.Content[1].ToolUseID != response.Content[0].ID {
		t.Fatalf("non-stream tool result ID mismatch: %+v", response.Content)
	}
	raw, err := json.Marshal(response)
	if err != nil || !strings.Contains(string(raw), `"tool_use_id":"srvtoolu_linked"`) {
		t.Fatalf("non-stream JSON lost tool_use_id: %s err=%v", raw, err)
	}
}

func TestWebSearchLoopSSEPreservesToolIDsAndClientTools(t *testing.T) {
	recorder := httptest.NewRecorder()
	timing := newRequestFirstContentTimer(time.Now())
	session, err := newWebSearchSSESession(context.Background(), &Handler{}, recorder, "claude-sonnet-5", 10, timing)
	if err != nil {
		t.Fatalf("new stream session: %v", err)
	}
	content := append(webSearchExecutionBlocks(&webSearchExecution{
		toolUseID: "srvtoolu_stream", query: "kiro",
		results: &webSearchResults{Results: []webSearchResult{{Title: "Kiro", URL: "https://example.com"}}},
	}), ClaudeContentBlock{Type: "tool_use", ID: "toolu_client", Name: "Bash", Input: map[string]interface{}{"command": "pwd"}})
	session.finish(content, "tool_use", 100, 12, 3, promptCacheUsage{CacheReadInputTokens: 90}, 1)
	session.close()
	body := recorder.Body.String()
	for _, want := range []string{
		`event: message_start`, `"id":"srvtoolu_stream"`, `"tool_use_id":"srvtoolu_stream"`,
		`"id":"toolu_client"`, `"name":"Bash"`, `"cache_read_input_tokens":90`, `"input_tokens":10`, `"web_search_requests":1`, `event: message_stop`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("stream missing %q: %s", want, body)
		}
	}
}
