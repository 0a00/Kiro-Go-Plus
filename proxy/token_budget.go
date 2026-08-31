package proxy

import (
	"fmt"
	"kiro-go/config"
	"strings"
)

// normalizeClaudeThinkingBudget reconciles the two Claude limits before the
// request reaches Kiro. Anthropic requires budget_tokens < max_tokens. Clients
// such as Claude Code can legitimately send a smaller max_tokens together with
// a proxy/default thinking budget, which used to become a deterministic 400.
//
// A client-provided max_tokens is authoritative, so we lower only the thinking
// budget (rather than silently increasing the requested output cap). At least
// 1024 thinking tokens must remain; genuinely undersized requests still receive
// a clear client error from this function.
func normalizeClaudeThinkingBudget(req *ClaudeRequest) (changed bool, err error) {
	if req == nil || req.Thinking == nil || req.MaxTokens <= 0 {
		return false, nil
	}
	if !strings.EqualFold(strings.TrimSpace(req.Thinking.Type), "enabled") || req.Thinking.BudgetTokens <= 0 {
		return false, nil
	}
	if req.Thinking.BudgetTokens < req.MaxTokens {
		return false, nil
	}
	adjusted := req.MaxTokens - 1
	if adjusted < 1024 {
		return false, fmt.Errorf("max_tokens must be greater than 1024 when thinking.budget_tokens is enabled")
	}
	original := req.Thinking.BudgetTokens
	req.Thinking.BudgetTokens = adjusted
	return original != adjusted, nil
}

func applyClaudeTokenBudgetDefaults(req *ClaudeRequest) int {
	if req == nil {
		return 0
	}
	clientMaxOutput := req.MaxTokens > 0 || req.MaxOutputTokens > 0
	if req.MaxTokens <= 0 && req.MaxOutputTokens > 0 {
		req.MaxTokens = req.MaxOutputTokens
	}
	if req.MaxTokens <= 0 {
		req.MaxTokens = defaultMaxOutputTokens(req.Model)
	}
	if !clientMaxOutput && req.Thinking != nil && strings.EqualFold(strings.TrimSpace(req.Thinking.Type), "enabled") && req.Thinking.BudgetTokens > 0 && req.MaxTokens <= req.Thinking.BudgetTokens {
		reserve := max(1024, min(4096, req.Thinking.BudgetTokens/4))
		req.MaxTokens = req.Thinking.BudgetTokens + reserve
	}
	return resolveContextWindowTokens(req.Model, req.ContextWindow, req.MaxInputTokens)
}

func applyOpenAITokenBudgetDefaults(req *OpenAIRequest) int {
	if req == nil {
		return 0
	}
	if req.MaxTokens <= 0 && req.MaxCompletionTokens > 0 {
		req.MaxTokens = req.MaxCompletionTokens
	}
	if req.MaxTokens <= 0 && req.MaxOutputTokens > 0 {
		req.MaxTokens = req.MaxOutputTokens
	}
	if req.MaxTokens <= 0 {
		req.MaxTokens = defaultMaxOutputTokens(req.Model)
	}
	return resolveContextWindowTokens(req.Model, req.ContextWindow, req.MaxInputTokens)
}

func defaultMaxOutputTokens(model string) int {
	if entry, ok := resolveConfiguredModelBudget(model); ok && entry.MaxTokens > 0 {
		return entry.MaxTokens
	}
	return config.GetThinkingConfig().DefaultMaxOutputTokens
}

func resolveContextWindowTokens(model string, explicitValues ...int) int {
	for _, value := range explicitValues {
		if value > 0 {
			return value
		}
	}
	if entry, ok := resolveConfiguredModelBudget(model); ok && entry.ContextWindow > 0 {
		return entry.ContextWindow
	}
	return getContextWindowSize(model)
}

func resolveConfiguredModelBudget(model string) (config.ModelEntry, bool) {
	model = strings.TrimSpace(model)
	thinkingSuffix := config.GetThinkingConfig().Suffix
	if thinkingSuffix != "" && strings.HasSuffix(strings.ToLower(model), strings.ToLower(thinkingSuffix)) {
		model = model[:len(model)-len(thinkingSuffix)]
	}
	if entry, ok := config.ResolveConfiguredModel(model); ok {
		return entry, true
	}
	return config.GetConfiguredModelMetadata(model)
}
