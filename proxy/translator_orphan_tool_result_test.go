package proxy

import "testing"

// TestClaudeToKiroNeverLeavesOrphanedHistoryToolResults reproduces the upstream
// rejection
//
//	HTTP 400 TOOL_USE_RESULT_MISMATCH: messages.N.content.M: unexpected
//	`tool_use_id` found in `tool_result` blocks
//
// Kiro requires every history tool_result to be answered by a tool_use in the
// immediately preceding assistant turn. History flattening keeps at most one
// active structured tool turn, so an image-bearing tool_result turn that kept
// its structured results while its assistant turn was flattened becomes an
// orphan and fails the whole request.
func TestClaudeToKiroNeverLeavesOrphanedHistoryToolResults(t *testing.T) {
	req := &ClaudeRequest{
		Model: "claude-sonnet-4.6",
		Messages: []ClaudeMessage{
			{Role: "user", Content: "take a screenshot"},
			{Role: "assistant", Content: []interface{}{
				map[string]interface{}{"type": "tool_use", "id": "toolu_shot", "name": "screenshot", "input": map[string]interface{}{}},
			}},
			{Role: "user", Content: []interface{}{
				map[string]interface{}{"type": "tool_result", "tool_use_id": "toolu_shot", "content": []interface{}{
					map[string]interface{}{"type": "image", "source": map[string]interface{}{
						"type": "base64", "media_type": "image/png", "data": "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8DwHwAFAAH/q842iQAAAABJRU5ErkJggg==",
					}},
				}},
			}},
			{Role: "user", Content: "now describe what you saw"},
		},
	}

	payload := ClaudeToKiro(req, false)

	history := payload.ConversationState.History
	for i, entry := range history {
		user := entry.UserInputMessage
		if user == nil || user.UserInputMessageContext == nil {
			continue
		}
		results := user.UserInputMessageContext.ToolResults
		if len(results) == 0 {
			continue
		}
		available := map[string]bool{}
		if i > 0 {
			if prev := history[i-1].AssistantResponseMessage; prev != nil {
				for _, tu := range prev.ToolUses {
					available[tu.ToolUseID] = true
				}
			}
		}
		for _, tr := range results {
			if !available[tr.ToolUseID] {
				t.Fatalf("history[%d] carries orphaned tool_result %q; upstream rejects it with TOOL_USE_RESULT_MISMATCH", i, tr.ToolUseID)
			}
		}
	}

	// The tool image must survive the flattening rather than be dropped.
	images := 0
	for _, entry := range history {
		if entry.UserInputMessage != nil {
			images += len(entry.UserInputMessage.Images)
		}
	}
	if images != 1 {
		t.Fatalf("expected the tool image to survive in history, got %d", images)
	}
}
