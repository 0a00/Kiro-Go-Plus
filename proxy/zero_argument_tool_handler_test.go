package proxy

import (
	"encoding/json"
	"github.com/google/uuid"
	"kiro-go/config"
	accountpool "kiro-go/pool"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestClaudeMessagesRecoversZeroArgumentToolEndToEnd(t *testing.T) {
	t.Setenv("ALLOW_UNAUTHENTICATED_API", "true")
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.AddAccount(config.Account{
		ID: "zero-argument-handler", Enabled: true, AccessToken: "token-zero", ProfileArn: "arn:aws:codewhisperer:profile/zero",
	}); err != nil {
		t.Fatalf("add account: %v", err)
	}
	if err := config.UpdatePreferredEndpoint("kiro"); err != nil {
		t.Fatalf("set preferred endpoint: %v", err)
	}
	if err := config.UpdateEndpointFallback(false); err != nil {
		t.Fatalf("disable endpoint fallback: %v", err)
	}

	upstreamCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		var payload KiroPayload
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode upstream payload: %v", err)
			http.Error(w, "invalid payload", http.StatusBadRequest)
			return
		}
		context := payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext
		if context == nil || len(context.Tools) != 1 {
			t.Errorf("upstream tools = %#v, want one tool", context)
			http.Error(w, "missing tool", http.StatusBadRequest)
			return
		}
		toolName := context.Tools[0].ToolSpecification.Name
		w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
			"toolUseId": "toolu_zero_handler",
			"name":      toolName,
		}))
		_, _ = w.Write(awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{"stopReason": "tool_use"}))
	}))
	defer upstream.Close()
	defer swapKiroEndpointsForTest(t, upstream)()

	p := accountpool.GetPool()
	p.Reload()
	h := &Handler{pool: p, promptCache: newPromptCacheTracker(defaultPromptCacheTTL), requestLog: newRequestLog(defaultRequestLogLimit)}

	const toolName = "mcp__memory__read_graph"
	for _, stream := range []bool{false, true} {
		name := "non-stream"
		if stream {
			name = "stream"
		}
		t.Run(name, func(t *testing.T) {
			before := upstreamCalls
			requestBody := `{
				"model":"claude-sonnet-4.6",
				"stream":` + jsonBool(stream) + `,
				"max_tokens":256,
				"messages":[{"role":"user","content":"Read the memory graph."}],
				"tools":[{"name":"` + toolName + `","description":"Read memory.","input_schema":{"type":"object","properties":{},"additionalProperties":false}}],
				"tool_choice":{"type":"tool","name":"` + toolName + `"}
			}`
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(requestBody))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			if upstreamCalls-before != 1 {
				t.Fatalf("upstream calls = %d, want exactly one", upstreamCalls-before)
			}
			if stream {
				assertZeroArgumentToolSSE(t, rec.Body.String(), toolName)
				return
			}

			var response ClaudeResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
				t.Fatalf("decode Claude response: %v body=%s", err, rec.Body.String())
			}
			if response.StopReason != "tool_use" || len(response.Content) != 1 {
				t.Fatalf("unexpected response completion: %+v", response)
			}
			block := response.Content[0]
			input, ok := block.Input.(map[string]interface{})
			if block.Type != "tool_use" || block.Name != toolName || !ok || len(input) != 0 {
				t.Fatalf("unexpected zero-argument tool block: %+v", block)
			}
		})
	}
}

func TestClaudeMessagesZeroArgumentRecoveryPolicyEndToEnd(t *testing.T) {
	tests := []struct {
		name        string
		schema      map[string]interface{}
		stopReasons []string
		wantSuccess bool
	}{
		{
			name: "closed schema at clean EOF",
			schema: map[string]interface{}{
				"type": "object", "properties": map[string]interface{}{}, "additionalProperties": false,
			},
			wantSuccess: true,
		},
		{
			name: "declared empty schema at explicit tool use",
			schema: map[string]interface{}{
				"type": "object", "properties": map[string]interface{}{},
			},
			stopReasons: []string{"tool_use"},
			wantSuccess: true,
		},
		{
			name: "declared empty schema at clean EOF",
			schema: map[string]interface{}{
				"type": "object", "properties": map[string]interface{}{},
			},
		},
		{
			name: "open schema at explicit tool use",
			schema: map[string]interface{}{
				"type": "object", "properties": map[string]interface{}{}, "additionalProperties": true,
			},
			stopReasons: []string{"tool_use"},
		},
		{
			name: "closed schema at end turn",
			schema: map[string]interface{}{
				"type": "object", "properties": map[string]interface{}{}, "additionalProperties": false,
			},
			stopReasons: []string{"end_turn"},
		},
		{
			name: "closed schema with conflicting stops",
			schema: map[string]interface{}{
				"type": "object", "properties": map[string]interface{}{}, "additionalProperties": false,
			},
			stopReasons: []string{"tool_use", "end_turn"},
		},
	}

	for _, tc := range tests {
		for _, stream := range []bool{false, true} {
			mode := "non-stream"
			if stream {
				mode = "stream"
			}
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				runZeroArgumentPolicyHandlerCase(t, tc.schema, tc.stopReasons, stream, tc.wantSuccess)
			})
		}
	}
}

