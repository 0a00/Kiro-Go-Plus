package proxy

import (
	"encoding/json"
	"testing"
)

func TestSchemaDefinesZeroArgumentsConservatively(t *testing.T) {
	tests := []struct {
		name   string
		schema interface{}
		want   bool
	}{
		{name: "object only is open", schema: map[string]interface{}{"type": "object"}},
		{name: "empty schema is open", schema: map[string]interface{}{}},
		{name: "empty properties", schema: map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}, want: true},
		{name: "empty required", schema: map[string]interface{}{"type": "object", "properties": map[string]interface{}{}, "required": []interface{}{}, "minProperties": json.Number("0")}, want: true},
		{name: "closed empty object", schema: map[string]interface{}{"type": "object", "properties": map[string]interface{}{}, "additionalProperties": false, "maxProperties": float64(0)}, want: true},
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
		{name: "malformed properties", schema: map[string]interface{}{"type": "object", "properties": []interface{}{}}},
		{name: "missing schema", schema: nil},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := schemaDefinesZeroArguments(tc.schema); got != tc.want {
				t.Fatalf("schemaDefinesZeroArguments() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestEventStreamParseOptionsUseUpstreamToolNames(t *testing.T) {
	payload := payloadWithTestTool("mcpMemoryReadGraphH123", map[string]interface{}{
		"type":       "object",
		"properties": map[string]interface{}{},
	})
	payload.ToolNameMap = map[string]string{"mcpMemoryReadGraphH123": "mcp__memory__read_graph"}

	options := eventStreamParseOptionsForPayload(payload)
	if !options.allowsEmptyToolInput("mcpMemoryReadGraphH123") {
		t.Fatal("sanitized upstream tool name was not registered")
	}
	if options.allowsEmptyToolInput("mcp__memory__read_graph") {
		t.Fatal("client-facing tool name must not be used to match upstream events")
	}
}

func TestEventStreamParseOptionsRejectConflictingDuplicateToolNames(t *testing.T) {
	payload := payloadWithTestTool("duplicateTool", map[string]interface{}{
		"type": "object", "properties": map[string]interface{}{},
	})
	parameterized := KiroToolWrapper{}
	parameterized.ToolSpecification.Name = "duplicateTool"
	parameterized.ToolSpecification.InputSchema = InputSchema{JSON: map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"path": map[string]interface{}{"type": "string"},
		},
	}}
	context := payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext
	context.Tools = append(context.Tools, parameterized)

	options := eventStreamParseOptionsForPayload(payload)
	if options.allowsEmptyToolInput("duplicateTool") {
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
	return payload
}
