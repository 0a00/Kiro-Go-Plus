package proxy

import (
	"encoding/json"
	"kiro-go/config"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestExposedRequestModelHidesFallbackTarget(t *testing.T) {
	payload := &KiroPayload{}
	payload.recordModelFallback("claude-opus-5", "claude-sonnet-4.5", "always")
	if got := exposedRequestModel(payload, "claude-sonnet-4.5"); got != "claude-opus-5" {
		t.Fatalf("exposed model = %q, want requested model", got)
	}
	entry := requestLogEntry{Model: exposedRequestModel(payload, "claude-sonnet-4.5"), ModelFallbackFrom: "claude-opus-5", ModelFallbackTo: "claude-sonnet-4.5", ModelFallbackRuleID: "always"}
	encoded, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("marshal request log: %v", err)
	}
	if strings.Contains(string(encoded), "modelFallback") || strings.Contains(string(encoded), "claude-sonnet-4.5") {
		t.Fatalf("fallback route leaked into request log: %s", encoded)
	}
}

func TestModelFallbackResolverHonorsKeyScopeAndMatchModes(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	keyID := "key-scope"
	if _, err := config.AddApiKey(config.ApiKeyEntry{ID: keyID, Key: "sk-scope", Enabled: true}); err != nil {
		t.Fatalf("add key: %v", err)
	}
	if err := config.UpdateModelFallbackConfig(config.ModelFallbackConfig{
		Enabled:        true,
		DefaultTrigger: config.ModelFallbackTriggerModelUnavailable,
		MaxHops:        1,
		Rules: []config.ModelFallbackRule{
			{ID: "other", Enabled: true, MatchType: config.ModelFallbackMatchExact, SourceModel: "claude-opus-5", TargetModel: "claude-haiku-4.5", APIKeyIDs: []string{"other-key"}},
			{ID: "scope", Enabled: true, MatchType: config.ModelFallbackMatchPrefix, SourceModel: "claude-", TargetModel: "claude-sonnet-4.5", APIKeyIDs: []string{keyID}},
		},
	}); err != nil {
		t.Fatalf("update fallback: %v", err)
	}

	decision, ok := resolveModelFallback("claude-opus-5-thinking", keyID, config.ModelFallbackTriggerModelUnavailable, nil)
	if !ok || decision.TargetModel != "claude-sonnet-4.5" {
		t.Fatalf("unexpected scoped decision: %+v ok=%v", decision, ok)
	}
	if _, ok := resolveModelFallback("claude-opus-5", "other-key", config.ModelFallbackTriggerModelUnavailable, nil); !ok {
		t.Fatal("exact rule for another key was not selected")
	}
	if _, ok := resolveModelFallback("claude-opus-5", "missing-key", config.ModelFallbackTriggerModelUnavailable, nil); ok {
		t.Fatal("key-scoped rule leaked to an unrelated key")
	}
}

func TestResolveRequestModelRouteHandlesUnknownAndAlwaysModes(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	if err := config.UpdateModelFallbackConfig(config.ModelFallbackConfig{
		Enabled:        true,
		DefaultTrigger: config.ModelFallbackTriggerModelUnavailable,
		MaxHops:        1,
		Rules: []config.ModelFallbackRule{
			{ID: "unknown", Enabled: true, MatchType: config.ModelFallbackMatchPrefix, SourceModel: "claude-opus-", TargetModel: "claude-sonnet-4.5"},
		},
	}); err != nil {
		t.Fatalf("update fallback: %v", err)
	}
	h := &Handler{}
	if got, _, changed := h.resolveRequestModelRoute("claude-opus-9", "claude-opus-9", ""); !changed || got != "claude-sonnet-4.5" {
		t.Fatalf("unknown model was not routed: got=%q changed=%v", got, changed)
	}

	if err := config.UpdateModelFallbackConfig(config.ModelFallbackConfig{
		Enabled:        true,
		DefaultTrigger: config.ModelFallbackTriggerAlways,
		MaxHops:        1,
		Rules: []config.ModelFallbackRule{{
			ID: "always", Enabled: true, MatchType: config.ModelFallbackMatchPrefix,
			SourceModel: "claude-opus-", TargetModel: "claude-sonnet-4.5", Trigger: config.ModelFallbackTriggerAlways,
		}},
	}); err != nil {
		t.Fatalf("update always fallback: %v", err)
	}
	if got, _, changed := h.resolveRequestModelRoute("claude-opus-5", "claude-opus-5", ""); !changed || got != "claude-sonnet-4.5" {
		t.Fatalf("always model was not routed: got=%q changed=%v", got, changed)
	}
}

