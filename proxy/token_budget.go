package proxy

import (
	"kiro-go/config"
	"strings"
)

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
