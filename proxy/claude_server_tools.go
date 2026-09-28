package proxy

import (
	"fmt"
	"kiro-go/config"
	"strings"
)

// These tools require a provider-side execution environment. Client tools such
// as bash_20250124, text_editor_20250124 and computer_20250124 remain callable.
func validateClaudeServerTools(tools []ClaudeTool) error {
	for _, tool := range tools {
		kind := strings.ToLower(strings.TrimSpace(tool.Type))
		if isNativeWebSearchTool(tool) || strings.HasPrefix(kind, "web_search_") {
			if !isNativeWebSearchTool(tool) {
				return fmt.Errorf("native web search tool must be named web_search or WebSearch")
			}
			if !config.GetWebSearchConfig().Enabled {
				return fmt.Errorf("native web search is disabled on this gateway")
			}
			continue
		}
		for _, prefix := range []string{"web_fetch_", "code_execution_", "bash_code_execution_", "text_editor_code_execution_"} {
			if strings.HasPrefix(kind, prefix) {
				return fmt.Errorf("server tool type %q is not supported on this gateway; use a client-executed tool instead", tool.Type)
			}
		}
	}
	return nil
}
