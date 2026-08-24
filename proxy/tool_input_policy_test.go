package proxy

import (
	"encoding/json"
	"testing"
)

func TestSchemaDefinesZeroArgumentsConservatively(t *testing.T) {
	tests := []struct {
		name   string
		schema interface{}
		want   toolInputPolicy
	}{
		{name: "object only is open", schema: map[string]interface{}{"type": "object"}},
		{name: "empty schema is open", schema: map[string]interface{}{}},
		{name: "empty properties", schema: map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}, want: toolInputPolicyDeclaredEmpty},
		{name: "empty required", schema: map[string]interface{}{"type": "object", "properties": map[string]interface{}{}, "required": []interface{}{}, "minProperties": json.Number("0")}, want: toolInputPolicyDeclaredEmpty},
		{name: "closed empty object", schema: map[string]interface{}{"type": "object", "properties": map[string]interface{}{}, "additionalProperties": false, "maxProperties": float64(0)}, want: toolInputPolicyClosedEmpty},
		{name: "null required", schema: map[string]interface{}{"type": "object", "properties": map[string]interface{}{}, "required": nil}},
		{name: "optional property", schema: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"path": map[string]interface{}{"type": "string"}}}},
		{name: "required property", schema: map[string]interface{}{"type": "object", "required": []interface{}{"path"}}},
		{name: "minimum properties", schema: map[string]interface{}{"type": "object", "minProperties": float64(1)}},
		{name: "invalid negative minimum", schema: map[string]interface{}{"type": "object", "properties": map[string]interface{}{}, "minProperties": float64(-1)}},
		{name: "positive maximum", schema: map[string]interface{}{"type": "object", "properties": map[string]interface{}{}, "maxProperties": float64(1)}},
		{name: "open additional properties", schema: map[string]interface{}{"type": "object", "properties": map[string]interface{}{}, "additionalProperties": true}},
		{name: "typed additional properties", schema: map[string]interface{}{"type": "object", "properties": map[string]interface{}{}, "additionalProperties": map[string]interface{}{"type": "string"}}},
		{name: "pattern properties", schema: map[string]interface{}{"type": "object", "properties": map[string]interface{}{}, "patternProperties": map[string]interface{}{}}},
		{name: "dependent schema", schema: map[string]interface{}{"type": "object", "properties": map[string]interface{}{}, "dependentSchemas": map[string]interface{}{}}},
		{name: "composed schema", schema: map[string]interface{}{"type": "object", "allOf": []interface{}{map[string]interface{}{"type": "object"}}}},
		{name: "reference", schema: map[string]interface{}{"type": "object", "$ref": "#/$defs/input"}},
		{name: "array", schema: map[string]interface{}{"type": "array"}},
		{name: "uppercase object", schema: map[string]interface{}{"type": "OBJECT", "properties": map[string]interface{}{}}},
		{name: "padded object", schema: map[string]interface{}{"type": " object ", "properties": map[string]interface{}{}}},
		{name: "missing type", schema: map[string]interface{}{"properties": map[string]interface{}{}}},
		{name: "malformed properties", schema: map[string]interface{}{"type": "object", "properties": []interface{}{}}},
		{name: "missing schema", schema: nil},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyToolInputPolicy(tc.schema); got != tc.want {
				t.Fatalf("classifyToolInputPolicy() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestClaudeToKiroPreservesOriginalToolInputPolicies(t *testing.T) {
	req := &ClaudeRequest{Model: "claude-sonnet-4.6", Tools: []ClaudeTool{
		{Name: "closed_empty", InputSchema: map[string]interface{}{"type": "object", "properties": map[string]interface{}{}, "additionalProperties": false}},
		{Name: "declared_empty", InputSchema: map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}},
		{Name: "open_empty", InputSchema: map[string]interface{}{"type": "object", "properties": map[string]interface{}{}, "additionalProperties": true}},
	}}
	payload := ClaudeToKiro(req, false)
	context := payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext
	if context == nil || len(context.Tools) != 3 {
		t.Fatalf("translated Claude tools = %#v", context)
	}
	closedName := context.Tools[0].ToolSpecification.Name
	declaredName := context.Tools[1].ToolSpecification.Name
	openName := context.Tools[2].ToolSpecification.Name
	if payload.toolInputPolicies[closedName] != toolInputPolicyClosedEmpty ||
		payload.toolInputPolicies[declaredName] != toolInputPolicyDeclaredEmpty ||
		payload.toolInputPolicies[openName] != toolInputPolicyNone {
		t.Fatalf("original Claude schema policies were not preserved: %#v", payload.toolInputPolicies)
	}
	for _, wrapper := range context.Tools {
		schema := wrapper.ToolSpecification.InputSchema.JSON.(map[string]interface{})
		if _, exists := schema["additionalProperties"]; exists {
			t.Fatalf("Kiro schema still contains additionalProperties: %#v", schema)
		}
	}
}

