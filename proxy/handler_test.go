package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"kiro-go/config"
	"kiro-go/logger"
	accountpool "kiro-go/pool"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestThinkingSourceReasoningFirst(t *testing.T) {
	var source thinkingStreamSource

	if !allowReasoningSource(&source) {
		t.Fatalf("expected reasoning source to be accepted first")
	}
	if source != thinkingSourceReasoningEvent {
		t.Fatalf("expected source to be reasoning, got %v", source)
	}
	if allowTagSource(&source) {
		t.Fatalf("expected tag source to be rejected after reasoning source selected")
	}
}

func TestThinkingConfigAPIUpdatesTokenDefaults(t *testing.T) {
	tempDir := t.TempDir()
	if err := config.Init(filepath.Join(tempDir, "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	t.Cleanup(func() { _ = config.Init(filepath.Join(tempDir, "reset.json")) })
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/admin/api/thinking", strings.NewReader(`{
		"suffix":"-thinking",
		"openaiFormat":"reasoning_content",
		"claudeFormat":"thinking",
		"defaultBudgetTokens":4000,
		"budgetCapTokens":10000,
		"defaultMaxOutputTokens":64000,
		"defaultContextWindowTokens":1000000,
		"bufferToolStreams":true,
		"enforceAgentToolUse":true
	}`))
	(&Handler{}).apiUpdateThinkingConfig(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	got := config.GetThinkingConfig()
	if got.DefaultMaxOutputTokens != 64000 || got.DefaultContextWindowTokens != 1000000 {
		t.Fatalf("unexpected persisted token defaults: %+v", got)
	}

	invalid := httptest.NewRecorder()
	(&Handler{}).apiUpdateThinkingConfig(invalid, httptest.NewRequest(http.MethodPost, "/admin/api/thinking", strings.NewReader(`{"defaultContextWindowTokens":512}`)))
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("expected invalid context window to return 400, got %d", invalid.Code)
	}
}

func TestServeHTTPRejectsOversizedRequestBody(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}"))
	req.ContentLength = maxRequestBodyBytes + 1
	rec := httptest.NewRecorder()
	(&Handler{}).ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d", rec.Code)
	}
}

func TestServeHTTPRejectsOversizedChunkedRequestBody(t *testing.T) {
	t.Setenv("ALLOW_UNAUTHENTICATED_API", "true")
	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(make([]byte, maxRequestBodyBytes+1)))
	req.ContentLength = -1
	rec := httptest.NewRecorder()
	(&Handler{}).ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestApiPromptCacheConfigUpdatesRuntimeTracker(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}

	h := &Handler{promptCache: newPromptCacheTrackerWithSettings(time.Hour, 1)}
	req := httptest.NewRequest(http.MethodPost, "/prompt-cache", strings.NewReader(`{
		"enabled": false,
		"namespaceMode": "account_api_key",
		"cacheReadEfficiencyMin": 0.5,
		"cacheReadEfficiencyMax": 0.6,
		"kvCacheTtlSecs": 120,
		"maxEntriesPerAccount": 123,
		"maxEntriesTotal": 456
	}`))
	rec := httptest.NewRecorder()

	h.apiUpdatePromptCache(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	got := config.GetPromptCacheConfig()
	if got.Enabled || got.NamespaceMode != config.PromptCacheNamespaceAccountAPIKey {
		t.Fatalf("expected persisted disabled/account_api_key policy, got enabled=%v namespace=%q", got.Enabled, got.NamespaceMode)
	}
	if got.CacheReadEfficiencyMin != 0.5 || got.CacheReadEfficiencyMax != 0.6 {
		t.Fatalf("expected persisted cache efficiency range 0.5-0.6, got %v-%v", got.CacheReadEfficiencyMin, got.CacheReadEfficiencyMax)
	}
	if got.KvCacheTTLSecs != 120 {
		t.Fatalf("expected persisted kvCacheTtlSecs 120, got %d", got.KvCacheTTLSecs)
	}
	if got.MaxEntriesPerAccount != 123 || got.MaxEntriesTotal != 456 {
		t.Fatalf("expected persisted cache limits 123/456, got %d/%d", got.MaxEntriesPerAccount, got.MaxEntriesTotal)
	}
	h.promptCache.settingsMu.RLock()
	defer h.promptCache.settingsMu.RUnlock()
	if h.promptCache.enabled || h.promptCache.namespaceMode != config.PromptCacheNamespaceAccountAPIKey {
		t.Fatalf("expected runtime disabled/account_api_key policy, got enabled=%v namespace=%q", h.promptCache.enabled, h.promptCache.namespaceMode)
	}
	if h.promptCache.readEfficiencyMin != 0.5 || h.promptCache.readEfficiencyMax != 0.6 {
		t.Fatalf("expected runtime read efficiency range 0.5-0.6, got %v-%v", h.promptCache.readEfficiencyMin, h.promptCache.readEfficiencyMax)
	}
	if h.promptCache.maxSupportedTTL != 120*time.Second {
		t.Fatalf("expected runtime ttl 120s, got %v", h.promptCache.maxSupportedTTL)
	}
	if h.promptCache.maxEntriesPerAccount != 123 || h.promptCache.maxEntriesTotal != 456 {
		t.Fatalf("expected runtime cache limits 123/456, got %d/%d", h.promptCache.maxEntriesPerAccount, h.promptCache.maxEntriesTotal)
	}
}

func TestApiPromptCacheReportsStatsAndClearsState(t *testing.T) {
	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	tracker := newPromptCacheTracker(time.Hour)
	var fingerprint [32]byte
	tracker.Update("acct-1", &promptCacheProfile{
		Model:            "claude-sonnet-4.5",
		TotalInputTokens: 2048,
		Breakpoints: []promptCacheBreakpoint{{
			Fingerprint:      fingerprint,
			CumulativeTokens: 2048,
			TTL:              time.Hour,
		}},
	})
	tracker.RecordUsage(promptCacheUsage{CacheReadInputTokens: 100}, true)
	h := &Handler{promptCache: tracker}

	getRecorder := httptest.NewRecorder()
	h.apiGetPromptCache(getRecorder, httptest.NewRequest(http.MethodGet, "/prompt-cache", nil))
	var response struct {
		Stats promptCacheStats `json:"stats"`
	}
	if err := json.Unmarshal(getRecorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response.Stats.Entries != 1 || response.Stats.CacheHits != 1 {
		t.Fatalf("unexpected prompt cache stats: %+v", response.Stats)
	}

	clearRecorder := httptest.NewRecorder()
	h.apiClearPromptCache(clearRecorder, httptest.NewRequest(http.MethodDelete, "/prompt-cache", nil))
	if clearRecorder.Code != http.StatusOK || tracker.entryCountValue() != 0 || tracker.Stats().TrackedRequests != 0 {
		t.Fatalf("prompt cache was not cleared: code=%d stats=%+v", clearRecorder.Code, tracker.Stats())
	}
}

func TestDueAutoRefreshAccountsFiltersAndUsesAdaptiveStaleness(t *testing.T) {
	now := time.Now()
	h := &Handler{autoRefreshFail: map[string]int64{"cooling": now.Add(time.Hour).Unix()}}
	autoRefresh := config.AutoRefreshConfig{
		IntervalMinutes:           30,
		TokenRefreshBeforeSeconds: 120,
	}
	accounts := []config.Account{
		{ID: "disabled", Enabled: false, AccessToken: "token"},
		{ID: "fresh", Enabled: true, AccessToken: "token", LastUsed: now.Unix(), LastRefresh: now.Add(-5 * time.Minute).Unix()},
		{ID: "active-stale", Enabled: true, AccessToken: "token", LastUsed: now.Unix(), LastRefresh: now.Add(-31 * time.Minute).Unix()},
		{ID: "idle-fresh", Enabled: true, AccessToken: "token", LastRefresh: now.Add(-2 * time.Hour).Unix()},
		{ID: "token-due", Enabled: true, AccessToken: "token", RefreshToken: "refresh", ExpiresAt: now.Add(time.Minute).Unix(), LastRefresh: now.Unix()},
		{ID: "cooling", Enabled: true, AccessToken: "token", LastRefresh: 0},
		{ID: "no-creds", Enabled: true},
	}

	due := h.dueAutoRefreshAccounts(accounts, autoRefresh)
	ids := make([]string, 0, len(due))
	for _, account := range due {
		ids = append(ids, account.ID)
	}
	if got := strings.Join(ids, ","); got != "token-due,active-stale" {
		t.Fatalf("unexpected due accounts: %s", got)
	}
}

func TestAutoRefreshSelectionPrioritizesExpiringTokens(t *testing.T) {
	now := time.Now()
	h := &Handler{}
	autoRefresh := config.AutoRefreshConfig{IntervalMinutes: 30, TokenRefreshBeforeSeconds: 120}
	accounts := []config.Account{
		{ID: "regular", Enabled: true, AccessToken: "token", LastUsed: now.Unix(), LastRefresh: 0},
		{ID: "expires-later", Enabled: true, AccessToken: "token", RefreshToken: "refresh", ExpiresAt: now.Add(time.Minute).Unix()},
		{ID: "missing-token", Enabled: true, RefreshToken: "refresh", LastRefresh: now.Unix()},
	}
	due := h.dueAutoRefreshAccounts(accounts, autoRefresh)
	selected := h.nextAutoRefreshAccounts(due, 2)
	if len(selected) != 2 || selected[0].ID != "missing-token" || selected[1].ID != "expires-later" {
		t.Fatalf("expected urgent token refreshes first, got %+v", selected)
	}
}

func TestRebuildCachedModelsDropsRemovedModels(t *testing.T) {
	h := &Handler{modelsByAccount: map[string][]ModelInfo{
		"a": {{ModelId: "model-a"}, {ModelId: "model-shared"}},
		"b": {{ModelId: "model-shared"}},
	}}
	h.modelsCacheMu.Lock()
	h.rebuildCachedModelsLocked()
	if len(h.cachedModels) != 2 {
		h.modelsCacheMu.Unlock()
		t.Fatalf("expected two aggregate models, got %d", len(h.cachedModels))
	}
	h.modelsByAccount["a"] = []ModelInfo{{ModelId: "model-a"}}
	h.modelsByAccount["b"] = nil
	h.rebuildCachedModelsLocked()
	models := append([]ModelInfo(nil), h.cachedModels...)
	h.modelsCacheMu.Unlock()
	if len(models) != 1 || models[0].ModelId != "model-a" {
		t.Fatalf("removed model remained in aggregate: %+v", models)
	}
}

func TestNextAutoRefreshAccountsRotatesWhenMaxPerRunSet(t *testing.T) {
	h := &Handler{}
	accounts := []config.Account{
		{ID: "a"},
		{ID: "b"},
		{ID: "c"},
		{ID: "d"},
	}

	first := h.nextAutoRefreshAccounts(accounts, 2)
	second := h.nextAutoRefreshAccounts(accounts, 2)
	third := h.nextAutoRefreshAccounts(accounts, 2)

	gotIDs := func(items []*config.Account) []string {
		out := make([]string, 0, len(items))
		for _, item := range items {
			out = append(out, item.ID)
		}
		return out
	}
	if got := strings.Join(gotIDs(first), ","); got != "a,b" {
		t.Fatalf("expected first window a,b, got %s", got)
	}
	if got := strings.Join(gotIDs(second), ","); got != "c,d" {
		t.Fatalf("expected second window c,d, got %s", got)
	}
	if got := strings.Join(gotIDs(third), ","); got != "a,b" {
		t.Fatalf("expected third window a,b, got %s", got)
	}
}

func TestApiRuntimeConfigUpdatesConfigAndLogger(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	prevLevel := logger.GetLevel()
	defer logger.SetLevel(prevLevel)
	logger.SetLevel(logger.LevelInfo)

	h := &Handler{}
	req := httptest.NewRequest(http.MethodPost, "/runtime-config", strings.NewReader(`{
		"host": "127.0.0.1",
		"port": 9090,
		"logLevel": "debug",
		"kiroVersion": "0.12.0",
		"systemVersion": "linux#6.1",
		"nodeVersion": "22.23.0"
	}`))
	rec := httptest.NewRecorder()

	h.apiUpdateRuntimeConfig(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if logger.GetLevel() != logger.LevelDebug {
		t.Fatalf("expected logger level debug, got %s", logger.LevelName(logger.GetLevel()))
	}
	got := config.GetRuntimeConfig()
	if got.Host != "127.0.0.1" || got.Port != 9090 || got.LogLevel != "debug" {
		t.Fatalf("unexpected runtime config: %+v", got)
	}

	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["restartRequired"] != true {
		t.Fatalf("expected restartRequired=true for host/port change, got %#v", body["restartRequired"])
	}
}

func TestClaudeCodeBufferedEndpointUsesRealInputTokens(t *testing.T) {
	t.Setenv("ALLOW_UNAUTHENTICATED_API", "true")
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.AddAccount(config.Account{
		ID:          "cc-account",
		Enabled:     true,
		AccessToken: "token-cc",
		ProfileArn:  "arn:aws:codewhisperer:profile/cc",
	}); err != nil {
		t.Fatalf("add account: %v", err)
	}
	if err := config.UpdatePreferredEndpoint("kiro"); err != nil {
		t.Fatalf("set preferred endpoint: %v", err)
	}
	if err := config.UpdateEndpointFallback(false); err != nil {
		t.Fatalf("disable endpoint fallback: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
			"content": "buffered ok",
		}))
		_, _ = w.Write(awsEventStreamFrame(t, "contextUsageEvent", map[string]interface{}{
			"contextUsagePercentage": 1.23,
		}))
	}))
	defer server.Close()
	defer swapKiroEndpointsForTest(t, server)()

	p := accountpool.GetPool()
	p.Reload()
	h := &Handler{
		pool:        p,
		promptCache: newPromptCacheTracker(defaultPromptCacheTTL),
		requestLog:  newRequestLog(defaultRequestLogLimit),
	}

	req := httptest.NewRequest(http.MethodPost, "/cc/v1/messages", strings.NewReader(`{
		"model":"claude-sonnet-4.5",
		"stream":true,
		"messages":[{"role":"user","content":"hi"}]
	}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "buffered ok") {
		t.Fatalf("expected buffered content, got:\n%s", body)
	}
	if !strings.Contains(body, `"input_tokens":2460`) {
		t.Fatalf("expected real input_tokens 2460 in message_start, got:\n%s", body)
	}
	if strings.Contains(body, fmt.Sprintf(`"input_tokens":%d`, estimateApproxTokens("hi"))) {
		t.Fatalf("expected buffered endpoint not to use approximate tokens, got:\n%s", body)
	}
}

