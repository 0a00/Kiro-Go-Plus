package proxy

import (
	"kiro-go/config"
	"path/filepath"
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
	if strings.Contains(extractSystemPrompt(req.System), agentToolPolicyMarker) {
		t.Fatal("default tool enforcement must not inject hidden steering")
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

func TestPrepareClaudeToolPolicyRequiresToolForWorkspaceContinuation(t *testing.T) {
	req := &ClaudeRequest{
		Messages: []ClaudeMessage{
			{Role: "user", Content: "Create and edit the HTML file in the workspace."},
			{Role: "assistant", Content: []interface{}{
				map[string]interface{}{"type": "tool_use", "id": "tool-1", "name": "Edit", "input": map[string]interface{}{"file_path": "index.html"}},
			}},
			{Role: "user", Content: []interface{}{
				map[string]interface{}{"type": "tool_result", "tool_use_id": "tool-1", "content": "edited"},
			}},
			{Role: "user", Content: "继续"},
		},
		Tools: []ClaudeTool{{Name: "Edit", Description: "Edit a file"}},
	}
	if err := prepareClaudeToolPolicy(req, true); err != nil {
		t.Fatalf("prepare continuation policy: %v", err)
	}
	if !req.RequireToolUse || req.ToolUsePolicy != toolUsePolicyInferred {
		t.Fatalf("workspace continuation was not inferred: require=%v policy=%q", req.RequireToolUse, req.ToolUsePolicy)
	}
}

func TestPrepareClaudeToolPolicyDoesNotForcePlainContinuation(t *testing.T) {
	req := &ClaudeRequest{
		Messages: []ClaudeMessage{
			{Role: "user", Content: "Explain the design."},
			{Role: "assistant", Content: "The design is complete."},
			{Role: "user", Content: "继续解释"},
		},
		Tools: []ClaudeTool{{Name: "Edit", Description: "Edit a file"}},
	}
	if err := prepareClaudeToolPolicy(req, true); err != nil {
		t.Fatalf("prepare explanatory continuation policy: %v", err)
	}
	if req.RequireToolUse {
		t.Fatal("plain explanatory continuation unexpectedly required a workspace tool")
	}
}

func TestClaudeCodeTransparentModeRecognizesGatewayForwardedClient(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	req := &ClaudeRequest{
		ClientUserAgent: "Go-http-client/1.1",
		System: []interface{}{map[string]interface{}{
			"type": "text", "text": "Claude Code interactive agent using tools",
		}},
		Tools: []ClaudeTool{
			{Name: "ToolSearch"}, {Name: "AskUserQuestion"}, {Name: "Write"},
		},
	}
	if !isClaudeCodeTransparentRequest(req) {
		t.Fatal("gateway-forwarded Claude Code request was not recognized")
	}
}

func TestClaudeCodeBetaHeaderRecognizesGenericForwardedClient(t *testing.T) {
	req := &ClaudeRequest{
		ClientUserAgent:      "Go-http-client/1.1",
		ClientClaudeCodeBeta: true,
	}
	if !looksLikeClaudeCodeRequest(req) {
		t.Fatal("Claude Code beta header was not recognized for a generic forwarded client")
	}
}

func TestClaudeCodeBetaHeaderMatcher(t *testing.T) {
	for _, tc := range []struct {
		name   string
		header string
		want   bool
	}{
		{name: "current beta", header: "claude-code-20250219,interleaved-thinking-2025-05-14", want: true},
		{name: "mixed case", header: "Prompt-Caching,CLAUDE-CODE-20250219", want: true},
		{name: "unrelated beta", header: "prompt-caching-2024-07-31", want: false},
		{name: "empty", header: "", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasClaudeCodeBetaHeader(tc.header); got != tc.want {
				t.Fatalf("hasClaudeCodeBetaHeader(%q) = %v, want %v", tc.header, got, tc.want)
			}
		})
	}
}

func TestPrepareClaudeToolPolicyDisabledLeavesAutoRequestUnmodified(t *testing.T) {
	req := &ClaudeRequest{
		Messages: []ClaudeMessage{{Role: "user", Content: "Please read the file."}},
		System:   "Client-owned system prompt.",
		Tools:    []ClaudeTool{{Name: "Write", Description: "Original description"}},
	}
	if err := prepareClaudeToolPolicy(req, false); err != nil {
		t.Fatalf("prepare policy: %v", err)
	}
	if req.RequireToolUse || req.AgentToolSteering {
		t.Fatalf("disabled steering changed request policy: %+v", req)
	}
	if strings.Contains(extractSystemPrompt(req.System), agentToolPolicyMarker) {
		t.Fatal("disabled steering injected a system policy")
	}
	payload := ClaudeToKiro(req, false)
	ctx := payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext
	if ctx == nil || len(ctx.Tools) != 1 || ctx.Tools[0].ToolSpecification.Description != "Original description" {
		t.Fatalf("disabled steering changed tool descriptions: %+v", ctx)
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
	if req.AgentToolSteering || strings.Contains(extractSystemPrompt(req.System), agentToolPolicyMarker) {
		t.Fatal("explicit tool choice must not enable hidden steering")
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
