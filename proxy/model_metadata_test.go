package proxy

import (
	"encoding/json"
	"kiro-go/config"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestNormalizeModelInfoExtractsEffortSchema(t *testing.T) {
	model := ModelInfo{
		ModelId: "claude-opus-5",
		AdditionalModelRequestFieldsSchema: map[string]interface{}{
			"properties": map[string]interface{}{
				"reasoning": map[string]interface{}{
					"properties": map[string]interface{}{
						"effort": map[string]interface{}{
							"enum": []interface{}{"low", "medium", "high", "xhigh", "max"},
						},
					},
				},
			},
		},
	}
	normalizeModelInfo(&model)
	if model.EffortSchemaPath != "reasoning" {
		t.Fatalf("effort schema path = %q, want reasoning", model.EffortSchemaPath)
	}
	want := []string{"low", "medium", "high", "xhigh", "max"}
	if !reflect.DeepEqual(model.EffortLevels, want) {
		t.Fatalf("effort levels = %#v, want %#v", model.EffortLevels, want)
	}
}

func TestResolveSupportedEffortUsesClosestAdvertisedLevel(t *testing.T) {
	got, ok := resolveSupportedEffort("xhigh", []string{"low", "medium", "high", "max"})
	if !ok || got != "max" {
		t.Fatalf("resolved effort = %q ok=%v, want max", got, ok)
	}
	got, ok = resolveSupportedEffort("max", []string{"low", "medium", "high", "xhigh"})
	if !ok || got != "xhigh" {
		t.Fatalf("resolved effort = %q ok=%v, want xhigh", got, ok)
	}
}

func TestClaudeOpus5UsesNativeKiroEffort(t *testing.T) {
	h := &Handler{}
	req := &ClaudeRequest{
		Model:        "claude-opus-5",
		MaxTokens:    4096,
		Messages:     []ClaudeMessage{{Role: "user", Content: "hello"}},
		Thinking:     &ClaudeThinkingConfig{Type: "adaptive"},
		OutputConfig: &ClaudeOutputConfig{Effort: "xhigh"},
	}
	h.prepareClaudeNativeEffort(req, true)
	if req.NativeEffort != "xhigh" || req.NativeEffortPath != "output_config" {
		t.Fatalf("unexpected native effort selection: effort=%q path=%q", req.NativeEffort, req.NativeEffortPath)
	}
	payload := ClaudeToKiro(req, true)
	outputConfig, ok := payload.AdditionalModelRequestFields["output_config"].(map[string]interface{})
	if !ok || outputConfig["effort"] != "xhigh" {
		t.Fatalf("native effort missing from payload: %#v", payload.AdditionalModelRequestFields)
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	if strings.Contains(string(raw), "thinking_mode") || strings.Contains(string(raw), "max_thinking_length") {
		t.Fatalf("native effort payload still contains synthetic thinking tags: %s", raw)
	}
}

func TestLegacyModelKeepsSyntheticThinkingFallback(t *testing.T) {
	h := &Handler{cachedModels: []ModelInfo{{ModelId: "claude-haiku-4.5"}}}
	req := &ClaudeRequest{
		Model:     "claude-haiku-4.5",
		MaxTokens: 4096,
		Messages:  []ClaudeMessage{{Role: "user", Content: "hello"}},
		Thinking:  &ClaudeThinkingConfig{Type: "adaptive"},
	}
	h.prepareClaudeNativeEffort(req, true)
	if req.NativeEffort != "" {
		t.Fatalf("legacy model unexpectedly selected native effort %q", req.NativeEffort)
	}
	payload := ClaudeToKiro(req, true)
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	if !strings.Contains(string(raw), "thinking_mode") {
		t.Fatalf("legacy model lost synthetic thinking fallback: %s", raw)
	}
}

func TestOpenAIReasoningEffortUsesNativeKiroField(t *testing.T) {
	h := &Handler{}
	req := &OpenAIRequest{
		Model:           "claude-opus-5",
		Messages:        []OpenAIMessage{{Role: "user", Content: "hello"}},
		ReasoningEffort: "max",
	}
	h.prepareOpenAINativeEffort(req, true)
	payload := OpenAIToKiro(req, true)
	outputConfig, ok := payload.AdditionalModelRequestFields["output_config"].(map[string]interface{})
	if !ok || outputConfig["effort"] != "max" {
		t.Fatalf("OpenAI effort missing from payload: %#v", payload.AdditionalModelRequestFields)
	}
}

func TestFallbackModelsAdvertiseOpus5Limits(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	models := fallbackAnthropicModels("-thinking")
	for _, id := range []string{"claude-opus-5", "claude-opus-5-thinking"} {
		var found map[string]interface{}
		for _, model := range models {
			if model["id"] == id {
				found = model
				break
			}
		}
		if found == nil {
			t.Fatalf("fallback model %q not found", id)
		}
		if found["context_window"] != 1_000_000 || found["max_output_tokens"] != 128_000 {
			t.Fatalf("unexpected Opus 5 limits for %q: %#v", id, found)
		}
		if found["prompt_cache_min_tokens"] != 512 {
			t.Fatalf("unexpected Opus 5 cache metadata for %q: %#v", id, found)
		}
	}
}
