package proxy

import (
	"context"
	"kiro-go/config"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHasPureWebSearchToolOnlyMatchesSingleTool(t *testing.T) {
	if !hasPureWebSearchTool(&ClaudeRequest{Tools: []ClaudeTool{{Name: "web_search"}}}) {
		t.Fatal("expected single web_search tool to match")
	}
	if !hasPureWebSearchTool(&ClaudeRequest{Tools: []ClaudeTool{{Name: "web_search_20250305"}}}) {
		t.Fatal("expected dated web_search tool to match")
	}
	if hasPureWebSearchTool(&ClaudeRequest{Tools: []ClaudeTool{{Name: "web_search"}, {Name: "other"}}}) {
		t.Fatal("expected multiple tools not to match")
	}
	if hasPureWebSearchTool(&ClaudeRequest{Tools: []ClaudeTool{{Name: "other"}}}) {
		t.Fatal("expected other tool not to match")
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
			"name": "web_search_20250305",
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