func TestAlwaysFallbackUsesTargetContextWindowAcrossProtocols(t *testing.T) {
	h, cleanup := setupResponsesTestHandler(t)
	defer cleanup()
	if err := config.UpdateThinkingConfigWithToolStreamMode(
		"-thinking", "reasoning_content", "thinking", 4000, 10000, 64000, 0,
		config.ToolStreamModeBalanced, true,
	); err != nil {
		t.Fatalf("update thinking config: %v", err)
	}
	if err := config.UpdateModelFallbackConfig(config.ModelFallbackConfig{
		Enabled:        true,
		DefaultTrigger: config.ModelFallbackTriggerAlways,
		MaxHops:        1,
		Rules: []config.ModelFallbackRule{{
			ID: "context-window", Enabled: true, MatchType: config.ModelFallbackMatchExact,
			SourceModel: "claude-opus-5", TargetModel: "claude-sonnet-4.5", Trigger: config.ModelFallbackTriggerAlways,
		}},
	}); err != nil {
		t.Fatalf("update fallback: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload KiroPayload
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode upstream payload: %v", err)
		} else if model := payload.ConversationState.CurrentMessage.UserInputMessage.ModelID; model != "claude-sonnet-4.5" {
			t.Errorf("upstream model = %q, want fallback target", model)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "ok"}))
		_, _ = w.Write(awsEventStreamFrame(t, "contextUsageEvent", map[string]interface{}{"contextUsagePercentage": 10.0}))
		_, _ = w.Write(awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{"stopReason": "end_turn"}))
	}))
	defer server.Close()
	defer swapKiroEndpointsForTest(t, server)()

	tests := []struct {
		name       string
		path       string
		body       string
		usageField string
		serve      func(http.ResponseWriter, *http.Request)
	}{
		{
			name: "claude", path: "/v1/messages", usageField: "input_tokens",
			body:  `{"model":"claude-opus-5","max_tokens":256,"messages":[{"role":"user","content":"hello"}]}`,
			serve: h.handleClaudeMessages,
		},
		{
			name: "chat", path: "/v1/chat/completions", usageField: "prompt_tokens",
			body:  `{"model":"claude-opus-5","max_tokens":256,"messages":[{"role":"user","content":"hello"}]}`,
			serve: h.handleOpenAIChat,
		},
		{
			name: "responses", path: "/v1/responses", usageField: "input_tokens",
			body:  `{"model":"claude-opus-5","max_output_tokens":256,"input":"hello","store":false}`,
			serve: h.handleOpenAIResponses,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			tc.serve(rec, httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body)))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			var response struct {
				Model string                     `json:"model"`
				Usage map[string]json.RawMessage `json:"usage"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
				t.Fatalf("decode response: %v body=%s", err, rec.Body.String())
			}
			var got int
			if err := json.Unmarshal(response.Usage[tc.usageField], &got); err != nil {
				t.Fatalf("decode %s: %v body=%s", tc.usageField, err, rec.Body.String())
			}
			if got != 20_000 {
				t.Fatalf("%s = %d, want 20000 from target model's 200K context window", tc.usageField, got)
			}
			if response.Model != "claude-opus-5" {
				t.Fatalf("public model = %q, want requested model", response.Model)
			}
		})
	}

	streamTests := []struct {
		name  string
		path  string
		body  string
		serve func(http.ResponseWriter, *http.Request)
	}{
		{
			name: "claude stream", path: "/v1/messages",
			body:  `{"model":"claude-opus-5","stream":true,"max_tokens":256,"messages":[{"role":"user","content":"hello"}]}`,
			serve: h.handleClaudeMessages,
		},
		{
			name: "chat stream", path: "/v1/chat/completions",
			body:  `{"model":"claude-opus-5","stream":true,"max_tokens":256,"messages":[{"role":"user","content":"hello"}]}`,
			serve: h.handleOpenAIChat,
		},
		{
			name: "responses stream", path: "/v1/responses",
			body:  `{"model":"claude-opus-5","stream":true,"max_output_tokens":256,"input":"hello","store":false}`,
			serve: h.handleOpenAIResponses,
		},
	}
	for _, tc := range streamTests {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			tc.serve(rec, httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body)))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			body := rec.Body.String()
			if !strings.Contains(body, `claude-opus-5`) {
				t.Fatalf("requested model missing from stream: %s", body)
			}
			if strings.Contains(body, `claude-sonnet-4.5`) {
				t.Fatalf("fallback model leaked into stream: %s", body)
			}
		})
	}
}

func TestCallKiroAPIFallsBackToConfiguredModelOnUnavailable(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	if _, err := config.AddApiKey(config.ApiKeyEntry{ID: "fallback-key", Key: "sk-fallback", Enabled: true}); err != nil {
		t.Fatalf("add key: %v", err)
	}
	if err := config.UpdateModelFallbackConfig(config.ModelFallbackConfig{
		Enabled:        true,
		DefaultTrigger: config.ModelFallbackTriggerModelUnavailable,
		MaxHops:        1,
		Rules: []config.ModelFallbackRule{{
			ID: "opus-to-sonnet", Enabled: true, MatchType: config.ModelFallbackMatchExact,
			SourceModel: "claude-opus-5", TargetModel: "claude-sonnet-4.5",
		}},
	}); err != nil {
		t.Fatalf("update fallback: %v", err)
	}
	retry := config.GetRetryConfig()
	retry.MaxUpstreamAttempts = 6
	retry.MaxRetryDurationSeconds = 30
	retry.PreOutputStreamRetries = func() *int { value := 0; return &value }()
	retry.EmptyResponseRetries = 0
	if err := config.UpdateRetryConfig(retry); err != nil {
		t.Fatalf("update retry: %v", err)
	}
	if err := config.UpdatePreferredEndpoint("test"); err != nil {
		t.Fatalf("update endpoint: %v", err)
	}
	if err := config.UpdateEndpointFallback(false); err != nil {
		t.Fatalf("disable endpoint fallback: %v", err)
	}

	var requests atomic.Int32
	var models []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload KiroPayload
		_ = json.NewDecoder(r.Body).Decode(&payload)
		models = append(models, payload.ConversationState.CurrentMessage.UserInputMessage.ModelID)
		attempt := requests.Add(1)
		if attempt == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"code":"INVALID_MODEL_ID","message":"model is unavailable"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "fallback-ok"}))
		_, _ = w.Write(awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{"stopReason": "end_turn"}))
	}))
	defer server.Close()
	oldEndpoints := kiroEndpoints
	kiroEndpoints = []kiroEndpoint{{Key: "test", URL: server.URL, Name: "Test"}}
	t.Cleanup(func() { kiroEndpoints = oldEndpoints })

	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("{}"))
	payload := &KiroPayload{requestContext: withApiKeyContext(request, config.GetApiKeyEntry("fallback-key")).Context()}
	payload.ConversationState.CurrentMessage.UserInputMessage.ModelID = "claude-opus-5"
	var output strings.Builder
	err := CallKiroAPI(&config.Account{ID: "fallback-account", AccessToken: "token"}, payload, &KiroStreamCallback{
		OnText: func(text string, _ bool) { output.WriteString(text) },
	})
	if err != nil {
		t.Fatalf("fallback call failed: %v", err)
	}
	if requests.Load() != 2 || output.String() != "fallback-ok" || len(models) != 2 || models[0] != "claude-opus-5" || models[1] != "claude-sonnet-4.5" {
		t.Fatalf("unexpected fallback execution: requests=%d models=%v output=%q", requests.Load(), models, output.String())
	}
	if got := payload.ConversationState.CurrentMessage.UserInputMessage.ModelID; got != "claude-opus-5" {
		t.Fatalf("payload model was not restored: %q", got)
	}
	applied, from, to, ruleID := payload.modelFallbackInfo()
	if !applied || from != "claude-opus-5" || to != "claude-sonnet-4.5" || ruleID != "opus-to-sonnet" {
		t.Fatalf("fallback metadata missing: applied=%v from=%q to=%q rule=%q", applied, from, to, ruleID)
	}
}

func TestCallKiroAPIDoesNotFallbackWhenKeyDisablesIt(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	disabled := false
	if _, err := config.AddApiKey(config.ApiKeyEntry{ID: "disabled-fallback-key", Key: "sk-disabled", Enabled: true, ModelFallbackEnabled: &disabled}); err != nil {
		t.Fatalf("add key: %v", err)
	}
	if err := config.UpdateModelFallbackConfig(config.ModelFallbackConfig{
		Enabled: true, DefaultTrigger: config.ModelFallbackTriggerModelUnavailable, MaxHops: 1,
		Rules: []config.ModelFallbackRule{{ID: "disabled-rule", Enabled: true, MatchType: config.ModelFallbackMatchExact, SourceModel: "claude-opus-5", TargetModel: "claude-sonnet-4.5"}},
	}); err != nil {
		t.Fatalf("update fallback: %v", err)
	}
	retry := config.GetRetryConfig()
	retry.MaxUpstreamAttempts = 2
	retry.MaxRetryDurationSeconds = 10
	retry.PreOutputStreamRetries = func() *int { value := 0; return &value }()
	if err := config.UpdateRetryConfig(retry); err != nil {
		t.Fatalf("update retry: %v", err)
	}
	if err := config.UpdatePreferredEndpoint("test"); err != nil {
		t.Fatalf("update endpoint: %v", err)
	}
	if err := config.UpdateEndpointFallback(false); err != nil {
		t.Fatalf("disable endpoint fallback: %v", err)
	}
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"code":"INVALID_MODEL_ID"}`))
	}))
	defer server.Close()
	oldEndpoints := kiroEndpoints
	kiroEndpoints = []kiroEndpoint{{Key: "test", URL: server.URL, Name: "Test"}}
	t.Cleanup(func() { kiroEndpoints = oldEndpoints })
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("{}"))
	payload := &KiroPayload{requestContext: withApiKeyContext(request, config.GetApiKeyEntry("disabled-fallback-key")).Context()}
	payload.ConversationState.CurrentMessage.UserInputMessage.ModelID = "claude-opus-5"
	err := CallKiroAPI(&config.Account{ID: "disabled-fallback-account", AccessToken: "token"}, payload, &KiroStreamCallback{})
	if err == nil || requests.Load() != 1 {
		t.Fatalf("disabled key unexpectedly fell back: err=%v requests=%d", err, requests.Load())
	}
	if payload.modelFallbackApplied {
		t.Fatal("disabled key recorded a fallback")
	}
}