func TestClaudeNonStreamRetriesNextAccountAfterPreResponseFailure(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}

	if err := config.AddAccount(config.Account{
		ID:          "first",
		Enabled:     true,
		AccessToken: "token-first",
		ProfileArn:  "arn:aws:codewhisperer:profile/first",
	}); err != nil {
		t.Fatalf("add first account: %v", err)
	}
	if err := config.AddAccount(config.Account{
		ID:          "second",
		Enabled:     true,
		AccessToken: "token-second",
		ProfileArn:  "arn:aws:codewhisperer:profile/second",
	}); err != nil {
		t.Fatalf("add second account: %v", err)
	}
	if err := config.UpdatePreferredEndpoint("kiro"); err != nil {
		t.Fatalf("set preferred endpoint: %v", err)
	}
	if err := config.UpdateEndpointFallback(false); err != nil {
		t.Fatalf("disable endpoint fallback: %v", err)
	}

	requestTokens := make([]string, 0, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		requestTokens = append(requestTokens, token)
		// Fail the first attempted account (whichever it is) so the handler
		// is forced to add it to `excluded` and retry the other one.
		if len(requestTokens) == 1 {
			http.Error(w, "temporary upstream failure", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
			"content": "retried successfully",
		}))
	}))
	defer server.Close()

	oldEndpoints := kiroEndpoints
	kiroEndpoints = []kiroEndpoint{{
		URL:    server.URL,
		Origin: "AI_EDITOR",
		Name:   "test",
	}}
	defer func() { kiroEndpoints = oldEndpoints }()

	oldClient := kiroHttpStore.Load()
	kiroHttpStore.Store(&http.Client{Timeout: time.Second, Transport: &http.Transport{}})
	defer kiroHttpStore.Store(oldClient)

	p := accountpool.GetPool()
	p.Reload()
	h := &Handler{
		pool:        p,
		promptCache: newPromptCacheTracker(defaultPromptCacheTTL),
	}

	payload := &KiroPayload{}
	payload.ConversationState.CurrentMessage.UserInputMessage = KiroUserInputMessage{
		Content: "hello",
		ModelID: "claude-sonnet-4.5",
		Origin:  "AI_EDITOR",
	}

	rec := httptest.NewRecorder()
	h.handleClaudeNonStream(rec, payload, "claude-sonnet-4.5", false, claudeThinkingResponseOptions{}, 1, nil, "", "")

	if rec.Code != http.StatusOK {
		t.Fatalf("expected retry to succeed, status=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(requestTokens) != 2 {
		t.Fatalf("expected two account attempts, got %v", requestTokens)
	}
	if requestTokens[0] == requestTokens[1] {
		t.Fatalf("expected first account to be excluded before retry, got %v", requestTokens)
	}
	tokenSet := map[string]bool{requestTokens[0]: true, requestTokens[1]: true}
	if !tokenSet["token-first"] || !tokenSet["token-second"] {
		t.Fatalf("expected both accounts to be tried, got %v", requestTokens)
	}

	var resp ClaudeResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Content) == 0 || resp.Content[0].Text != "retried successfully" {
		t.Fatalf("expected retried response content, got %#v", resp.Content)
	}
}

