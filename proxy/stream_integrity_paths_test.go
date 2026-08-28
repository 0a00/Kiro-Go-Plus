package proxy

import (
	"encoding/json"
	"kiro-go/config"
	accountpool "kiro-go/pool"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func setupStreamIntegrityPathTest(t *testing.T, server *httptest.Server) *Handler {
	t.Helper()
	t.Setenv("ALLOW_UNAUTHENTICATED_API", "true")
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.AddAccount(config.Account{
		ID:          "integrity-test-account",
		Enabled:     true,
		AccessToken: "integrity-test-token",
		ProfileArn:  "arn:aws:codewhisperer:profile/integrity-test",
	}); err != nil {
		t.Fatalf("add account: %v", err)
	}
	if err := config.UpdatePreferredEndpoint("kiro"); err != nil {
		t.Fatalf("set endpoint: %v", err)
	}
	if err := config.UpdateEndpointFallback(false); err != nil {
		t.Fatalf("disable endpoint fallback: %v", err)
	}
	retry := config.GetRetryConfig()
	retry.MaxAccountAttempts = 1
	retry.MaxUpstreamAttempts = 12
	retry.MaxRetryDurationSeconds = 30
	preOutputRetries := 0
	retry.PreOutputStreamRetries = &preOutputRetries
	if err := config.UpdateRetryConfig(retry); err != nil {
		t.Fatalf("set retry config: %v", err)
	}

	sharedAccountEndpointRoutes.reset()
	t.Cleanup(sharedAccountEndpointRoutes.reset)
	oldHealth := sharedUpstreamHealth
	sharedUpstreamHealth = newUpstreamHealthRegistry()
	t.Cleanup(func() { sharedUpstreamHealth = oldHealth })
	oldEndpoints := kiroEndpoints
	oldClient := kiroHttpStore.Load()
	kiroEndpoints = []kiroEndpoint{{
		Key: "kiro", URL: server.URL, Origin: "AI_EDITOR", Name: "integrity-test",
	}}
	kiroHttpStore.Store(&http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{}})
	t.Cleanup(func() {
		kiroEndpoints = oldEndpoints
		kiroHttpStore.Store(oldClient)
	})

	p := accountpool.GetPool()
	p.Reload()
	return &Handler{
		pool:        p,
		promptCache: newPromptCacheTracker(defaultPromptCacheTTL),
		requestLog:  newRequestLog(defaultRequestLogLimit),
	}
}

func writeIntegrityText(t *testing.T, w http.ResponseWriter, text string, complete bool) {
	t.Helper()
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": text}))
	if complete {
		_, _ = w.Write(awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{"stopReason": "end_turn"}))
	}
}

