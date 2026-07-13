package proxy

import (
	"fmt"
	"regexp"
	"strings"
)

const (
	agentToolPolicyMarker         = "<agent_tool_policy>"
	agentRequiredToolActionMarker = "<required_tool_action>"
	toolUsePolicyExplicit         = "explicit"
	toolUsePolicyInferred         = "inferred"
)

var (
	englishWorkspaceAction = regexp.MustCompile(`(?i)\b(create|write|modify|edit|delete|implement|fix|run|execute|install|build|test|inspect|read|update|generate)\b`)
	englishWorkspaceObject = regexp.MustCompile(`(?i)\b(file|code|project|page|website|command|script|directory|folder|html|css|javascript|workspace|repository|repo)\b`)
	englishImperative      = regexp.MustCompile(`(?i)^\s*(please\s+)?(create|write|modify|edit|delete|implement|fix|run|execute|install|build|test|inspect|read|update|generate)\b`)
)

func prepareClaudeToolPolicy(req *ClaudeRequest, enforceWorkspaceActions bool) error {
	if req == nil {
		return nil
	}
	req.RequireToolUse = false
	req.RequiredToolName = ""
	req.ToolUsePolicy = ""

	mode, name, err := parseClaudeToolChoice(req.ToolChoice)
	if err != nil {
		return err
	}
	switch mode {
	case "none":
		req.Tools = nil
		return nil
	case "any":
		if len(req.Tools) == 0 {
			return fmt.Errorf("tool_choice=any requires at least one tool")
		}
		req.RequireToolUse = true
		req.ToolUsePolicy = toolUsePolicyExplicit
	case "tool":
		if !claudeToolExists(req.Tools, name) {
			return fmt.Errorf("tool_choice references unknown tool %q", name)
		}
		req.RequireToolUse = true
		req.RequiredToolName = name
		req.ToolUsePolicy = toolUsePolicyExplicit
	}

	if enforceWorkspaceActions && !req.RequireToolUse && shouldRequireWorkspaceTool(req) {
		req.RequireToolUse = true
		req.ToolUsePolicy = toolUsePolicyInferred
	}
	if len(req.Tools) == 0 {
		return nil
	}
	req.System = appendClaudeSystemText(req.System, buildClaudeAgentToolPolicy(req))
	return nil
}

func requiresStrictClaudeToolUse(req *ClaudeRequest) bool {
	return req != nil && req.ToolUsePolicy == toolUsePolicyExplicit
}

func parseClaudeToolChoice(raw interface{}) (mode, name string, err error) {
	if raw == nil {
		return "auto", "", nil
	}
	if value, ok := raw.(string); ok {
		mode = strings.ToLower(strings.TrimSpace(value))
	} else if value, ok := raw.(map[string]interface{}); ok {
		mode, _ = value["type"].(string)
		name, _ = value["name"].(string)
		mode = strings.ToLower(strings.TrimSpace(mode))
		name = strings.TrimSpace(name)
	} else {
		return "", "", fmt.Errorf("invalid tool_choice")
	}
	if mode == "" {
		mode = "auto"
	}
	switch mode {
	case "auto", "any", "none":
		return mode, "", nil
	case "tool":
		if name == "" {
			return "", "", fmt.Errorf("tool_choice tool name is required")
		}
		return mode, name, nil
	default:
		return "", "", fmt.Errorf("unsupported tool_choice %q", mode)
	}
}

func claudeToolExists(tools []ClaudeTool, name string) bool {
	for _, tool := range tools {
		if tool.Name == name {
			return true
		}
	}
	return false
}

func shouldRequireWorkspaceTool(req *ClaudeRequest) bool {
	if req == nil || !hasWorkspaceMutationTool(req.Tools) {
		return false
	}
	text := strings.TrimSpace(lastClaudeUserText(req.Messages))
	if text == "" {
		return false
	}
	lower := strings.ToLower(text)
	if containsAny(text, "创建", "新建", "写入", "修改", "编辑", "删除", "实现", "修复", "运行", "执行", "安装", "构建", "测试", "读取", "检查") &&
		containsAny(lower, "文件", "代码", "项目", "页面", "网站", "命令", "脚本", "目录", "html", "css", "javascript", "仓库") &&
		containsAny(text, "请", "任务目标", "帮我", "直接", "需要", "要求", "完成") {
		return true
	}
	return englishWorkspaceAction.MatchString(text) && englishWorkspaceObject.MatchString(text) &&
		(englishImperative.MatchString(text) || strings.Contains(lower, "please ") || strings.Contains(lower, "task:"))
}