func TestClaudeNonStreamDisablesSuspendedAccountsAndKeepsTrying(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}

	for i := 1; i <= 4; i++ {
		id := fmt.Sprintf("acc-%d", i)
		if err := config.AddAccount(config.Account{
			ID:          id,
			Email:       id + "@example.test",
			Enabled:     true,
			AccessToken: "token-" + id,
			ProfileArn:  "arn:aws:codewhisperer:profile/" + id,
		}); err != nil {
			t.Fatalf("add account %s: %v", id, err)
		}
	}
	if err := config.UpdatePreferredEndpoint("kiro"); err != nil {
		t.Fatalf("set preferred endpoint: %v", err)
	}
	if err := config.UpdateEndpointFallback(false); err != nil {
		t.Fatalf("disable endpoint fallback: %v", err)
	}

	requestTokens := make([]string, 0, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		requestTokens = append(requestTokens, token)
		if len(requestTokens) < 4 {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"Your User ID temporarily is suspended.","reason":null}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
			"content": "usable account",
		}))
	}))
	defer server.Close()
	defer swapKiroEndpointsForTest(t, server)()

	p := accountpool.GetPool()
	p.Reload()
	h := &Handler{
		pool:        p,
		promptCache: newPromptCacheTracker(defaultPromptCacheTTL),
	}

	payload := &KiroPayload{}
	payload.ConversationState.CurrentMessage.UserInputMessage = KiroUserInputMessage{
		Content: "hello",
		ModelID: "claude-sonnet-4.5",
		Origin:  "AI_EDITOR",
	}

	rec := httptest.NewRecorder()
	h.handleClaudeNonStream(rec, payload, "claude-sonnet-4.5", false, claudeThinkingResponseOptions{}, 1, nil, "", "")

	if rec.Code != http.StatusOK {
		t.Fatalf("expected retry to reach usable account, status=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(requestTokens) != 4 {
		t.Fatalf("expected four account attempts, got %v", requestTokens)
	}
	accounts := config.GetAccounts()
	banned := 0
	for _, account := range accounts {
		if account.BanStatus == "BANNED" {
			banned++
		}
	}
	if banned != 3 {
		t.Fatalf("expected first three suspended accounts to be banned, got %d", banned)
	}
}

