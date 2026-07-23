package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kiro-go/config"
)

func TestResponsesCodexAdditionalToolsAndCompatibilityItems(t *testing.T) {
	raw := json.RawMessage(`[
		{"type":"additional_tools","role":"developer","tools":[
			{"type":"custom","name":"exec","description":"Run JavaScript","format":{"type":"grammar"}},
			{"type":"function","name":"wait","description":"Wait","parameters":{"type":"object","properties":{"ms":{"type":"number"}}}}
		]},
		{"type":"reasoning","encrypted_content":"opaque"},
		{"type":"item_reference","id":"item_1"},
		{"type":"message","role":"developer","content":"follow repository rules"},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"list files"}]}
	]`)

	result, err := parseResponsesInputWithTools(raw)
	if err != nil {
		t.Fatalf("parse Codex input: %v", err)
	}
	if !result.AdditionalToolsPresent || len(result.AdditionalTools) != 2 {
		t.Fatalf("additional tools were not collected: %+v", result)
	}
	if len(result.Messages) != 2 || result.Messages[0].Role != "system" || result.Messages[1].Role != "user" {
		t.Fatalf("unexpected normalized messages: %+v", result.Messages)
	}

	converted := convertOpenAITools(result.AdditionalTools)
	if len(converted) != 2 {
		t.Fatalf("expected custom and function tools, got %d", len(converted))
	}
	schema, ok := converted[0].ToolSpecification.InputSchema.JSON.(map[string]interface{})
	if !ok {
		t.Fatalf("custom tool schema has unexpected type: %#v", converted[0].ToolSpecification.InputSchema.JSON)
	}
	properties, _ := schema["properties"].(map[string]interface{})
	input, _ := properties["input"].(map[string]interface{})
	if input["type"] != "string" {
		t.Fatalf("custom tool did not receive freeform input schema: %#v", schema)
	}
}

func TestResponsesCustomToolCallHistory(t *testing.T) {
	raw := json.RawMessage(`[
		{"type":"message","role":"user","content":"run it"},
		{"type":"custom_tool_call","call_id":"call_exec","name":"exec","input":"await tools.exec_command({cmd:'pwd'})"},
		{"type":"custom_tool_call_output","call_id":"call_exec","output":"/workspace"}
	]`)

	messages, err := parseResponsesInput(raw)
	if err != nil {
		t.Fatalf("parse custom tool history: %v", err)
	}
	if len(messages) != 3 || len(messages[1].ToolCalls) != 1 {
		t.Fatalf("unexpected custom tool history: %+v", messages)
	}
	arguments := decodeOpenAIToolArguments(messages[1].ToolCalls[0].Function.Arguments)
	if arguments["input"] != "await tools.exec_command({cmd:'pwd'})" {
		t.Fatalf("custom input was not wrapped for Kiro: %#v", arguments)
	}
	if messages[2].Role != "tool" || messages[2].ToolCallID != "call_exec" {
		t.Fatalf("custom tool output was not normalized: %+v", messages[2])
	}
}

func TestResponsesRequestOptionsInheritOnlyWhenAbsent(t *testing.T) {
	custom := mustTool(t, `{"type":"custom","name":"exec","description":"Run code"}`)
	previous := &ResponsesObject{
		StoredTools:      []OpenAITool{custom},
		StoredToolChoice: json.RawMessage(`{"type":"custom","name":"exec"}`),
	}

	inherited := ResponsesRequest{}
	applyResponsesRequestOptions(&inherited, responsesInputResult{}, previous)
	if len(inherited.Tools) != 1 || inherited.Tools[0].Function.Name != "exec" {
		t.Fatalf("missing inherited tools: %+v", inherited.Tools)
	}
	if string(inherited.ToolChoice) != `{"type":"custom","name":"exec"}` {
		t.Fatalf("missing inherited tool choice: %s", inherited.ToolChoice)
	}

	explicitEmpty := ResponsesRequest{
		Tools:      []OpenAITool{},
		ToolChoice: json.RawMessage(`"none"`),
	}
	applyResponsesRequestOptions(&explicitEmpty, responsesInputResult{}, previous)
	if explicitEmpty.Tools == nil || len(explicitEmpty.Tools) != 0 {
		t.Fatalf("explicit empty tools must not inherit: %#v", explicitEmpty.Tools)
	}
	if string(explicitEmpty.ToolChoice) != `"none"` {
		t.Fatalf("explicit tool choice was overwritten: %s", explicitEmpty.ToolChoice)
	}
}