func runZeroArgumentPolicyHandlerCase(t *testing.T, schema map[string]interface{}, stopReasons []string, stream, wantSuccess bool) {
	t.Helper()
	t.Setenv("ALLOW_UNAUTHENTICATED_API", "true")
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	retry := config.GetRetryConfig()
	retry.MaxAccountAttempts = 1
	retry.MaxUpstreamAttempts = 1
	retry.MaxRetryDurationSeconds = 3
	retry.FirstTokenTimeoutSeconds = 2
	retry.StreamIdleTimeoutSeconds = 2
	if err := config.UpdateRetryConfig(retry); err != nil {
		t.Fatalf("update retry config: %v", err)
	}
	accountID := "zero-policy-" + uuid.New().String()
	if err := config.AddAccount(config.Account{
		ID: accountID, Enabled: true, AccessToken: "token-zero", ProfileArn: "arn:aws:codewhisperer:profile/" + accountID,
	}); err != nil {
		t.Fatalf("add account: %v", err)
	}
	if err := config.UpdatePreferredEndpoint("kiro"); err != nil {
		t.Fatalf("set preferred endpoint: %v", err)
	}
	if err := config.UpdateEndpointFallback(false); err != nil {
		t.Fatalf("disable endpoint fallback: %v", err)
	}

	upstreamCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		var payload KiroPayload
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode upstream payload: %v", err)
			http.Error(w, "invalid payload", http.StatusBadRequest)
			return
		}
		context := payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext
		if context == nil || len(context.Tools) != 1 {
			t.Errorf("upstream tools = %#v, want one tool", context)
			http.Error(w, "missing tool", http.StatusBadRequest)
			return
		}
		toolName := context.Tools[0].ToolSpecification.Name
		w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
			"toolUseId": "toolu_zero_policy", "name": toolName,
		}))
		for _, reason := range stopReasons {
			_, _ = w.Write(awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{"stopReason": reason}))
		}
	}))
	defer upstream.Close()
	defer swapKiroEndpointsForTest(t, upstream)()

	p := accountpool.GetPool()
	p.Reload()
	h := &Handler{pool: p, promptCache: newPromptCacheTracker(defaultPromptCacheTTL), requestLog: newRequestLog(defaultRequestLogLimit)}

	const toolName = "mcp__memory__read_graph"
	requestBody, err := json.Marshal(map[string]interface{}{
		"model": "claude-sonnet-4.6", "stream": stream, "max_tokens": 256,
		"messages": []interface{}{map[string]interface{}{"role": "user", "content": "Read the memory graph."}},
		"tools": []interface{}{map[string]interface{}{
			"name": toolName, "description": "Read memory.", "input_schema": schema,
		}},
		"tool_choice": map[string]interface{}{"type": "tool", "name": toolName},
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(string(requestBody)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if upstreamCalls != 1 {
		t.Fatalf("upstream calls = %d, want exactly one", upstreamCalls)
	}
	if wantSuccess {
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
		if stream {
			assertZeroArgumentToolSSE(t, rec.Body.String(), toolName)
			return
		}
		var response ClaudeResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
			t.Fatalf("decode Claude response: %v body=%s", err, rec.Body.String())
		}
		if response.StopReason != "tool_use" || len(response.Content) != 1 || response.Content[0].Type != "tool_use" {
			t.Fatalf("unexpected successful response: %+v", response)
		}
		return
	}

	body := rec.Body.String()
	if strings.Contains(body, `"type":"tool_use"`) {
		t.Fatalf("unsafe zero-argument tool was emitted: status=%d body=%s", rec.Code, body)
	}
	if !strings.Contains(body, `"type":"error"`) {
		t.Fatalf("rejected tool response did not contain an error: status=%d body=%s", rec.Code, body)
	}
	if !stream && rec.Code < http.StatusBadRequest {
		t.Fatalf("non-stream rejection status = %d, body=%s", rec.Code, body)
	}
}

func assertZeroArgumentToolSSE(t *testing.T, body, toolName string) {
	t.Helper()
	checks := map[string]int{
		`"type":"tool_use"`:           1,
		`"name":"` + toolName + `"`:   1,
		`"partial_json":"{}"`:         1,
		`"type":"content_block_stop"`: 1,
		`"type":"message_stop"`:       1,
	}
	for fragment, want := range checks {
		if got := strings.Count(body, fragment); got != want {
			t.Fatalf("SSE fragment %q count = %d, want %d; body=%s", fragment, got, want, body)
		}
	}
	if !strings.Contains(body, `"stop_reason":"tool_use"`) || strings.Contains(body, `"type":"error"`) {
		t.Fatalf("unexpected SSE terminal state: %s", body)
	}
}

func jsonBool(value bool) string {
	if value {
		return "true"
	}
	return "false"
}
