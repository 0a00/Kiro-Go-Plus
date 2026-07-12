package proxy

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"kiro-go/config"
	accountpool "kiro-go/pool"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func configureResponsesEncryption(t *testing.T) {
	t.Helper()
	t.Setenv("KIRO_MASTER_KEY_FILE", "")
	t.Setenv("KIRO_MASTER_KEY", base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef")))
}

func TestResponsesParseStringInput(t *testing.T) {
	raw := json.RawMessage(`"hello world"`)
	msgs, err := parseResponsesInput(raw)
	if err != nil {
		t.Fatalf("parse string input: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	if msgs[0].Role != "user" {
		t.Fatalf("expected user role, got %q", msgs[0].Role)
	}
	if got, _ := msgs[0].Content.(string); got != "hello world" {
		t.Fatalf("expected hello world, got %v", msgs[0].Content)
	}
}

func TestResponsesParseArrayInput(t *testing.T) {
	raw := json.RawMessage(`[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"first"}]},
		{"type":"input_text","text":"loose part"},
		{"type":"function_call_output","call_id":"call_1","output":"42"}
	]`)
	msgs, err := parseResponsesInput(raw)
	if err != nil {
		t.Fatalf("parse array input: %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("expected 3 messages, got %d (msgs=%+v)", len(msgs), msgs)
	}
	if msgs[0].Role != "user" {
		t.Fatalf("expected first message user, got %q", msgs[0].Role)
	}
	if got, _ := msgs[0].Content.(string); got != "first" {
		t.Fatalf("expected first text, got %v", msgs[0].Content)
	}
	if msgs[2].Role != "tool" || msgs[2].ToolCallID != "call_1" {
		t.Fatalf("expected tool result with call_id call_1, got %+v", msgs[2])
	}
	if got, _ := msgs[2].Content.(string); got != "42" {
		t.Fatalf("expected tool output 42, got %v", msgs[2].Content)
	}
}

func TestResponsesRejectsMalformedAndUnknownInputItems(t *testing.T) {
	for _, raw := range []json.RawMessage{
		json.RawMessage(`[1]`),
		json.RawMessage(`[{"type":"unknown"}]`),
		json.RawMessage(`[{"type":"function_call_output","output":"missing id"}]`),
	} {
		if _, err := parseResponsesInput(raw); err == nil {
			t.Fatalf("expected input to be rejected: %s", raw)
		}
	}
}

func TestResponsesStoreAndLoad(t *testing.T) {
	configureResponsesEncryption(t)
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}

	resp := &ResponsesObject{
		ID:        "resp_unit_test_001",
		Object:    "response",
		CreatedAt: time.Now().Unix(),
		Status:    "completed",
		Model:     "claude-sonnet-4.5",
		Output: []ResponseOutputItem{{
			ID:   "msg_x",
			Type: "message",
			Role: "assistant",
			Content: []ResponseContentPart{{
				Type: "output_text",
				Text: "stored hello",
			}},
		}},
		StoredInput: json.RawMessage(`"hi"`),
	}

	if err := saveResponse(resp); err != nil {
		t.Fatalf("save: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(responsesDir(), sanitizeResponseID(resp.ID)+".json"))
	if err != nil {
		t.Fatalf("read encrypted response: %v", err)
	}
	if strings.Contains(string(raw), "stored hello") || strings.Contains(string(raw), `"hi"`) {
		t.Fatalf("response content was stored in plaintext: %s", raw)
	}
	if !strings.Contains(string(raw), `"encryption_version": 1`) {
		t.Fatalf("expected encrypted response envelope, got %s", raw)
	}

	loaded, err := loadResponse(resp.ID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded.ID != resp.ID || loaded.Model != resp.Model {
		t.Fatalf("loaded mismatch: %+v", loaded)
	}
	if len(loaded.Output) != 1 || loaded.Output[0].Content[0].Text != "stored hello" {
		t.Fatalf("loaded output mismatch: %+v", loaded.Output)
	}
	if string(loaded.StoredInput) != `"hi"` {
		t.Fatalf("stored input mismatch: %s", string(loaded.StoredInput))
	}

	if _, err := loadResponse("does_not_exist"); err == nil {
		t.Fatalf("expected load error for missing id")
	}
}

func TestResponsesStoreRequiresMasterKey(t *testing.T) {
	t.Setenv("KIRO_MASTER_KEY_FILE", "")
	t.Setenv("KIRO_MASTER_KEY", "")
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"test","input":"private","store":true}`))
	(&Handler{}).handleOpenAIResponses(recorder, request)
	if recorder.Code != http.StatusServiceUnavailable || !strings.Contains(recorder.Body.String(), "KIRO_MASTER_KEY") {
		t.Fatalf("expected storage key error, code=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestResponsesLegacyPlaintextMigratesOnRead(t *testing.T) {
	configureResponsesEncryption(t)
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	doc := storedResponseDoc{
		ID: "resp_legacy_plaintext", Object: "response", Status: "completed", Model: "test",
		StoredAt: time.Now().Unix(), StoredInput: json.RawMessage(`"legacy private prompt"`),
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatalf("marshal legacy response: %v", err)
	}
	if err := os.MkdirAll(responsesDir(), 0o700); err != nil {
		t.Fatalf("mkdir responses: %v", err)
	}
	path := filepath.Join(responsesDir(), sanitizeResponseID(doc.ID)+".json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write legacy response: %v", err)
	}
	if _, err := loadResponse(doc.ID); err != nil {
		t.Fatalf("load legacy response: %v", err)
	}
	migrated, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read migrated response: %v", err)
	}
	if strings.Contains(string(migrated), "legacy private prompt") || !strings.Contains(string(migrated), `"encryption_version": 1`) {
		t.Fatalf("legacy response was not encrypted on read: %s", migrated)
	}
}

func TestResponsesStoreIsBoundToAPIKeyOwner(t *testing.T) {
	configureResponsesEncryption(t)
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	resp := &ResponsesObject{
		ID:            "resp_owner_test",
		Object:        "response",
		Status:        "completed",
		Model:         "claude-sonnet-4.5",
		StoredAt:      time.Now().Unix(),
		OwnerAPIKeyID: "key-a",
	}
	if err := saveResponse(resp); err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, err := loadResponseForOwner(resp.ID, "key-a"); err != nil {
		t.Fatalf("owner should load response: %v", err)
	}
	if _, err := loadResponseForOwner(resp.ID, "key-b"); err == nil {
		t.Fatal("different API key must not load stored response")
	}
	ownerless := &ResponsesObject{ID: "resp_ownerless_test", Object: "response", Status: "completed", Model: "test", StoredAt: time.Now().Unix()}
	if err := saveResponse(ownerless); err != nil {
		t.Fatalf("save ownerless response: %v", err)
	}
	if _, err := loadResponseForOwner(ownerless.ID, "key-a"); err == nil {
		t.Fatal("authenticated API key must not claim an ownerless response")
	}
	if _, err := loadResponseForOwner(ownerless.ID, ""); err != nil {
		t.Fatalf("ownerless caller should load ownerless response: %v", err)
	}
}

func TestResponsesStoreEnforcesFileQuota(t *testing.T) {
	configureResponsesEncryption(t)
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	settings := config.GetResponsesStorageConfig()
	settings.MaxFiles = 2
	settings.MaxBytes = 1 << 20
	if err := config.UpdateResponsesStorageConfig(settings); err != nil {
		t.Fatalf("UpdateResponsesStorageConfig: %v", err)
	}

	for i, id := range []string{"resp_quota_a", "resp_quota_b", "resp_quota_c"} {
		resp := &ResponsesObject{ID: id, Object: "response", Status: "completed", Model: "test", StoredAt: time.Now().Unix()}
		if err := saveResponse(resp); err != nil {
			t.Fatalf("save %s: %v", id, err)
		}
		path := filepath.Join(responsesDir(), sanitizeResponseID(id)+".json")
		stamp := time.Now().Add(time.Duration(i) * time.Second)
		if err := os.Chtimes(path, stamp, stamp); err != nil && !os.IsNotExist(err) {
			t.Fatalf("chtimes %s: %v", id, err)
		}
	}
	purgeResponsesStorage()
	entries, err := os.ReadDir(responsesDir())
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	count := 0
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".json") {
			count++
		}
	}
	if count != 2 {
		t.Fatalf("expected file quota to retain 2 responses, got %d", count)
	}
}

func TestResponsesHistoryRespectsByteBudget(t *testing.T) {
	configureResponsesEncryption(t)
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	settings := config.GetResponsesStorageConfig()
	settings.MaxHistoryBytes = 64 << 10
	if err := config.UpdateResponsesStorageConfig(settings); err != nil {
		t.Fatalf("UpdateResponsesStorageConfig: %v", err)
	}

	large := strings.Repeat("x", 40000)
	ancestor := &ResponsesObject{
		ID: "resp_history_ancestor", Object: "response", Status: "completed", Model: "test",
		StoredAt: time.Now().Unix(), OwnerAPIKeyID: "key-a", StoredInput: json.RawMessage(`"ancestor"`),
		Output: []ResponseOutputItem{{Type: "message", Role: "assistant", Content: []ResponseContentPart{{Type: "output_text", Text: large}}}},
	}
	current := &ResponsesObject{
		ID: "resp_history_current", Object: "response", Status: "completed", Model: "test",
		StoredAt: time.Now().Unix(), OwnerAPIKeyID: "key-a", PreviousResponseID: ancestor.ID, StoredInput: json.RawMessage(`"current"`),
		Output: []ResponseOutputItem{{Type: "message", Role: "assistant", Content: []ResponseContentPart{{Type: "output_text", Text: large}}}},
	}
	if err := saveResponse(ancestor); err != nil {
		t.Fatalf("save ancestor: %v", err)
	}
	if err := saveResponse(current); err != nil {
		t.Fatalf("save current: %v", err)
	}
	chain := collectAncestorChain(current)
	if len(chain) != 1 || chain[0].ID != current.ID {
		t.Fatalf("expected byte budget to keep only current response, got %+v", chain)
	}
}

func TestResponsesPreviousResponseIDExpands(t *testing.T) {
	prev := &ResponsesObject{
		ID:          "resp_prev",
		StoredInput: json.RawMessage(`"earlier user"`),
		Output: []ResponseOutputItem{
			{
				Type: "message",
				Role: "assistant",
				Content: []ResponseContentPart{{
					Type: "output_text",
					Text: "earlier assistant reply",
				}},
			},
			{
				Type:      "function_call",
				CallID:    "call_prev",
				Name:      "lookup",
				Arguments: `{"q":"x"}`,
			},
		},
	}

	expanded := expandPreviousResponseHistory(prev)
	if len(expanded) != 3 {
		t.Fatalf("expected 3 messages from history, got %d (%+v)", len(expanded), expanded)
	}
	if expanded[0].Role != "user" {
		t.Fatalf("expected first message to be user, got %+v", expanded[0])
	}
	if expanded[1].Role != "assistant" {
		t.Fatalf("expected second message to be assistant, got %+v", expanded[1])
	}
	if expanded[2].Role != "assistant" || len(expanded[2].ToolCalls) != 1 {
		t.Fatalf("expected third to be assistant with tool_calls, got %+v", expanded[2])
	}
	if expanded[2].ToolCalls[0].ID != "call_prev" {
		t.Fatalf("expected tool call id call_prev, got %+v", expanded[2].ToolCalls[0])
	}
}

func TestResponsesRouteKeyUsesStableConversationID(t *testing.T) {
	payload := &KiroPayload{}
	payload.ConversationState.ConversationID = "conversation-stable"
	first := responsesRouteKey(payload, "", "resp_first")
	second := responsesRouteKey(payload, "resp_first", "resp_second")
	third := responsesRouteKey(payload, "resp_second", "resp_third")
	if first != "conversation-stable" || second != first || third != first {
		t.Fatalf("expected stable route key across response chain, got %q %q %q", first, second, third)
	}
}

// A → B → C: when expanding history starting from C, all of A's and B's
// inputs/outputs must appear before C's. Previously only C's direct parent
// (B) was emitted, dropping A entirely.
func TestResponsesPreviousResponseIDExpandsFullChain(t *testing.T) {
	configureResponsesEncryption(t)
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}

	a := &ResponsesObject{
		ID:           "resp_a",
		Object:       "response",
		Status:       "completed",
		Model:        "claude-sonnet-4.5",
		StoredInput:  json.RawMessage(`"turn A user"`),
		StoredAt:     time.Now().Unix(),
		Instructions: "be terse",
		Output: []ResponseOutputItem{{
			Type: "message", Role: "assistant",
			Content: []ResponseContentPart{{Type: "output_text", Text: "turn A assistant"}},
		}},
	}
	b := &ResponsesObject{
		ID:                 "resp_b",
		Object:             "response",
		Status:             "completed",
		Model:              "claude-sonnet-4.5",
		StoredInput:        json.RawMessage(`"turn B user"`),
		StoredAt:           time.Now().Unix(),
		PreviousResponseID: a.ID,
		Output: []ResponseOutputItem{{
			Type: "message", Role: "assistant",
			Content: []ResponseContentPart{{Type: "output_text", Text: "turn B assistant"}},
		}},
	}
	if err := saveResponse(a); err != nil {
		t.Fatalf("save a: %v", err)
	}
	if err := saveResponse(b); err != nil {
		t.Fatalf("save b: %v", err)
	}

	expanded := expandPreviousResponseHistory(b)

	var transcript []string
	for _, m := range expanded {
		role := m.Role
		text, _ := m.Content.(string)
		transcript = append(transcript, role+":"+text)
	}
	got := strings.Join(transcript, "|")
	want := "system:be terse|user:turn A user|assistant:turn A assistant|user:turn B user|assistant:turn B assistant"
	if got != want {
		t.Fatalf("chain order mismatch:\n got=%s\nwant=%s", got, want)
	}
}

// New instructions sent on a continuation request must take effect, even when
// previous_response_id is set. The bug: the old code only attached
// req.Instructions when previous_response_id was empty, silently dropping
// updated system prompts on follow-up turns.
func TestResponsesContinuationKeepsNewInstructions(t *testing.T) {
	h, cleanup := setupResponsesTestHandler(t)
	defer cleanup()

	prev := &ResponsesObject{
		ID:          "resp_for_continuation",
		Object:      "response",
		Status:      "completed",
		Model:       "claude-sonnet-4.5",
		StoredInput: json.RawMessage(`"first user message"`),
		StoredAt:    time.Now().Unix(),
		Output: []ResponseOutputItem{{
			Type: "message", Role: "assistant",
			Content: []ResponseContentPart{{Type: "output_text", Text: "first reply"}},
		}},
	}
	if err := saveResponse(prev); err != nil {
		t.Fatalf("save prev: %v", err)
	}

	var capturedSystem string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		capturedSystem = string(body)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
			"content": "second reply",
		}))
		_, _ = w.Write(awsEventStreamFrame(t, "contextUsageEvent", map[string]interface{}{
			"contextUsagePercentage": 1.0,
		}))
	}))
	defer server.Close()
	defer swapKiroEndpointsForTest(t, server)()

	body := strings.NewReader(`{
		"model":"claude-sonnet-4.5",
		"input":"second user turn",
		"previous_response_id":"resp_for_continuation",
		"instructions":"speak only French",
		"store":false
	}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", body)
	rec := httptest.NewRecorder()
	h.handleOpenAIResponses(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(capturedSystem, "speak only French") {
		t.Fatalf("expected new instructions to reach upstream, payload=%s", capturedSystem)
	}
}

func setupResponsesTestHandler(t *testing.T) (*Handler, func()) {
	t.Helper()
	configureResponsesEncryption(t)
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.AddAccount(config.Account{
		ID:          "test-account",
		Enabled:     true,
		AccessToken: "token-test",
		ProfileArn:  "arn:aws:codewhisperer:profile/test",
	}); err != nil {
		t.Fatalf("add account: %v", err)
	}
	if err := config.UpdatePreferredEndpoint("kiro"); err != nil {
		t.Fatalf("set endpoint: %v", err)
	}
	if err := config.UpdateEndpointFallback(false); err != nil {
		t.Fatalf("disable fallback: %v", err)
	}
	p := accountpool.GetPool()
	p.Reload()
	h := &Handler{
		pool:        p,
		promptCache: newPromptCacheTracker(defaultPromptCacheTTL),
	}
	cleanup := func() {}
	return h, cleanup
}

func swapKiroEndpointsForTest(t *testing.T, server *httptest.Server) func() {
	t.Helper()
	oldEndpoints := kiroEndpoints
	kiroEndpoints = []kiroEndpoint{{
		URL:    server.URL,
		Origin: "AI_EDITOR",
		Name:   "test",
	}}
	oldClient := kiroHttpStore.Load()
	kiroHttpStore.Store(&http.Client{Timeout: time.Second, Transport: &http.Transport{}})
	return func() {
		kiroEndpoints = oldEndpoints
		kiroHttpStore.Store(oldClient)
	}
}

func TestResponsesNonStreamRoundTrip(t *testing.T) {
	h, cleanup := setupResponsesTestHandler(t)
	defer cleanup()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
			"content": "responses non-stream OK",
		}))
		_, _ = w.Write(awsEventStreamFrame(t, "contextUsageEvent", map[string]interface{}{
			"contextUsagePercentage": 1.0,
		}))
	}))
	defer server.Close()
	defer swapKiroEndpointsForTest(t, server)()

	body := strings.NewReader(`{"model":"claude-sonnet-4.5","input":"hi from test","store":true}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", body)
	req = req.WithContext(context.Background())
	rec := httptest.NewRecorder()

	h.handleOpenAIResponses(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	var resp ResponsesObject
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body.String())
	}
	if resp.Object != "response" {
		t.Fatalf("expected object=response, got %q", resp.Object)
	}
	if resp.Status != "completed" {
		t.Fatalf("expected status=completed, got %q", resp.Status)
	}
	if len(resp.Output) == 0 {
		t.Fatalf("expected output items, got none")
	}
	if resp.Output[0].Type != "message" || len(resp.Output[0].Content) == 0 {
		t.Fatalf("expected message with content, got %+v", resp.Output[0])
	}
	if resp.Output[0].Content[0].Text != "responses non-stream OK" {
		t.Fatalf("unexpected text: %q", resp.Output[0].Content[0].Text)
	}

	loaded, err := loadResponse(resp.ID)
	if err != nil {
		t.Fatalf("loadResponse: %v", err)
	}
	if loaded.ID != resp.ID {
		t.Fatalf("stored response id mismatch")
	}
}

func TestResponsesStreamSSE(t *testing.T) {
	h, cleanup := setupResponsesTestHandler(t)
	defer cleanup()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
			"content": "stream chunk",
		}))
		_, _ = w.Write(awsEventStreamFrame(t, "contextUsageEvent", map[string]interface{}{
			"contextUsagePercentage": 1.0,
		}))
	}))
	defer server.Close()
	defer swapKiroEndpointsForTest(t, server)()

	body := strings.NewReader(`{"model":"claude-sonnet-4.5","input":"stream please","stream":true,"store":false}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", body)
	rec := httptest.NewRecorder()

	h.handleOpenAIResponses(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	bodyBytes, _ := io.ReadAll(rec.Body)
	bodyStr := string(bodyBytes)

	for _, evt := range []string{"event: response.created", "event: response.output_text.delta", "event: response.completed"} {
		if !strings.Contains(bodyStr, evt) {
			t.Fatalf("missing event %q in stream body:\n%s", evt, bodyStr)
		}
	}
	if count := strings.Count(bodyStr, "event: response.in_progress"); count != 1 {
		t.Fatalf("expected one response.in_progress event, got %d:\n%s", count, bodyStr)
	}
	if !strings.Contains(bodyStr, "stream chunk") {
		t.Fatalf("expected stream content delta, got:\n%s", bodyStr)
	}
}

func TestResponsesEmitsReasoningOutputAndStreamEvents(t *testing.T) {
	h, cleanup := setupResponsesTestHandler(t)
	defer cleanup()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(awsEventStreamFrame(t, "reasoningContentEvent", map[string]interface{}{"text": "reasoning summary"}))
		_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "final answer"}))
		_, _ = w.Write(awsEventStreamFrame(t, "contextUsageEvent", map[string]interface{}{"contextUsagePercentage": 1.0}))
	}))
	defer server.Close()
	defer swapKiroEndpointsForTest(t, server)()

	nonStream := httptest.NewRecorder()
	h.handleOpenAIResponses(nonStream, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"claude-sonnet-4.5-thinking","input":"think","store":false}`)))
	var response ResponsesObject
	if err := json.Unmarshal(nonStream.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode non-stream response: %v body=%s", err, nonStream.Body.String())
	}
	if len(response.Output) < 2 || response.Output[0].Type != "reasoning" || len(response.Output[0].Summary) != 1 || response.Output[0].Summary[0].Text != "reasoning summary" {
		t.Fatalf("reasoning output missing: %+v", response.Output)
	}

	stream := httptest.NewRecorder()
	h.handleOpenAIResponses(stream, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"claude-sonnet-4.5-thinking","input":"think","stream":true,"store":false}`)))
	for _, event := range []string{
		"event: response.reasoning_summary_part.added",
		"event: response.reasoning_summary_text.delta",
		"event: response.reasoning_summary_text.done",
	} {
		if !strings.Contains(stream.Body.String(), event) {
			t.Fatalf("missing %q in stream:\n%s", event, stream.Body.String())
		}
	}
}