func TestCallKiroAPIDoesNotReplayAfterVisibleOutput(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	if err := config.UpdateModelFallbackConfig(config.ModelFallbackConfig{
		Enabled: true, DefaultTrigger: config.ModelFallbackTriggerUpstreamError, MaxHops: 1,
		Rules: []config.ModelFallbackRule{{ID: "visible-output", Enabled: true, MatchType: config.ModelFallbackMatchExact, SourceModel: "claude-opus-5", TargetModel: "claude-sonnet-4.5", Trigger: config.ModelFallbackTriggerUpstreamError}},
	}); err != nil {
		t.Fatalf("update fallback: %v", err)
	}
	retry := config.GetRetryConfig()
	retry.MaxUpstreamAttempts = 4
	retry.MaxRetryDurationSeconds = 10
	retry.PreOutputStreamRetries = func() *int { value := 0; return &value }()
	if err := config.UpdateRetryConfig(retry); err != nil {
		t.Fatalf("update retry: %v", err)
	}
	if err := config.UpdatePreferredEndpoint("test"); err != nil {
		t.Fatalf("update endpoint: %v", err)
	}
	if err := config.UpdateEndpointFallback(false); err != nil {
		t.Fatalf("disable endpoint fallback: %v", err)
	}
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "partial"}))
	}))
	defer server.Close()
	oldEndpoints := kiroEndpoints
	kiroEndpoints = []kiroEndpoint{{Key: "test", URL: server.URL, Name: "Test"}}
	t.Cleanup(func() { kiroEndpoints = oldEndpoints })
	payload := &KiroPayload{}
	payload.ConversationState.CurrentMessage.UserInputMessage.ModelID = "claude-opus-5"
	var output strings.Builder
	err := CallKiroAPI(&config.Account{ID: "visible-output-account", AccessToken: "token"}, payload, &KiroStreamCallback{
		OnText: func(text string, _ bool) { output.WriteString(text) },
	})
	if err == nil || requests.Load() != 1 || output.String() != "partial" {
		t.Fatalf("visible output was incorrectly replayed: err=%v requests=%d output=%q", err, requests.Load(), output.String())
	}
}

