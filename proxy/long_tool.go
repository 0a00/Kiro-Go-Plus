package proxy

import (
	"fmt"
	"kiro-go/config"
	"strings"
	"unicode"
)

const (
	longToolPolicyMarker   = "<long_tool_policy>"
	toolRecoveryHintMarker = "<tool_truncation_recovery>"
)

func isHighRiskToolName(name string) bool {
	var normalized strings.Builder
	var previous rune
	for index, current := range strings.TrimSpace(name) {
		if unicode.IsUpper(current) && index > 0 && (unicode.IsLower(previous) || unicode.IsDigit(previous)) {
			normalized.WriteByte(' ')
		}
		if unicode.IsLetter(current) || unicode.IsDigit(current) {
			normalized.WriteRune(unicode.ToLower(current))
		} else {
			normalized.WriteByte(' ')
		}
		previous = current
	}
	tokens := strings.Fields(normalized.String())
	for _, token := range tokens {
		switch token {
		case "write", "edit", "editor", "patch", "bash", "shell", "exec", "execute", "command":
			return true
		}
	}
	compact := strings.Join(tokens, "")
	for _, suffix := range []string{"writefile", "filewrite", "multiedit", "notebookedit", "applypatch", "strreplaceeditor", "executecommand", "runcommand", "shellcommand"} {
		if strings.HasSuffix(compact, suffix) {
			return true
		}
	}
	return false
}

func claudeToolNames(tools []ClaudeTool) []string {
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.Name)
	}
	return names
}

func openAIToolNames(tools []OpenAITool) []string {
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		if tool.Type == "function" {
			names = append(names, tool.Function.Name)
		}
	}
	return names
}

func hasHighRiskToolNames(names []string) bool {
	for _, name := range names {
		if isHighRiskToolName(name) {
			return true
		}
	}
	return false
}

func resolveModelMaxToolTokens(model string) int {
	if entry, ok := config.ResolveConfiguredModel(model); ok && entry.MaxToolTokens > 0 {
		return entry.MaxToolTokens
	}
	mapped := MapModel(model)
	if entry, ok := config.GetConfiguredModelMetadata(mapped); ok && entry.MaxToolTokens > 0 {
		return entry.MaxToolTokens
	}

	lower := strings.ToLower(mapped)
	for _, capable := range []string{"claude-opus-4.7", "claude-opus-4.8", "claude-sonnet-5"} {
		if strings.Contains(lower, capable) {
			return 32768
		}
	}
	return config.GetLongToolConfig().DefaultMaxToolTokens
}

func appendLongToolPolicy(base, model string, toolNames []string) string {
	settings := config.GetLongToolConfig()
	if !settings.Enabled || !hasHighRiskToolNames(toolNames) || strings.Contains(base, longToolPolicyMarker) {
		return base
	}
	limit := resolveModelMaxToolTokens(model)
	policy := fmt.Sprintf(`%s
Kiro can truncate oversized tool-call arguments even when the advertised model output limit is higher. Keep every individual Write, Edit, patch, Bash, shell, exec, or command tool call below approximately %d output tokens. For large files, write a small valid skeleton with a unique placeholder, then append or replace content through multiple Edit/patch calls. Keep each file-changing chunk at no more than 50 lines and avoid putting a complete large file, base64 payload, or large heredoc into one tool call. Complete chunked work without asking the user to change approaches.
</long_tool_policy>`, longToolPolicyMarker, limit)
	if strings.TrimSpace(base) == "" {
		return policy
	}
	return base + "\n\n" + policy
}

func maybeLongToolFallback(model string, maxTokens int, toolNames []string) (string, bool) {
	settings := config.GetLongToolConfig()
	if !settings.Enabled || !settings.FallbackEnabled || !hasHighRiskToolNames(toolNames) || maxTokens <= 0 {
		return model, false
	}
	currentLimit := resolveModelMaxToolTokens(model)
	if maxTokens <= currentLimit {
		return model, false
	}
	fallback, _ := ParseModelAndThinking(settings.FallbackModel, config.GetThinkingConfig().Suffix)
	fallback = strings.TrimSpace(fallback)
	if fallback == "" || strings.EqualFold(fallback, model) {
		return model, false
	}
	if resolveModelMaxToolTokens(fallback) <= currentLimit {
		return model, false
	}
	return fallback, true
}

func (p *KiroPayload) recordToolStreamMetrics(argumentBytes, fragmentCount int) {
	if p == nil {
		return
	}
	p.streamMetricsMu.Lock()
	if argumentBytes > p.maxToolArgumentBytes {
		p.maxToolArgumentBytes = argumentBytes
	}
	if fragmentCount > p.maxToolFragmentCount {
		p.maxToolFragmentCount = fragmentCount
	}
	p.streamMetricsMu.Unlock()
}

func (p *KiroPayload) recordToolTruncation(argumentBytes, fragmentCount int, retryable bool) bool {
	if p == nil {
		return false
	}
	settings := config.GetLongToolConfig()
	p.streamMetricsMu.Lock()
	p.toolTruncationCount++
	if argumentBytes > p.maxToolArgumentBytes {
		p.maxToolArgumentBytes = argumentBytes
	}
	if fragmentCount > p.maxToolFragmentCount {
		p.maxToolFragmentCount = fragmentCount
	}
	allowRetry := retryable && settings.Enabled && p.toolTruncationCount <= settings.TruncationRetries
	if allowRetry {
		p.toolRecoveryAttempts++
		if !p.toolRecoveryHintApplied {
			p.applyToolRecoveryHintLocked()
			p.toolRecoveryHintApplied = true
		}
	}
	p.streamMetricsMu.Unlock()
	return allowRetry
}

func (p *KiroPayload) applyToolRecoveryHintLocked() {
	message := &p.ConversationState.CurrentMessage.UserInputMessage
	if strings.Contains(message.Content, toolRecoveryHintMarker) {
		return
	}
	limit := resolveModelMaxToolTokens(message.ModelID)
	hint := fmt.Sprintf(`%s
The previous upstream attempt was truncated while constructing a tool call. Retry with smaller, complete, valid JSON tool calls. Keep each tool call below approximately %d output tokens. For file creation, write a short skeleton first and continue with Edit/patch chunks of at most 50 lines. Never repeat the same oversized single Write or heredoc.
</tool_truncation_recovery>`, toolRecoveryHintMarker, limit)
	if content := strings.TrimSpace(message.Content); content != "" {
		message.Content = content + "\n\n" + hint
	} else {
		message.Content = hint
	}
	if message.UserInputMessageContext == nil {
		return
	}
	for i := range message.UserInputMessageContext.Tools {
		tool := &message.UserInputMessageContext.Tools[i].ToolSpecification
		name := tool.Name
		if original, ok := p.ToolNameMap[name]; ok {
			name = original
		}
		if !isHighRiskToolName(name) || strings.Contains(tool.Description, toolRecoveryHintMarker) {
			continue
		}
		tool.Description = truncateToolDescription(tool.Description+"\n"+hint, maxToolDescLen)
	}
}
