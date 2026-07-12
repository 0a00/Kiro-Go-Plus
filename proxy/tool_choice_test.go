package proxy

import (
	"encoding/json"
	"strings"
	"testing"
)

func openAITestTool(name string) OpenAITool {
	tool := OpenAITool{Type: "function"}
	tool.Function.Name = name
	tool.Function.Parameters = map[string]interface{}{"type": "object"}
	return tool
}

func TestApplyOpenAIToolChoiceSpecificFunction(t *testing.T) {
	req := &OpenAIRequest{
		Messages:   []OpenAIMessage{{Role: "system", Content: "base"}, {Role: "user", Content: "run"}},
		Tools:      []OpenAITool{openAITestTool("one"), openAITestTool("two")},
		ToolChoice: json.RawMessage(`{"type":"function","function":{"name":"two"}}`),
	}
	if err := applyOpenAIToolChoice(req); err != nil {
		t.Fatalf("apply tool choice: %v", err)
	}
	if len(req.Tools) != 1 || req.Tools[0].Function.Name != "two" {
		t.Fatalf("expected selected tool only, got %+v", req.Tools)
	}
	if len(req.Messages) != 3 || req.Messages[1].Role != "system" || !strings.Contains(req.Messages[1].Content.(string), "two") {
		t.Fatalf("expected tool constraint after system prompt, got %+v", req.Messages)
	}
}

func TestApplyOpenAIToolChoiceNoneRemovesTools(t *testing.T) {
	req := &OpenAIRequest{Tools: []OpenAITool{openAITestTool("one")}, ToolChoice: json.RawMessage(`"none"`)}
	if err := applyOpenAIToolChoice(req); err != nil {
		t.Fatalf("apply tool choice: %v", err)
	}
	if len(req.Tools) != 0 {
		t.Fatalf("expected tools to be removed, got %+v", req.Tools)
	}
}

func TestApplyOpenAIToolChoiceRejectsUnknownFunction(t *testing.T) {
	req := &OpenAIRequest{Tools: []OpenAITool{openAITestTool("one")}, ToolChoice: json.RawMessage(`{"type":"function","name":"missing"}`)}
	if err := applyOpenAIToolChoice(req); err == nil {
		t.Fatal("expected unknown function to be rejected")
	}
}

func TestOpenAIExplicitZeroTemperatureIsPreserved(t *testing.T) {
	var req OpenAIRequest
	if err := json.Unmarshal([]byte(`{"model":"test","messages":[{"role":"user","content":"hi"}],"temperature":0}`), &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if req.Temperature == nil || *req.Temperature != 0 {
		t.Fatalf("expected explicit zero temperature, got %#v", req.Temperature)
	}
	payload := OpenAIToKiro(&req, false)
	if payload.InferenceConfig == nil || payload.InferenceConfig.Temperature == nil || *payload.InferenceConfig.Temperature != 0 {
		t.Fatalf("explicit zero temperature was dropped: %+v", payload.InferenceConfig)
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	if !strings.Contains(string(raw), `"temperature":0`) {
		t.Fatalf("upstream payload omitted zero temperature: %s", raw)
	}
}