func TestThinkingSourceTagFirst(t *testing.T) {
	var source thinkingStreamSource

	if !allowTagSource(&source) {
		t.Fatalf("expected tag source to be accepted first")
	}
	if source != thinkingSourceTagBlock {
		t.Fatalf("expected source to be tag, got %v", source)
	}
	if allowReasoningSource(&source) {
		t.Fatalf("expected reasoning source to be rejected after tag source selected")
	}
}

func TestThinkingSourceSameSourceRemainsAllowed(t *testing.T) {
	var source thinkingStreamSource

	if !allowTagSource(&source) {
		t.Fatalf("expected initial tag source selection to succeed")
	}
	if !allowTagSource(&source) {
		t.Fatalf("expected repeated tag source selection to stay allowed")
	}

	source = thinkingSourceUnknown
	if !allowReasoningSource(&source) {
		t.Fatalf("expected initial reasoning source selection to succeed")
	}
	if !allowReasoningSource(&source) {
		t.Fatalf("expected repeated reasoning source selection to stay allowed")
	}
}

func TestValidateOpenAIRequestShapeRejectsAssistantPrefill(t *testing.T) {
	req := &OpenAIRequest{
		Messages: []OpenAIMessage{
			{Role: "user", Content: "hello"},
			{Role: "assistant", Content: "prefill"},
		},
	}

	if msg := validateOpenAIRequestShape(req); msg == "" {
		t.Fatalf("expected assistant-prefill final message to be rejected")
	}
}