func hasWorkspaceMutationTool(tools []ClaudeTool) bool {
	for _, tool := range tools {
		name := strings.ToLower(strings.TrimSpace(tool.Name))
		compact := strings.NewReplacer("_", "", "-", "", ".", "").Replace(name)
		for _, marker := range []string{"write", "edit", "patch", "bash", "shell", "exec", "command", "notebookedit"} {
			if strings.Contains(compact, marker) {
				return true
			}
		}
	}
	return false
}

func lastClaudeUserText(messages []ClaudeMessage) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role != "user" {
			continue
		}
		text, _, _ := extractClaudeUserContent(messages[i].Content)
		return text
	}
	return ""
}

func containsAny(text string, values ...string) bool {
	for _, value := range values {
		if strings.Contains(text, value) {
			return true
		}
	}
	return false
}

func buildClaudeAgentToolPolicy(req *ClaudeRequest) string {
	policy := agentToolPolicyMarker + `
The tools supplied with this request are real callable actions. When the user asks you to create or modify files, inspect the workspace, run commands, or otherwise perform an action, call the appropriate tool instead of printing the intended file or command as ordinary text. Do not stop after planning, do not describe an action as a future "next" step, and do not imitate tool-call JSON in text.`
	if req != nil && req.RequireToolUse {
		if req.RequiredToolName != "" {
			policy += fmt.Sprintf("\nFor this request, you must call the %q tool before ending your turn.", req.RequiredToolName)
		} else {
			policy += "\nFor this request, you must call at least one appropriate tool before ending your turn."
		}
	}
	return policy + "\n</agent_tool_policy>"
}

func appendClaudeRequiredToolAction(content string, req *ClaudeRequest) string {
	if req == nil || !req.RequireToolUse || strings.Contains(content, agentRequiredToolActionMarker) {
		return content
	}
	reminder := agentRequiredToolActionMarker + "\nThis request is not complete until you call at least one appropriate provided tool. Do not output the requested file contents or commands as a substitute for the tool call."
	if req.RequiredToolName != "" {
		reminder = fmt.Sprintf("%s\nCall the %q tool before ending this turn.", agentRequiredToolActionMarker, req.RequiredToolName)
	}
	reminder += "\n</required_tool_action>"
	if strings.TrimSpace(content) == "" {
		return reminder
	}
	return content + "\n\n" + reminder
}

func appendClaudeSystemText(system interface{}, text string) interface{} {
	if strings.TrimSpace(text) == "" || strings.Contains(extractSystemPrompt(system), agentToolPolicyMarker) {
		return system
	}
	block := map[string]interface{}{"type": "text", "text": text}
	switch value := system.(type) {
	case nil:
		return []interface{}{block}
	case string:
		if strings.TrimSpace(value) == "" {
			return []interface{}{block}
		}
		return []interface{}{map[string]interface{}{"type": "text", "text": value}, block}
	case []interface{}:
		blocks := append([]interface{}(nil), value...)
		return append(blocks, block)
	default:
		return []interface{}{map[string]interface{}{"type": "text", "text": extractSystemPrompt(system)}, block}
	}
}

func enhanceClaudeToolDescription(name, description string) string {
	description = normalizeToolDesc(description, name)
	compact := strings.NewReplacer("_", "", "-", "", ".", "").Replace(strings.ToLower(name))
	switch {
	case compact == "write" || strings.HasSuffix(compact, "writefile"):
		return "IMPORTANT: Use this tool to perform requested workspace changes. Do not print replacement file contents as the final answer instead of calling the tool. If the content exceeds 150 lines, write only the first 50 lines with a unique placeholder, then use Edit calls to append the remainder in chunks of at most 50 lines.\n" + description
	case compact == "edit" || strings.Contains(compact, "applypatch") || strings.Contains(compact, "notebookedit"):
		return "IMPORTANT: Use this tool to perform requested workspace changes. Do not print replacement file contents as the final answer instead of calling the tool. Keep each inserted or replacement chunk to at most 50 lines and continue with additional calls when needed.\n" + description
	case strings.Contains(compact, "write") || strings.Contains(compact, "edit") || strings.Contains(compact, "patch"):
		return "IMPORTANT: Use this tool to perform requested workspace changes. Do not print replacement file contents as the final answer instead of calling the tool.\n" + description
	case strings.Contains(compact, "bash") || strings.Contains(compact, "shell") || strings.Contains(compact, "exec"):
		return "IMPORTANT: When command execution is requested, call this tool instead of only describing the command.\n" + description
	default:
		return description
	}
}
