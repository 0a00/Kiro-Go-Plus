package proxy

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestPrepareClaudeToolPolicyRequiresToolForWorkspaceTask(t *testing.T) {
	req := &ClaudeRequest{
		Messages: []ClaudeMessage{{
			Role:    "user",
			Content: "任务目标：请创建一个单个 HTML 文件并直接写入工作区。",
		}},
		Tools: []ClaudeTool{{Name: "Write", Description: "Write a file"}},
	}
	if err := prepareClaudeToolPolicy(req, true); err != nil {
		t.Fatalf("prepare policy: %v", err)
	}
	if !req.RequireToolUse {
		t.Fatal("expected workspace task to require a tool")
	}
	if req.ToolUsePolicy != toolUsePolicyInferred {
		t.Fatalf("expected inferred tool policy, got %q", req.ToolUsePolicy)
	}
	if requiresStrictClaudeToolUse(req) {
		t.Fatal("inferred workspace policy must not reject a valid text response")
	}
	if !strings.Contains(extractSystemPrompt(req.System), agentToolPolicyMarker) {
		t.Fatal("expected agent tool policy in system prompt")
	}
	payload := ClaudeToKiro(req, true)
	if !strings.Contains(payload.ConversationState.CurrentMessage.UserInputMessage.Content, agentRequiredToolActionMarker) {
		t.Fatal("expected required tool reminder in current message")
	}
}

func TestPrepareClaudeToolPolicyDoesNotForceExplanatoryQuestion(t *testing.T) {
	req := &ClaudeRequest{
		Messages: []ClaudeMessage{{Role: "user", Content: "如何创建一个 HTML 文件？只解释原理。"}},
		Tools:    []ClaudeTool{{Name: "Write", Description: "Write a file"}},
	}
	if err := prepareClaudeToolPolicy(req, true); err != nil {
		t.Fatalf("prepare policy: %v", err)
	}
	if req.RequireToolUse {
		t.Fatal("explanatory question should not force tool use")
	}
}

func TestPrepareClaudeToolPolicyHonorsRequiredChoice(t *testing.T) {
	req := &ClaudeRequest{
		Messages:   []ClaudeMessage{{Role: "user", Content: "Check the workspace"}},
		Tools:      []ClaudeTool{{Name: "Bash", Description: "Run a command"}},
		ToolChoice: map[string]interface{}{"type": "tool", "name": "Bash"},
	}
	if err := prepareClaudeToolPolicy(req, false); err != nil {
		t.Fatalf("prepare policy: %v", err)
	}
	if !req.RequireToolUse || req.RequiredToolName != "Bash" {
		t.Fatalf("unexpected required tool state: %+v", req)
	}
	if req.ToolUsePolicy != toolUsePolicyExplicit {
		t.Fatalf("expected explicit tool policy, got %q", req.ToolUsePolicy)
	}
	if !requiresStrictClaudeToolUse(req) {
		t.Fatal("explicit tool choice must keep strict tool enforcement")
	}
}

func TestPrepareClaudeToolPolicyRejectsUnknownChoice(t *testing.T) {
	req := &ClaudeRequest{
		Tools:      []ClaudeTool{{Name: "Bash", Description: "Run a command"}},
		ToolChoice: map[string]interface{}{"type": "tool", "name": "Write"},
	}
	if err := prepareClaudeToolPolicy(req, false); err == nil {
		t.Fatal("expected unknown tool choice to fail")
	}
}

func TestEnhanceClaudeToolDescription(t *testing.T) {
	description := enhanceClaudeToolDescription("Write", "Write a file")
	if !strings.Contains(description, "Do not print replacement file contents") {
		t.Fatalf("missing write-tool guidance: %q", description)
	}
}

func TestTruncateToolDescriptionUsesUTF8SafeByteLimit(t *testing.T) {
	description := strings.Repeat("中", maxToolDescLen)
	got := truncateToolDescription(description, maxToolDescLen)
	if !utf8.ValidString(got) {
		t.Fatal("truncated description is not valid UTF-8")
	}
	if len(got) > maxToolDescLen+len("...") {
		t.Fatalf("description exceeded byte limit: %d", len(got))
	}
}