func TestValidateOpenAIRequestShapeAllowsToolResultFinalTurn(t *testing.T) {
	req := &OpenAIRequest{
		Messages: []OpenAIMessage{
			{Role: "user", Content: "find weather"},
			{
				Role: "assistant",
				ToolCalls: []ToolCall{{
					ID:   "call_1",
					Type: "function",
					Function: struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					}{Name: "get_weather", Arguments: "{}"},
				}},
			},
			{Role: "tool", ToolCallID: "call_1", Content: "sunny"},
		},
	}

	if msg := validateOpenAIRequestShape(req); msg != "" {
		t.Fatalf("expected tool-result final turn to be valid, got %q", msg)
	}
}

func TestValidateClaudeRequestShapeRejectsAssistantPrefill(t *testing.T) {
	req := &ClaudeRequest{
		Messages: []ClaudeMessage{
			{Role: "user", Content: "hello"},
			{Role: "assistant", Content: "prefill"},
		},
	}

	if msg := validateClaudeRequestShape(req); msg == "" {
		t.Fatalf("expected assistant-prefill final message to be rejected")
	}
}

func TestResolveClaudeThinkingModeHonorsRequestThinking(t *testing.T) {
	tests := []struct {
		name         string
		model        string
		thinking     *ClaudeThinkingConfig
		wantModel    string
		wantThinking bool
	}{
		{
			name:         "adaptive request enables thinking",
			model:        "claude-sonnet-4.6",
			thinking:     &ClaudeThinkingConfig{Type: "adaptive"},
			wantModel:    "claude-sonnet-4.6",
			wantThinking: true,
		},
		{
			name:         "enabled request enables thinking",
			model:        "claude-opus-4.5",
			thinking:     &ClaudeThinkingConfig{Type: "enabled", BudgetTokens: 2048},
			wantModel:    "claude-opus-4.5",
			wantThinking: true,
		},
		{
			name:         "disabled request keeps thinking off",
			model:        "claude-opus-4.7",
			thinking:     &ClaudeThinkingConfig{Type: "disabled"},
			wantModel:    "claude-opus-4.7",
			wantThinking: false,
		},
		{
			name:         "suffix remains supported when thinking is disabled",
			model:        "claude-sonnet-4.5-thinking",
			thinking:     &ClaudeThinkingConfig{Type: "disabled"},
			wantModel:    "claude-sonnet-4.5",
			wantThinking: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotModel, gotThinking := resolveClaudeThinkingMode(tc.model, tc.thinking, "-thinking")
			if gotModel != tc.wantModel {
				t.Fatalf("expected model %q, got %q", tc.wantModel, gotModel)
			}
			if gotThinking != tc.wantThinking {
				t.Fatalf("expected thinking=%v, got %v", tc.wantThinking, gotThinking)
			}
		})
	}
}