func TestModelFallbackAdminAPIValidatesAndReturnsConfig(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	h := &Handler{}
	get := httptest.NewRecorder()
	h.apiGetModelFallback(get, httptest.NewRequest(http.MethodGet, "/admin/api/model-fallback", nil))
	if get.Code != http.StatusOK {
		t.Fatalf("get status=%d body=%s", get.Code, get.Body.String())
	}
	var defaults config.ModelFallbackConfig
	if err := json.Unmarshal(get.Body.Bytes(), &defaults); err != nil {
		t.Fatalf("decode defaults: %v", err)
	}
	if defaults.Enabled || defaults.MaxHops != config.DefaultModelFallbackMaxHops {
		t.Fatalf("unexpected defaults: %+v", defaults)
	}

	save := httptest.NewRecorder()
	h.apiUpdateModelFallback(save, httptest.NewRequest(http.MethodPost, "/admin/api/model-fallback", strings.NewReader(`{"enabled":true,"defaultTrigger":"model_unavailable","maxHops":1,"rules":[{"id":"all-claude","enabled":true,"matchType":"prefix","sourceModel":"claude-","targetModel":"claude-sonnet-4.5"}]}`)))
	if save.Code != http.StatusOK {
		t.Fatalf("save status=%d body=%s", save.Code, save.Body.String())
	}
	if !config.GetModelFallbackConfig().Enabled || len(config.GetModelFallbackConfig().Rules) != 1 {
		t.Fatalf("fallback config was not saved: %+v", config.GetModelFallbackConfig())
	}
}