func TestResponsesCustomToolOutputAndHistoryExpansion(t *testing.T) {
	customTools := responseToolNameSet{"exec": {}}
	item := buildResponseToolOutputItem(KiroToolUse{
		ToolUseID: "call_exec",
		Name:      "exec",
		Input:     map[string]interface{}{"input": "console.log('ok')"},
	}, customTools)
	if item.Type != "custom_tool_call" || item.Input != "console.log('ok')" || item.Arguments != "" {
		t.Fatalf("unexpected custom output item: %+v", item)
	}

	messages := outputToMessages([]ResponseOutputItem{item})
	if len(messages) != 1 || len(messages[0].ToolCalls) != 1 {
		t.Fatalf("custom stored output was not expanded: %+v", messages)
	}
	arguments := decodeOpenAIToolArguments(messages[0].ToolCalls[0].Function.Arguments)
	if arguments["input"] != "console.log('ok')" {
		t.Fatalf("expanded custom input mismatch: %#v", arguments)
	}
}

func TestResponsesStorePersistsContinuationTools(t *testing.T) {
	configureResponsesEncryption(t)
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	response := &ResponsesObject{
		ID:                 "resp_tools_storage",
		Object:             "response",
		Status:             "completed",
		Model:              "claude-sonnet-4.5",
		StoredAt:           time.Now().Unix(),
		StoredTools:        []OpenAITool{mustTool(t, `{"type":"custom","name":"exec"}`)},
		StoredToolChoice:   json.RawMessage(`"required"`),
		PreviousResponseID: "",
	}
	if err := saveResponse(response); err != nil {
		t.Fatalf("save response: %v", err)
	}
	loaded, err := loadResponse(response.ID)
	if err != nil {
		t.Fatalf("load response: %v", err)
	}
	if len(loaded.StoredTools) != 1 || loaded.StoredTools[0].Type != "custom" {
		t.Fatalf("stored tools mismatch: %+v", loaded.StoredTools)
	}
	if string(loaded.StoredToolChoice) != `"required"` {
		t.Fatalf("stored tool choice mismatch: %s", loaded.StoredToolChoice)
	}
}

func TestResponsesCustomToolStreamEvents(t *testing.T) {
	h, cleanup := setupResponsesTestHandler(t)
	defer cleanup()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
			"toolUseId": "call_exec",
			"name":      "exec",
			"input":     `{"input":"await tools.exec_command({cmd:'pwd'})"}`,
			"stop":      true,
		}))
		_, _ = w.Write(awsEventStreamFrame(t, "contextUsageEvent", map[string]interface{}{
			"contextUsagePercentage": 1.0,
		}))
	}))
	defer server.Close()
	defer swapKiroEndpointsForTest(t, server)()

	body := strings.NewReader(`{
		"model":"claude-sonnet-4.5",
		"input":[
			{"type":"additional_tools","tools":[{"type":"custom","name":"exec","description":"Run code"}]},
			{"type":"message","role":"user","content":"run pwd"}
		],
		"stream":true,
		"store":false
	}`)
	recorder := httptest.NewRecorder()
	h.handleOpenAIResponses(recorder, httptest.NewRequest(http.MethodPost, "/v1/responses", body))

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", recorder.Code, recorder.Body.String())
	}
	stream := recorder.Body.String()
	for _, event := range []string{
		"event: response.custom_tool_call_input.delta",
		"event: response.custom_tool_call_input.done",
		`"type":"custom_tool_call"`,
		`"input":"await tools.exec_command({cmd:'pwd'})"`,
	} {
		if !strings.Contains(stream, event) {
			t.Fatalf("missing %q in stream:\n%s", event, stream)
		}
	}
	if strings.Contains(stream, "response.function_call_arguments.delta") {
		t.Fatalf("custom tool leaked function-call events:\n%s", stream)
	}
}

func TestOpenAIRoleNormalization(t *testing.T) {
	request := &OpenAIRequest{Messages: []OpenAIMessage{
		{Role: "developer", Content: "follow policy"},
		{Role: "user", Content: "run search"},
		{Role: "assistant", Content: "", ToolCalls: []ToolCall{{ID: "call_1", Type: "function"}}},
		{Role: "function", Name: "search", Content: "result"},
	}}
	if message := validateOpenAIRequestShape(request); message != "" {
		t.Fatalf("valid official roles were rejected: %s", message)
	}
	if request.Messages[0].Role != "system" {
		t.Fatalf("developer role was not normalized: %+v", request.Messages[0])
	}
	legacy := request.Messages[3]
	if legacy.Role != "tool" || legacy.ToolCallID != "search" {
		t.Fatalf("legacy function role was not normalized: %+v", legacy)
	}

	invalid := &OpenAIRequest{Messages: []OpenAIMessage{
		{Role: "user", Content: "hello"},
		{Role: "alien", Content: "ignored before this fix"},
	}}
	if message := validateOpenAIRequestShape(invalid); !strings.Contains(message, "unsupported message role") {
		t.Fatalf("unknown role should be rejected, got %q", message)
	}
}
