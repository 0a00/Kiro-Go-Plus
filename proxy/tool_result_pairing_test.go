package proxy

import (
	"encoding/json"
	"kiro-go/config"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func pairingHistory(toolIDs ...string) []KiroHistoryMessage {
	uses := make([]KiroToolUse, 0, len(toolIDs))
	for _, id := range toolIDs {
		uses = append(uses, KiroToolUse{ToolUseID: id, Name: "test_tool", Input: map[string]interface{}{}})
	}
	return []KiroHistoryMessage{
		{UserInputMessage: &KiroUserInputMessage{Content: "run the tools"}},
		{AssistantResponseMessage: &KiroAssistantResponseMessage{ToolUses: uses}},
	}
}

func pairingResult(id, text string) KiroToolResult {
	return KiroToolResult{
		ToolUseID: id,
		Content:   []KiroResultContent{{Text: text}},
		Status:    "success",
	}
}

func TestOrderToolResultsForLastAssistantRequiresExactPairing(t *testing.T) {
	history := pairingHistory("tool_a", "tool_b")
	cases := []struct {
		name    string
		results []KiroToolResult
		valid   bool
	}{
		{
			name: "exact pair",
			results: []KiroToolResult{
				pairingResult("tool_a", "a"),
				pairingResult("tool_b", "b"),
			},
			valid: true,
		},
		{
			name: "parallel results are reordered",
			results: []KiroToolResult{
				pairingResult("tool_b", "b"),
				pairingResult("tool_a", "a"),
			},
			valid: true,
		},
		{
			name:    "missing result",
			results: []KiroToolResult{pairingResult("tool_a", "a")},
		},
		{
			name: "extra result",
			results: []KiroToolResult{
				pairingResult("tool_a", "a"),
				pairingResult("tool_b", "b"),
				pairingResult("tool_c", "c"),
			},
		},
		{
			name: "duplicate result",
			results: []KiroToolResult{
				pairingResult("tool_a", "a1"),
				pairingResult("tool_a", "a2"),
			},
		},
		{
			name: "empty result ID",
			results: []KiroToolResult{
				pairingResult("", "a"),
				pairingResult("tool_b", "b"),
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ordered, valid := orderToolResultsForLastAssistant(history, tc.results)
			if valid != tc.valid {
				t.Fatalf("valid = %v, want %v; ordered = %#v", valid, tc.valid, ordered)
			}
			if !tc.valid {
				return
			}
			if len(ordered) != 2 || ordered[0].ToolUseID != "tool_a" || ordered[1].ToolUseID != "tool_b" {
				t.Fatalf("ordered IDs = %#v, want [tool_a tool_b]", ordered)
			}
		})
	}
}

func TestClaudeToKiroFlattensMismatchedToolResults(t *testing.T) {
	request := &ClaudeRequest{
		Model: "claude-sonnet-4.6",
		Messages: []ClaudeMessage{
			{Role: "user", Content: "run a tool"},
			{Role: "assistant", Content: []interface{}{
				map[string]interface{}{"type": "tool_use", "id": "tool_a", "name": "test_tool", "input": map[string]interface{}{}},
			}},
			{Role: "user", Content: []interface{}{
				map[string]interface{}{"type": "tool_result", "tool_use_id": "tool_a", "content": "valid result"},
				map[string]interface{}{"type": "tool_result", "tool_use_id": "orphan", "content": "orphan result"},
			}},
		},
	}

	payload := ClaudeToKiro(request, false)
	current := payload.ConversationState.CurrentMessage.UserInputMessage
	if current.UserInputMessageContext != nil && len(current.UserInputMessageContext.ToolResults) != 0 {
		t.Fatalf("mismatched results remained structured: %#v", current.UserInputMessageContext.ToolResults)
	}
	if !strings.Contains(current.Content, "valid result") || !strings.Contains(current.Content, "orphan result") {
		t.Fatalf("flattened results lost content: %q", current.Content)
	}
}

func TestOpenAIToKiroOrdersParallelToolResults(t *testing.T) {
	request := &OpenAIRequest{
		Model: "claude-sonnet-4.6",
		Messages: []OpenAIMessage{
			{Role: "user", Content: "run both"},
			{Role: "assistant", ToolCalls: []ToolCall{
				newToolCall("call_a", "first", `{}`),
				newToolCall("call_b", "second", `{}`),
			}},
			{Role: "tool", ToolCallID: "call_b", Content: "result b"},
			{Role: "tool", ToolCallID: "call_a", Content: "result a"},
		},
	}

	payload := OpenAIToKiro(request, false)
	current := payload.ConversationState.CurrentMessage.UserInputMessage
	if current.UserInputMessageContext == nil || len(current.UserInputMessageContext.ToolResults) != 2 {
		t.Fatalf("parallel results were not preserved: %#v", current.UserInputMessageContext)
	}
	results := current.UserInputMessageContext.ToolResults
	if results[0].ToolUseID != "call_a" || results[1].ToolUseID != "call_b" {
		t.Fatalf("parallel result order = [%s %s], want [call_a call_b]", results[0].ToolUseID, results[1].ToolUseID)
	}
}

func TestResponsesInputToolPairingReachesOpenAITranslator(t *testing.T) {
	input := json.RawMessage(`[
		{"type":"function_call","call_id":"call_a","name":"first","arguments":"{}"},
		{"type":"function_call","call_id":"call_b","name":"second","arguments":"{}"},
		{"type":"function_call_output","call_id":"call_b","output":"result b"},
		{"type":"function_call_output","call_id":"call_a","output":"result a"}
	]`)
	parsed, err := parseResponsesInputWithTools(input)
	if err != nil {
		t.Fatalf("parse responses input: %v", err)
	}

	payload := OpenAIToKiro(&OpenAIRequest{Model: "claude-sonnet-4.6", Messages: parsed.Messages}, false)
	current := payload.ConversationState.CurrentMessage.UserInputMessage
	if current.UserInputMessageContext == nil || len(current.UserInputMessageContext.ToolResults) != 2 {
		t.Fatalf("responses tool results were not preserved: %#v", current.UserInputMessageContext)
	}
	results := current.UserInputMessageContext.ToolResults
	if results[0].ToolUseID != "call_a" || results[1].ToolUseID != "call_b" {
		t.Fatalf("responses result order = [%s %s], want [call_a call_b]", results[0].ToolUseID, results[1].ToolUseID)
	}
}

func TestRepairKiroPayloadToolResultsFlattensInvalidCurrentResults(t *testing.T) {
	payload := &KiroPayload{}
	payload.ConversationState.History = pairingHistory("tool_a")
	payload.ConversationState.CurrentMessage.UserInputMessage = KiroUserInputMessage{
		Content: "continue",
		UserInputMessageContext: &UserInputMessageContext{
			ToolResults: []KiroToolResult{pairingResult("unexpected", "important output")},
		},
	}

	repairKiroPayloadToolResults(payload)
	current := payload.ConversationState.CurrentMessage.UserInputMessage
	if current.UserInputMessageContext != nil && len(current.UserInputMessageContext.ToolResults) != 0 {
		t.Fatalf("invalid current result remained structured: %#v", current.UserInputMessageContext.ToolResults)
	}
	if !strings.Contains(current.Content, "important output") {
		t.Fatalf("flattened result was lost: %q", current.Content)
	}
	for i, message := range payload.ConversationState.History {
		if message.AssistantResponseMessage != nil && len(message.AssistantResponseMessage.ToolUses) > 0 {
			t.Fatalf("history[%d] retained an orphaned tool use", i)
		}
		if message.UserInputMessage != nil && message.UserInputMessage.UserInputMessageContext != nil &&
			len(message.UserInputMessage.UserInputMessageContext.ToolResults) > 0 {
			t.Fatalf("history[%d] retained structured tool results", i)
		}
	}
}

func TestCallKiroAPIPreflightRepairsPayloadBeforeMarshal(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.UpdatePreferredEndpoint("kiro"); err != nil {
		t.Fatalf("set endpoint: %v", err)
	}
	if err := config.UpdateEndpointFallback(false); err != nil {
		t.Fatalf("disable endpoint fallback: %v", err)
	}

	requestChecked := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var received KiroPayload
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Errorf("decode upstream payload: %v", err)
		} else {
			current := received.ConversationState.CurrentMessage.UserInputMessage
			if current.UserInputMessageContext != nil && len(current.UserInputMessageContext.ToolResults) > 0 {
				t.Errorf("upstream received structured orphan result: %#v", current.UserInputMessageContext.ToolResults)
			}
			for i, message := range received.ConversationState.History {
				if message.AssistantResponseMessage != nil && len(message.AssistantResponseMessage.ToolUses) > 0 {
					t.Errorf("upstream received history tool use at index %d", i)
				}
			}
			if !strings.Contains(current.Content, "preflight output") {
				t.Errorf("upstream payload lost flattened output: %q", current.Content)
			}
		}
		requestChecked <- struct{}{}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "ok"}))
		_, _ = w.Write(awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{"stopReason": "end_turn"}))
	}))
	defer server.Close()

	oldEndpoints := kiroEndpoints
	oldClient := kiroHttpStore.Load()
	kiroEndpoints = []kiroEndpoint{{Key: "kiro", URL: server.URL, Origin: "AI_EDITOR", Name: "pairing-test"}}
	kiroHttpStore.Store(&http.Client{Transport: &http.Transport{}})
	t.Cleanup(func() {
		kiroEndpoints = oldEndpoints
		kiroHttpStore.Store(oldClient)
	})
	sharedAccountEndpointRoutes.reset()
	t.Cleanup(sharedAccountEndpointRoutes.reset)

	payload := &KiroPayload{}
	payload.ConversationState.CurrentMessage.UserInputMessage = KiroUserInputMessage{
		ModelID: "claude-sonnet-4.6",
		Content: "continue",
		UserInputMessageContext: &UserInputMessageContext{
			ToolResults: []KiroToolResult{pairingResult("orphan", "preflight output")},
		},
	}
	payload.ConversationState.History = pairingHistory("tool_a")

	if err := CallKiroAPI(&config.Account{ID: "preflight-pairing-account", AccessToken: "token"}, payload, &KiroStreamCallback{}); err != nil {
		t.Fatalf("CallKiroAPI: %v", err)
	}
	select {
	case <-requestChecked:
	default:
		t.Fatal("upstream request was not observed")
	}
}

func TestToolUseResultMismatchIsNonRetryableClientError(t *testing.T) {
	err := classifyUpstreamHTTPError(http.StatusBadRequest, "Kiro IDE", []byte(`{
		"__type":"TOOL_USE_RESULT_MISMATCH",
		"message":"unexpected tool_use_id found in tool_result blocks"
	}`))
	if err.Kind != UpstreamErrorClientRequest || err.RetryAcrossAccounts || err.RetryAcrossEndpoints {
		t.Fatalf("unexpected mismatch classification: %+v", err)
	}
	if mapped := mapDownstreamError(err); mapped.Status != http.StatusBadRequest {
		t.Fatalf("mismatch mapped to HTTP %d, want 400", mapped.Status)
	}
}