func TestClaudeNonStreamRetriesTruncatedResponseOnSameAccount(t *testing.T) {
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			writeIntegrityText(t, w, "discarded partial answer", false)
			return
		}
		writeIntegrityText(t, w, "recovered answer", true)
	}))
	defer upstream.Close()
	h := setupStreamIntegrityPathTest(t, upstream)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"claude-sonnet-4.5",
		"max_tokens":128,
		"messages":[{"role":"user","content":"hello"}]
	}`))
	h.handleClaudeMessages(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected recovered 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var response ClaudeResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v body=%s", err, rec.Body.String())
	}
	if len(response.Content) == 0 || !strings.Contains(response.Content[0].Text, "recovered answer") {
		t.Fatalf("missing recovered answer: %+v", response)
	}
	if strings.Contains(rec.Body.String(), "discarded partial answer") || hits.Load() != 2 {
		t.Fatalf("truncated attempt leaked or was not retried: hits=%d body=%s", hits.Load(), rec.Body.String())
	}
}

func TestOpenAIAndResponsesNonStreamDiscardTruncatedAttempt(t *testing.T) {
	for _, tc := range []struct {
		name string
		path string
		body string
		text string
	}{
		{
			name: "openai chat",
			path: "/v1/chat/completions",
			body: `{"model":"claude-sonnet-4.5","messages":[{"role":"user","content":"hello"}]}`,
			text: "chat recovered",
		},
		{
			name: "responses",
			path: "/v1/responses",
			body: `{"model":"claude-sonnet-4.5","input":"hello","store":false}`,
			text: "responses recovered",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var hits atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if hits.Add(1) == 1 {
					writeIntegrityText(t, w, "discarded partial answer", false)
					return
				}
				writeIntegrityText(t, w, tc.text, true)
			}))
			defer upstream.Close()
			h := setupStreamIntegrityPathTest(t, upstream)

			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body)))
			if rec.Code != http.StatusOK {
				t.Fatalf("expected recovered 200, got %d body=%s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.text) || strings.Contains(rec.Body.String(), "discarded partial answer") {
				t.Fatalf("unexpected recovered response: %s", rec.Body.String())
			}
			if hits.Load() != 2 {
				t.Fatalf("expected one same-account retry, hits=%d", hits.Load())
			}
		})
	}
}

func TestClaudeStreamDoesNotReplayAfterActionableOutput(t *testing.T) {
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		writeIntegrityText(t, w, "visible partial answer with enough substantive text to flush through the Claude streaming parser", false)
	}))
	defer upstream.Close()
	h := setupStreamIntegrityPathTest(t, upstream)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"claude-sonnet-4.5",
		"stream":true,
		"messages":[{"role":"user","content":"hello"}]
	}`))
	h.handleClaudeMessages(rec, req)
	body := rec.Body.String()
	if hits.Load() != 1 {
		t.Fatalf("stream was replayed after visible output, hits=%d", hits.Load())
	}
	if !strings.Contains(body, "visible partial answer") || !strings.Contains(body, `"type":"error"`) {
		t.Fatalf("expected partial output followed by SSE error: %s", body)
	}
	if strings.Contains(body, `"stop_reason":"end_turn"`) || strings.Contains(body, "message_stop") {
		t.Fatalf("truncated stream was reported as normal completion: %s", body)
	}
}

func TestClaudePrecommitThinkingRetryClosesProvisionalBlock(t *testing.T) {
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(awsEventStreamFrame(t, "reasoningContentEvent", map[string]interface{}{"text": "provisional reasoning"}))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "final answer after retry"}))
		_, _ = w.Write(awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{"stopReason": "end_turn"}))
	}))
	defer upstream.Close()
	h := setupStreamIntegrityPathTest(t, upstream)
	payload := &KiroPayload{}
	payload.requestContext = httptest.NewRequest(http.MethodPost, "/", nil).Context()
	payload.ConversationState.CurrentMessage.UserInputMessage.ModelID = "claude-sonnet-4.5"
	payload.requireActionableOutput = true
	payload.streamThinkingPrecommit = true

	rec := httptest.NewRecorder()
	h.handleClaudeStream(rec, payload, "claude-sonnet-4.5", true, claudeThinkingResponseOptions{Format: "thinking"}, 10, nil, "", "integrity-test-route")
	body := rec.Body.String()
	if hits.Load() != 2 {
		t.Fatalf("expected same-account retry after provisional thinking, hits=%d body=%s", hits.Load(), body)
	}
	if !strings.Contains(body, "final answer after retry") || !strings.Contains(body, "message_stop") {
		t.Fatalf("recovered Claude stream is incomplete: %s", body)
	}
	if starts, stops := strings.Count(body, "content_block_start"), strings.Count(body, "content_block_stop"); starts != stops {
		t.Fatalf("provisional content block was not closed: starts=%d stops=%d body=%s", starts, stops, body)
	}
}

func TestStreamIntegrityRetryHonorsUpstreamAttemptBudget(t *testing.T) {
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		writeIntegrityText(t, w, "partial", false)
	}))
	defer upstream.Close()
	h := setupStreamIntegrityPathTest(t, upstream)
	retry := config.GetRetryConfig()
	retry.MaxUpstreamAttempts = 1
	if err := config.UpdateRetryConfig(retry); err != nil {
		t.Fatalf("set attempt budget: %v", err)
	}

	rec := httptest.NewRecorder()
	h.handleClaudeMessages(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"claude-sonnet-4.5",
		"max_tokens":64,
		"messages":[{"role":"user","content":"hello"}]
	}`)))
	if hits.Load() != 1 {
		t.Fatalf("integrity retry bypassed the request budget, hits=%d", hits.Load())
	}
}
