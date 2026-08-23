package proxy

import (
	"path/filepath"
	"testing"

	"kiro-go/config"
)

func TestModelIDForAPI(t *testing.T) {
	tests := []struct {
		name     string
		model    string
		enabled  bool
		expected string
	}{
		{name: "minor version", model: "claude-opus-4.8", enabled: true, expected: "claude-opus-4-8"},
		{name: "thinking suffix", model: "claude-sonnet-4.6-thinking", enabled: true, expected: "claude-sonnet-4-6-thinking"},
		{name: "dated snapshot", model: "claude-haiku-4.5-20251001", enabled: true, expected: "claude-haiku-4-5-20251001"},
		{name: "latest suffix", model: "claude-opus-4.8-latest", enabled: true, expected: "claude-opus-4-8-latest"},
		{name: "already official", model: "claude-opus-4-8", enabled: true, expected: "claude-opus-4-8"},
		{name: "major version", model: "claude-opus-5", enabled: true, expected: "claude-opus-5"},
		{name: "gpt version", model: "gpt-5.6-sol", enabled: true, expected: "gpt-5.6-sol"},
		{name: "disabled", model: "claude-opus-4.8", enabled: false, expected: "claude-opus-4.8"},
		{name: "official disabled", model: "claude-opus-4-8", enabled: false, expected: "claude-opus-4.8"},
		{name: "official thinking disabled", model: "claude-sonnet-4-6-thinking", enabled: false, expected: "claude-sonnet-4.6-thinking"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := modelIDForAPI(test.model, test.enabled); got != test.expected {
				t.Fatalf("modelIDForAPI(%q, %t) = %q, want %q", test.model, test.enabled, got, test.expected)
			}
		})
	}
}

func TestResponseTranslatorsHonorOfficialModelNameSetting(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}

	assertModels := func(want string) {
		t.Helper()
		claude := KiroToClaudeResponse("ok", "", false, nil, 1, 1, "claude-opus-4.8")
		openAI := KiroToOpenAIResponse("ok", nil, 1, 1, "claude-opus-4.8")
		if claude.Model != want || openAI.Model != want {
			t.Fatalf("response models = Claude %q, OpenAI %q; want %q", claude.Model, openAI.Model, want)
		}
	}

	assertModels("claude-opus-4-8")
	registry := config.GetModelRegistryConfig()
	disabled := false
	registry.UseOfficialModelNames = &disabled
	if err := config.UpdateModelRegistryConfig(registry); err != nil {
		t.Fatalf("disable official names: %v", err)
	}
	assertModels("claude-opus-4.8")
}

func TestOfficialModelNamesDedupeDynamicDotAndDashForms(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	models := buildAnthropicModelsResponse([]ModelInfo{
		{ModelId: "claude-opus-4.8"},
		{ModelId: "claude-opus-4-8"},
	}, "-thinking", true)
	models = dedupeModelResponse(models)
	if len(models) != 2 {
		t.Fatalf("expected one base and one thinking model after dedupe, got %#v", models)
	}
	if models[0]["id"] != "claude-opus-4-8" || models[1]["id"] != "claude-opus-4-8-thinking" {
		t.Fatalf("unexpected official model IDs: %#v", models)
	}
}

func TestModelListCanUseKiroDotNames(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	models := buildAnthropicModelsResponse([]ModelInfo{
		{ModelId: "claude-sonnet-4.6"},
		{ModelId: "claude-opus-4-8"},
	}, "-thinking", false)
	if len(models) != 4 || models[0]["id"] != "claude-sonnet-4.6" || models[1]["id"] != "claude-sonnet-4.6-thinking" || models[2]["id"] != "claude-opus-4.8" || models[3]["id"] != "claude-opus-4.8-thinking" {
		t.Fatalf("unexpected dot-form model list: %#v", models)
	}
}

func TestConfiguredModelPresentationPreservesCustomMapping(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	configured := []config.ModelEntry{
		{ID: "claude-opus-4.8", KiroModelID: "claude-haiku-4.5", ContextWindow: 200000, MaxTokens: 8192},
		{ID: "claude-sonnet-4.6", KiroModelID: "claude-sonnet-4.6", ContextWindow: 200000, MaxTokens: 8192},
	}
	models := mergeConfiguredModels(nil, configured, "-thinking", true)
	if len(models) != 4 {
		t.Fatalf("expected two configured models and thinking variants, got %#v", models)
	}
	if models[0]["id"] != "claude-opus-4.8" {
		t.Fatalf("custom cross-model alias was rewritten: %#v", models[0])
	}
	if models[2]["id"] != "claude-sonnet-4-6" {
		t.Fatalf("direct Kiro model mapping was not canonicalized: %#v", models[2])
	}
	dotModels := mergeConfiguredModels(nil, configured, "-thinking", false)
	if dotModels[0]["id"] != "claude-opus-4.8" || dotModels[2]["id"] != "claude-sonnet-4.6" {
		t.Fatalf("dot-form presentation changed configured aliases: %#v", dotModels)
	}
}