func TestCloneClaudeRequestForThinkingInjectsPromptWithoutMutatingOriginal(t *testing.T) {
	req := &ClaudeRequest{
		Model:  "claude-sonnet-4.6",
		System: "Follow the user instructions.",
	}

	cloned := cloneClaudeRequestForThinking(req, true)
	blocks, ok := cloned.System.([]interface{})
	if !ok {
		t.Fatalf("expected cloned system prompt to be structured blocks, got %T", cloned.System)
	}
	if len(blocks) != 2 {
		t.Fatalf("expected 2 system blocks after prepend, got %d", len(blocks))
	}
	gotPrompt := extractSystemPrompt(cloned.System)
	expected := ThinkingModePrompt + "\n\nFollow the user instructions."
	if gotPrompt != expected {
		t.Fatalf("expected injected system prompt %q, got %q", expected, gotPrompt)
	}
	if original, ok := req.System.(string); !ok || original != "Follow the user instructions." {
		t.Fatalf("expected original request system prompt to stay unchanged, got %#v", req.System)
	}
}

func TestCloneClaudeRequestForThinkingPreservesStructuredSystemBlocks(t *testing.T) {
	req := &ClaudeRequest{
		Model: "claude-sonnet-4.6",
		System: []interface{}{
			map[string]interface{}{
				"type": "text",
				"text": "cached system",
				"cache_control": map[string]interface{}{
					"type": "ephemeral",
					"ttl":  "5m",
				},
			},
		},
	}

	cloned := cloneClaudeRequestForThinking(req, true)
	blocks, ok := cloned.System.([]interface{})
	if !ok {
		t.Fatalf("expected structured system blocks, got %T", cloned.System)
	}
	if len(blocks) != 2 {
		t.Fatalf("expected 2 system blocks after prepend, got %d", len(blocks))
	}
	first, ok := blocks[0].(map[string]interface{})
	if !ok || first["text"] != ThinkingModePrompt+"\n" {
		t.Fatalf("expected first block to be thinking prompt, got %#v", blocks[0])
	}
	second, ok := blocks[1].(map[string]interface{})
	if !ok {
		t.Fatalf("expected original system block to remain a map, got %T", blocks[1])
	}
	cacheControl, ok := second["cache_control"].(map[string]interface{})
	if !ok || cacheControl["type"] != "ephemeral" {
		t.Fatalf("expected original cache_control to be preserved, got %#v", second["cache_control"])
	}
}

