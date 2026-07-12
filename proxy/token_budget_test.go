package proxy

import (
	"kiro-go/config"
	"path/filepath"
	"testing"
)

func initTokenBudgetTestConfig(t *testing.T, maxOutput, contextWindow int) {
	t.Helper()
	tempDir := t.TempDir()
	if err := config.Init(filepath.Join(tempDir, "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	t.Cleanup(func() { _ = config.Init(filepath.Join(tempDir, "reset.json")) })
	if err := config.UpdateThinkingConfig("-thinking", "reasoning_content", "thinking", 4000, 10000, maxOutput, contextWindow, true, true); err != nil {
		t.Fatalf("UpdateThinkingConfig: %v", err)
	}
}

func TestClaudeTokenBudgetDefaultsRespectClientValues(t *testing.T) {
	initTokenBudgetTestConfig(t, 64000, 1000000)
	req := &ClaudeRequest{
		Model:           "claude-sonnet-4.6",
		MaxTokens:       12000,
		MaxOutputTokens: 32000,
		ContextWindow:   300000,
		MaxInputTokens:  500000,
	}

	contextWindow := applyClaudeTokenBudgetDefaults(req)
	if req.MaxTokens != 12000 || contextWindow != 300000 {
		t.Fatalf("client values did not win: max=%d context=%d", req.MaxTokens, contextWindow)
	}
}

func TestClaudeTokenBudgetDefaultsFillOmittedValues(t *testing.T) {
	initTokenBudgetTestConfig(t, 64000, 500000)
	req := &ClaudeRequest{Model: "claude-sonnet-4.6"}

	contextWindow := applyClaudeTokenBudgetDefaults(req)
	if req.MaxTokens != 64000 || contextWindow != 500000 {
		t.Fatalf("unexpected defaults: max=%d context=%d", req.MaxTokens, contextWindow)
	}
	payload := ClaudeToKiro(req, false)
	if payload.InferenceConfig == nil || payload.InferenceConfig.MaxTokens != 64000 {
		t.Fatalf("default max output was not sent upstream: %+v", payload.InferenceConfig)
	}
}

func TestExplicitThinkingBudgetRaisesOnlyServerDefaultOutput(t *testing.T) {
	initTokenBudgetTestConfig(t, 8000, 500000)
	req := &ClaudeRequest{
		Model:    "claude-sonnet-4.6",
		Thinking: &ClaudeThinkingConfig{Type: "enabled", BudgetTokens: 12000},
	}
	applyClaudeTokenBudgetDefaults(req)
	if req.MaxTokens <= req.Thinking.BudgetTokens {
		t.Fatalf("server default did not make room for explicit thinking budget: %d", req.MaxTokens)
	}

	explicit := &ClaudeRequest{
		Model:     "claude-sonnet-4.6",
		MaxTokens: 8000,
		Thinking:  &ClaudeThinkingConfig{Type: "enabled", BudgetTokens: 12000},
	}
	applyClaudeTokenBudgetDefaults(explicit)
	if explicit.MaxTokens != 8000 {
		t.Fatalf("explicit max_tokens was overridden: %d", explicit.MaxTokens)
	}
}

func TestOpenAITokenBudgetAliasesRespectPrecedence(t *testing.T) {
	initTokenBudgetTestConfig(t, 64000, 500000)
	req := &OpenAIRequest{
		Model:               "claude-sonnet-4.6",
		MaxCompletionTokens: 16000,
		MaxOutputTokens:     32000,
		MaxInputTokens:      250000,
	}

	contextWindow := applyOpenAITokenBudgetDefaults(req)
	if req.MaxTokens != 16000 || contextWindow != 250000 {
		t.Fatalf("unexpected alias precedence: max=%d context=%d", req.MaxTokens, contextWindow)
	}
	payload := OpenAIToKiro(req, false)
	if payload.InferenceConfig == nil || payload.InferenceConfig.MaxTokens != 16000 {
		t.Fatalf("OpenAI max output was not sent upstream: %+v", payload.InferenceConfig)
	}
}

func TestConfiguredModelBudgetPrecedesGlobalDefault(t *testing.T) {
	initTokenBudgetTestConfig(t, 64000, 500000)
	if err := config.UpdateModelRegistryConfig(config.ModelRegistryConfig{
		NegativeCacheTTLSeconds: 600,
		Models: []config.ModelEntry{{
			ID: "custom", KiroModelID: "claude-sonnet-4.6", ContextWindow: 750000, MaxTokens: 24000,
		}},
	}); err != nil {
		t.Fatalf("UpdateModelRegistryConfig: %v", err)
	}
	req := &ClaudeRequest{Model: "custom-thinking"}

	contextWindow := applyClaudeTokenBudgetDefaults(req)
	if req.MaxTokens != 24000 || contextWindow != 750000 {
		t.Fatalf("configured model budget did not win: max=%d context=%d", req.MaxTokens, contextWindow)
	}
}

func TestPayloadContextWindowOverridesModelDefault(t *testing.T) {
	initTokenBudgetTestConfig(t, 0, 500000)
	payload := &KiroPayload{contextWindowTokens: 300000}
	if got := getPayloadContextWindowSize(payload, "claude-sonnet-4.6"); got != 300000 {
		t.Fatalf("unexpected payload context window: %d", got)
	}
}

func TestModelInfoAdvertisesGlobalTokenDefaults(t *testing.T) {
	initTokenBudgetTestConfig(t, 64000, 500000)
	model := buildModelInfoWithLimits("unknown-budget-model", "test", false, 200000, 8000)
	if model["context_window"] != 500000 || model["max_output_tokens"] != 64000 {
		t.Fatalf("unexpected model token defaults: %+v", model)
	}
}
