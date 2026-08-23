package proxy

import (
	"strings"
	"testing"
)

func TestBuildThinkingPromptUsesExplicitClientBudget(t *testing.T) {
	prompt := buildThinkingPrompt(
		&ClaudeThinkingConfig{Type: "enabled", BudgetTokens: 12000},
		nil,
		32000,
		4000,
		10000,
	)
	if !strings.Contains(prompt, "<thinking_mode>enabled</thinking_mode>") ||
		!strings.Contains(prompt, "<max_thinking_length>12000</max_thinking_length>") {
		t.Fatalf("unexpected enabled prompt: %q", prompt)
	}
}

func TestBuildThinkingPromptUsesAdaptiveEffort(t *testing.T) {
	prompt := buildThinkingPrompt(
		&ClaudeThinkingConfig{Type: "adaptive"},
		&ClaudeOutputConfig{Effort: "medium"},
		32000,
		4000,
		10000,
	)
	want := "<thinking_mode>adaptive</thinking_mode>\n<thinking_effort>medium</thinking_effort>"
	if prompt != want {
		t.Fatalf("unexpected adaptive prompt: got %q want %q", prompt, want)
	}
}

func TestBuildThinkingPromptDefaultsAdaptiveToHighEffort(t *testing.T) {
	prompt := buildThinkingPrompt(
		&ClaudeThinkingConfig{Type: "adaptive"},
		nil,
		32000,
		4000,
		10000,
	)
	if prompt != "<thinking_mode>adaptive</thinking_mode>\n<thinking_effort>high</thinking_effort>" {
		t.Fatalf("unexpected adaptive default: %q", prompt)
	}
}

func TestBuildThinkingPromptUsesSafeDefault(t *testing.T) {
	prompt := buildThinkingPrompt(nil, nil, 32000, 0, 10000)
	if !strings.Contains(prompt, "<max_thinking_length>4000</max_thinking_length>") {
		t.Fatalf("unexpected default prompt: %q", prompt)
	}
}

func TestBuildThinkingPromptKeepsHeadroomForProxyDefault(t *testing.T) {
	prompt := buildThinkingPrompt(
		nil,
		nil,
		8000,
		10000,
		0,
	)
	if !strings.Contains(prompt, "<max_thinking_length>6000</max_thinking_length>") {
		t.Fatalf("expected 25%% output headroom, got %q", prompt)
	}
}

func TestClaudeThinkingPromptPreservesExplicitBudgetWhenToolIsRequired(t *testing.T) {
	req := &ClaudeRequest{
		Model:          "claude-opus-4.8",
		MaxTokens:      32000,
		Thinking:       &ClaudeThinkingConfig{Type: "enabled", BudgetTokens: 12000},
		RequireToolUse: true,
	}
	prompt := claudeThinkingPrompt(req, true)
	if !strings.Contains(prompt, "<max_thinking_length>12000</max_thinking_length>") {
		t.Fatalf("explicit client budget was overridden: %q", prompt)
	}
}

func TestBuildThinkingPromptDisabled(t *testing.T) {
	if prompt := buildThinkingPrompt(&ClaudeThinkingConfig{Type: "disabled"}, nil, 32000, 4000, 10000); prompt != "" {
		t.Fatalf("disabled thinking produced a prompt: %q", prompt)
	}
}

func TestClaudeThinkingPromptKeepsAdaptiveModeWhenToolIsRequired(t *testing.T) {
	req := &ClaudeRequest{
		Model:          "claude-opus-4.8",
		MaxTokens:      32000,
		Thinking:       &ClaudeThinkingConfig{Type: "adaptive", Effort: "high"},
		RequireToolUse: true,
	}
	prompt := claudeThinkingPrompt(req, true)
	if prompt != "<thinking_mode>adaptive</thinking_mode>\n<thinking_effort>high</thinking_effort>" {
		t.Fatalf("unexpected required-tool thinking prompt: %q", prompt)
	}
}

func TestClaudeLegacySuffixUsesMinimalBudgetWhenToolIsRequired(t *testing.T) {
	req := &ClaudeRequest{
		Model:          "claude-haiku-4.5",
		MaxTokens:      32000,
		RequireToolUse: true,
	}
	prompt := claudeThinkingPrompt(req, true)
	if !strings.Contains(prompt, "<thinking_mode>enabled</thinking_mode>") ||
		!strings.Contains(prompt, "<max_thinking_length>1024</max_thinking_length>") {
		t.Fatalf("unexpected legacy required-tool prompt: %q", prompt)
	}
}
