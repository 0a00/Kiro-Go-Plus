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

func TestAdaptiveThinkingModelFallbacks(t *testing.T) {
	for _, model := range []string{
		"claude-opus-4.6", "claude-opus-4.7", "claude-opus-4.8",
		"claude-sonnet-5", "claude-opus-5",
	} {
		if !supportsAdaptiveThinking(model) {
			t.Errorf("model %q should support adaptive thinking", model)
		}
	}
	for _, model := range []string{"claude-opus-4.5", "claude-sonnet-4.6", "claude-haiku-4.5"} {
		if supportsAdaptiveThinking(model) {
			t.Errorf("legacy model %q unexpectedly uses adaptive fallback", model)
		}
	}
}

func TestOpus4ThinkingSuffixUsesSyntheticAdaptiveMode(t *testing.T) {
	for _, model := range []string{"claude-opus-4.6", "claude-opus-4.7", "claude-opus-4.8"} {
		req := &ClaudeRequest{Model: model, MaxTokens: 4096}
		prompt := claudeThinkingPrompt(req, true)
		if prompt != "<thinking_mode>adaptive</thinking_mode>\n<thinking_effort>high</thinking_effort>" {
			t.Errorf("model %q produced %q", model, prompt)
		}
	}
}

func TestOpenAIOpus4ThinkingSuffixUsesSyntheticAdaptiveMode(t *testing.T) {
	for _, model := range []string{"claude-opus-4.6", "claude-opus-4.7", "claude-opus-4.8"} {
		req := &OpenAIRequest{Model: model, MaxTokens: 4096, ReasoningEffort: effortMedium}
		prompt := openAIThinkingPrompt(req, true)
		if prompt != "<thinking_mode>adaptive</thinking_mode>\n<thinking_effort>medium</thinking_effort>" {
			t.Errorf("model %q produced %q", model, prompt)
		}
	}
	legacy := openAIThinkingPrompt(&OpenAIRequest{Model: "claude-opus-4.5", MaxTokens: 4096}, true)
	if !strings.Contains(legacy, "<thinking_mode>enabled</thinking_mode>") {
		t.Fatalf("legacy OpenAI suffix lost budget fallback: %q", legacy)
	}
}

func TestDiscoveredOpus4EffortMetadataTakesPriority(t *testing.T) {
	const model = "claude-opus-4.8"
	discoveredModelMetadata.Delete(model)
	t.Cleanup(func() { discoveredModelMetadata.Delete(model) })
	rememberDiscoveredModelMetadata([]ModelInfo{{
		ModelId: model, EffortLevels: []string{effortLow, effortHigh}, EffortSchemaPath: "reasoning",
	}})
	req := &ClaudeRequest{
		Model: model, Thinking: &ClaudeThinkingConfig{Type: "adaptive", Effort: effortLow},
	}
	(&Handler{}).prepareClaudeNativeEffort(req, true)
	if req.NativeEffort != effortLow || req.NativeEffortPath != "reasoning" {
		t.Fatalf("discovered metadata was not preferred: effort=%q path=%q", req.NativeEffort, req.NativeEffortPath)
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

func TestClaudeSonnet5UsesNativeAdaptiveFallback(t *testing.T) {
	h := &Handler{}
	req := &ClaudeRequest{
		Model:     "claude-sonnet-5",
		MaxTokens: 4096,
		Messages:  []ClaudeMessage{{Role: "user", Content: "hello"}},
		Thinking:  &ClaudeThinkingConfig{Type: "adaptive"},
	}
	h.prepareClaudeNativeEffort(req, true)
	if req.NativeEffort != effortHigh || req.NativeEffortPath != "output_config" {
		t.Fatalf("unexpected Sonnet 5 fallback: effort=%q path=%q", req.NativeEffort, req.NativeEffortPath)
	}
	payload := ClaudeToKiro(req, true)
	outputConfig, ok := payload.AdditionalModelRequestFields["output_config"].(map[string]interface{})
	if !ok || outputConfig["effort"] != effortHigh {
		t.Fatalf("Sonnet 5 adaptive effort missing from payload: %#v", payload.AdditionalModelRequestFields)
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

func TestFallbackModelsAdvertiseGeneration5Limits(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	models := fallbackAnthropicModels("-thinking", true)
	for _, id := range []string{"claude-opus-5", "claude-opus-5-thinking", "claude-sonnet-5", "claude-sonnet-5-thinking"} {
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
		if strings.HasPrefix(id, "claude-opus-5") && found["prompt_cache_min_tokens"] != 512 {
			t.Fatalf("unexpected Opus 5 cache metadata for %q: %#v", id, found)
		}
	}
}

func TestFallbackModelsIncludeGPT56WithoutGuessedLimits(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	models := fallbackAnthropicModels("-thinking", true)
	for _, id := range []string{"gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna"} {
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
		if _, exists := found["context_window"]; exists {
			t.Fatalf("uncertain GPT-5.6 context limit should not be guessed: %#v", found)
		}
	}
}