func TestThinkingPromptAffectsClaudeTokenEstimate(t *testing.T) {
	req := &ClaudeRequest{
		Model:    "claude-sonnet-4.6",
		Messages: []ClaudeMessage{{Role: "user", Content: "hello"}},
	}

	baseTokens := estimateClaudeRequestInputTokens(req)
	thinkingTokens := estimateClaudeRequestInputTokens(cloneClaudeRequestForThinking(req, true))

	if thinkingTokens <= baseTokens {
		t.Fatalf("expected thinking tokens (%d) to exceed base tokens (%d)", thinkingTokens, baseTokens)
	}
}

func TestValidateClaudeThinkingConfig(t *testing.T) {
	tests := []struct {
		name        string
		thinking    *ClaudeThinkingConfig
		maxTokens   int
		expectError bool
	}{
		{
			name:        "adaptive is valid",
			thinking:    &ClaudeThinkingConfig{Type: "adaptive"},
			maxTokens:   4096,
			expectError: false,
		},
		{
			name:        "enabled requires budget",
			thinking:    &ClaudeThinkingConfig{Type: "enabled"},
			maxTokens:   4096,
			expectError: true,
		},
		{
			name:        "enabled requires at least 1024 budget tokens",
			thinking:    &ClaudeThinkingConfig{Type: "enabled", BudgetTokens: 512},
			maxTokens:   4096,
			expectError: true,
		},
		{
			name:        "enabled rejects max tokens zero",
			thinking:    &ClaudeThinkingConfig{Type: "enabled", BudgetTokens: 2048},
			maxTokens:   0,
			expectError: true,
		},
		{
			name:        "enabled budget must stay below max tokens",
			thinking:    &ClaudeThinkingConfig{Type: "enabled", BudgetTokens: 4096},
			maxTokens:   4096,
			expectError: true,
		},
		{
			name:        "disabled rejects display",
			thinking:    &ClaudeThinkingConfig{Type: "disabled", Display: "summarized"},
			maxTokens:   4096,
			expectError: true,
		},
		{
			name:        "missing type is rejected",
			thinking:    &ClaudeThinkingConfig{},
			maxTokens:   4096,
			expectError: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			errMsg := validateClaudeThinkingConfig(tc.thinking, tc.maxTokens)
			if tc.expectError && errMsg == "" {
				t.Fatalf("expected validation error")
			}
			if !tc.expectError && errMsg != "" {
				t.Fatalf("expected thinking config to be valid, got %q", errMsg)
			}
		})
	}
}