func TestOpenAIToKiroPreservesOriginalToolInputPolicies(t *testing.T) {
	tool := func(name string, schema interface{}) OpenAITool {
		var value OpenAITool
		value.Type = "function"
		value.Function.Name = name
		value.Function.Parameters = schema
		return value
	}
	payload := OpenAIToKiro(&OpenAIRequest{Model: "claude-sonnet-4.6", Tools: []OpenAITool{
		tool("closed_empty", map[string]interface{}{"type": "object", "properties": map[string]interface{}{}, "additionalProperties": false}),
		tool("open_empty", map[string]interface{}{"type": "object", "properties": map[string]interface{}{}, "additionalProperties": true}),
	}}, false)
	if payload.toolInputPolicies["closed_empty"] != toolInputPolicyClosedEmpty || payload.toolInputPolicies["open_empty"] != toolInputPolicyNone {
		t.Fatalf("original OpenAI schema policies were not preserved: %#v", payload.toolInputPolicies)
	}
}

func TestEventStreamParseOptionsUseUpstreamToolNames(t *testing.T) {
	payload := payloadWithTestTool("mcpMemoryReadGraphH123", map[string]interface{}{
		"type":                 "object",
		"properties":           map[string]interface{}{},
		"additionalProperties": false,
	})
	payload.ToolNameMap = map[string]string{"mcpMemoryReadGraphH123": "mcp__memory__read_graph"}

	options := eventStreamParseOptionsForPayload(payload)
	if !options.allowsEmptyToolInput("mcpMemoryReadGraphH123", toolUseRecoveryCleanEOF) {
		t.Fatal("sanitized upstream tool name was not registered")
	}
	if options.allowsEmptyToolInput("mcp__memory__read_graph", toolUseRecoveryExplicitToolUse) {
		t.Fatal("client-facing tool name must not be used to match upstream events")
	}
}

func TestEventStreamParseOptionsRejectConflictingDuplicateToolNames(t *testing.T) {
	payload := ClaudeToKiro(&ClaudeRequest{Model: "claude-sonnet-4.6", Tools: []ClaudeTool{
		{Name: "duplicateTool", InputSchema: map[string]interface{}{
			"type": "object", "properties": map[string]interface{}{}, "additionalProperties": false,
		}},
		{Name: "duplicateTool", InputSchema: map[string]interface{}{
			"type": "object", "properties": map[string]interface{}{
				"path": map[string]interface{}{"type": "string"},
			},
		}},
	}}, false)
	context := payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext
	if context == nil || len(context.Tools) != 2 {
		t.Fatalf("translated duplicate tools = %#v", context)
	}
	upstreamName := context.Tools[0].ToolSpecification.Name

	options := eventStreamParseOptionsForPayload(payload)
	if options.allowsEmptyToolInput(upstreamName, toolUseRecoveryExplicitToolUse) {
		t.Fatal("conflicting duplicate tool schemas must disable empty-input recovery")
	}
}

func payloadWithTestTool(name string, schema interface{}) *KiroPayload {
	payload := &KiroPayload{}
	tool := KiroToolWrapper{}
	tool.ToolSpecification.Name = name
	tool.ToolSpecification.InputSchema = InputSchema{JSON: schema}
	payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext = &UserInputMessageContext{
		Tools: []KiroToolWrapper{tool},
	}
	payload.toolInputPolicies = map[string]toolInputPolicy{name: classifyToolInputPolicy(schema)}
	return payload
}
