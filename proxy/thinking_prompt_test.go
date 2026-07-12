package proxy

import (
	"strings"
	"testing"
)

func TestBuildThinkingPromptUsesClientBudgetAndCap(t *testing.T) {
	prompt := buildThinkingPrompt(
		&ClaudeThinkingConfig{Type: "enabled", BudgetTokens: 12000},
		nil,
		32000,
		4000,
		10000,
	)
	if !strings.Contains(prompt, "<thinking_mode>enabled</thinking_mode>") ||
		!strings.Contains(prompt, "<max_thinking_length>10000</max_thinking_length>") {
		t.Fatalf("unexpected enabled prompt: %q", prompt)
	}
}

func TestBuildThinkingPromptBoundsAdaptiveMode(t *testing.T) {
	prompt := buildThinkingPrompt(
		&ClaudeThinkingConfig{Type: "adaptive"},
		&ClaudeOutputConfig{Effort: "medium"},
		32000,
		4000,
		10000,
	)
	want := "<thinking_mode>enabled</thinking_mode>\n<max_thinking_length>4000</max_thinking_length>"
	if prompt != want {
		t.Fatalf("unexpected bounded adaptive prompt: got %q want %q", prompt, want)
	}
}

func TestBuildThinkingPromptDefaultsAdaptiveToMediumBudget(t *testing.T) {
	prompt := buildThinkingPrompt(
		&ClaudeThinkingConfig{Type: "adaptive"},
		nil,
		32000,
		4000,
		10000,
	)
	if !strings.Contains(prompt, "<max_thinking_length>4000</max_thinking_length>") {
		t.Fatalf("unexpected adaptive default: %q", prompt)
	}
}

func TestBuildThinkingPromptUsesSafeDefault(t *testing.T) {
	prompt := buildThinkingPrompt(nil, nil, 32000, 0, 10000)
	if !strings.Contains(prompt, "<max_thinking_length>4000</max_thinking_length>") {
		t.Fatalf("unexpected default prompt: %q", prompt)
	}
}

func TestBuildThinkingPromptKeepsFinalResponseHeadroom(t *testing.T) {
	prompt := buildThinkingPrompt(
		&ClaudeThinkingConfig{Type: "enabled", BudgetTokens: 10000},
		nil,
		8000,
		4000,
		0,
	)
	if !strings.Contains(prompt, "<max_thinking_length>6000</max_thinking_length>") {
		t.Fatalf("expected 25%% output headroom, got %q", prompt)
	}
}

func TestBuildThinkingPromptDisabled(t *testing.T) {
	if prompt := buildThinkingPrompt(&ClaudeThinkingConfig{Type: "disabled"}, nil, 32000, 4000, 10000); prompt != "" {
		t.Fatalf("disabled thinking produced a prompt: %q", prompt)
	}
}

func TestClaudeThinkingPromptUsesMinimalBudgetWhenToolIsRequired(t *testing.T) {
	req := &ClaudeRequest{
		MaxTokens:      32000,
		Thinking:       &ClaudeThinkingConfig{Type: "adaptive", Effort: "high"},
		RequireToolUse: true,
	}
	prompt := claudeThinkingPrompt(req, true)
	if !strings.Contains(prompt, "<max_thinking_length>1024</max_thinking_length>") {
		t.Fatalf("unexpected required-tool thinking prompt: %q", prompt)
	}
}