func TestResolveClaudeThinkingResponseOptions(t *testing.T) {
	tests := []struct {
		name       string
		thinking   *ClaudeThinkingConfig
		defaultFmt string
		wantFmt    string
		wantOmit   bool
	}{
		{
			name:       "default config is preserved when display unset",
			thinking:   &ClaudeThinkingConfig{Type: "enabled", BudgetTokens: 2048},
			defaultFmt: "think",
			wantFmt:    "think",
			wantOmit:   false,
		},
		{
			name:       "summarized forces official thinking blocks",
			thinking:   &ClaudeThinkingConfig{Type: "adaptive", Display: "summarized"},
			defaultFmt: "reasoning_content",
			wantFmt:    "thinking",
			wantOmit:   false,
		},
		{
			name:       "omitted forces official thinking blocks and hides content",
			thinking:   &ClaudeThinkingConfig{Type: "adaptive", Display: "omitted"},
			defaultFmt: "think",
			wantFmt:    "thinking",
			wantOmit:   true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			opts := resolveClaudeThinkingResponseOptions(tc.thinking, tc.defaultFmt)
			if opts.Format != tc.wantFmt {
				t.Fatalf("expected format %q, got %q", tc.wantFmt, opts.Format)
			}
			if opts.OmitDisplay != tc.wantOmit {
				t.Fatalf("expected omitDisplay=%v, got %v", tc.wantOmit, opts.OmitDisplay)
			}
		})
	}
}

func TestMergeUniqueModelsPreservesUnionAcrossAccounts(t *testing.T) {
	base := []ModelInfo{
		{ModelId: "claude-sonnet-4.5", InputTypes: []string{"TEXT"}},
	}
	incoming := []ModelInfo{
		{ModelId: "claude-sonnet-4.5", InputTypes: []string{"image"}},
		{ModelId: "claude-opus-4-7", InputTypes: []string{"text"}},
	}

	merged := mergeUniqueModels(base, incoming)
	if len(merged) != 2 {
		t.Fatalf("expected 2 unique models, got %d", len(merged))
	}
	if !modelSupportsImage(merged[0].InputTypes) {
		t.Fatalf("expected merged input types to preserve image capability, got %#v", merged[0].InputTypes)
	}
	if merged[1].ModelId != "claude-opus-4-7" {
		t.Fatalf("expected second model to be claude-opus-4-7, got %q", merged[1].ModelId)
	}
}

func TestBuildAnthropicModelsResponseGeneratesThinkingVariants(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	models := buildAnthropicModelsResponse([]ModelInfo{{
		ModelId:    "claude-sonnet-4.5",
		InputTypes: []string{"text", "image"},
		TokenLimits: &ModelTokenLimits{
			MaxInputTokens:  200000,
			MaxOutputTokens: 64000,
		},
	}}, "-thinking")

	if len(models) != 2 {
		t.Fatalf("expected base model and thinking variant, got %d", len(models))
	}
	if models[0]["id"] != "claude-sonnet-4.5" {
		t.Fatalf("unexpected base model id: %#v", models[0]["id"])
	}
	if models[1]["id"] != "claude-sonnet-4.5-thinking" {
		t.Fatalf("unexpected thinking model id: %#v", models[1]["id"])
	}
	if supportsImage, ok := models[0]["supports_image"].(bool); !ok || !supportsImage {
		t.Fatalf("expected image capability to be preserved, got %#v", models[0]["supports_image"])
	}
	if models[0]["context_window"] != 200000 || models[0]["max_output_tokens"] != 64000 {
		t.Fatalf("expected upstream token limits, got %#v", models[0])
	}
}
