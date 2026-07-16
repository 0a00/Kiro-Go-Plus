package proxy

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

// TestClaudeToKiroTruncatesOversizedHistory builds a conversation whose history
// far exceeds the upstream input limit and verifies the converted payload is
// trimmed below maxPayloadBytes, that a truncation placeholder is inserted, and
// that the current message is preserved.
func TestClaudeToKiroTruncatesOversizedHistory(t *testing.T) {
	// ~2KB chunk repeated across many turns to blow past the byte limit.
	big := strings.Repeat("lorem ipsum dolor sit amet ", 80) // ~2.1KB

	msgs := []ClaudeMessage{
		{Role: "user", Content: "start the long task"},
	}
	for i := 0; i < 800; i++ {
		msgs = append(msgs,
			ClaudeMessage{Role: "assistant", Content: "step result: " + big},
			ClaudeMessage{Role: "user", Content: "next: " + big},
		)
	}
	msgs = append(msgs, ClaudeMessage{Role: "user", Content: "FINAL: summarize everything above"})

	req := &ClaudeRequest{
		Model:    "claude-opus-4.8",
		System:   "You are a helpful assistant.",
		Messages: msgs,
	}

	payload := ClaudeToKiro(req, false)

	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	if len(raw) > maxPayloadBytes {
		t.Fatalf("payload size %d exceeds limit %d after truncation", len(raw), maxPayloadBytes)
	}

	// The current message must be preserved.
	cur := payload.ConversationState.CurrentMessage.UserInputMessage
	if !strings.Contains(cur.Content, "FINAL: summarize everything above") {
		t.Fatalf("current message lost after truncation, got %q", cur.Content[:min(80, len(cur.Content))])
	}

	// A truncation placeholder must be present in history.
	foundPlaceholder := false
	for _, h := range payload.ConversationState.History {
		if h.UserInputMessage != nil && strings.Contains(h.UserInputMessage.Content, "truncated to fit") {
			foundPlaceholder = true
			break
		}
	}
	if !foundPlaceholder {
		t.Fatalf("expected a truncation placeholder in history")
	}

	// System priming should still be at the front.
	if len(payload.ConversationState.History) < 2 {
		t.Fatalf("expected priming retained, history too short")
	}
	primingUser := payload.ConversationState.History[0].UserInputMessage
	if primingUser == nil || !strings.Contains(primingUser.Content, "helpful assistant") {
		t.Fatalf("expected system priming retained at front")
	}
}

// TestClaudeToKiroSmallPayloadNotTruncated ensures normal-sized conversations
// are left untouched (no placeholder inserted).
func TestClaudeToKiroSmallPayloadNotTruncated(t *testing.T) {
	req := &ClaudeRequest{
		Model:  "claude-opus-4.8",
		System: "You are helpful.",
		Messages: []ClaudeMessage{
			{Role: "user", Content: "hello"},
			{Role: "assistant", Content: "hi"},
			{Role: "user", Content: "how are you?"},
		},
	}
	payload := ClaudeToKiro(req, false)
	for _, h := range payload.ConversationState.History {
		if h.UserInputMessage != nil && strings.Contains(h.UserInputMessage.Content, "truncated to fit") {
			t.Fatalf("small payload should not be truncated")
		}
	}
}

func TestPayloadTruncationEnforcesTokenWindowBelowByteLimit(t *testing.T) {
	chunk := strings.Repeat("这是用于验证输入窗口的中文上下文。", 350)
	messages := []ClaudeMessage{{Role: "user", Content: "start"}}
	for i := 0; i < 18; i++ {
		messages = append(messages,
			ClaudeMessage{Role: "assistant", Content: chunk},
			ClaudeMessage{Role: "user", Content: chunk},
		)
	}
	messages = append(messages, ClaudeMessage{Role: "user", Content: "FINAL: summarize"})
	payload := ClaudeToKiro(&ClaudeRequest{Model: "claude-sonnet-4.6", Messages: messages}, false)
	if payloadByteSize(payload) >= maxPayloadBytes {
		t.Fatalf("test payload must exercise token-only truncation, bytes=%d", payloadByteSize(payload))
	}
	payload.contextWindowTokens = 50_000
	truncatePayloadToLimit(payload, payload.hasSystemPriming)

	limit := payloadInputTokenLimit(payload)
	if got := estimateKiroPayloadTokens(payload); got > limit {
		t.Fatalf("payload tokens=%d exceed limit=%d", got, limit)
	}
	if !strings.Contains(payload.ConversationState.CurrentMessage.UserInputMessage.Content, "FINAL") {
		t.Fatalf("current instruction was lost: %q", payload.ConversationState.CurrentMessage.UserInputMessage.Content)
	}
}

func TestPayloadTruncationShrinksActiveToolResultWithoutOrphaning(t *testing.T) {
	huge := strings.Repeat("中", 50_000)
	payload := ClaudeToKiro(&ClaudeRequest{
		Model: "claude-sonnet-4.6",
		Messages: []ClaudeMessage{
			{Role: "user", Content: "read it"},
			{Role: "assistant", Content: []interface{}{map[string]interface{}{
				"type": "tool_use", "id": "tool_1", "name": "Read", "input": map[string]interface{}{"path": "large.txt"},
			}}},
			{Role: "user", Content: []interface{}{map[string]interface{}{
				"type": "tool_result", "tool_use_id": "tool_1", "content": huge,
			}}},
		},
	}, false)
	payload.contextWindowTokens = 20_000
	truncatePayloadToLimit(payload, payload.hasSystemPriming)

	limit := payloadInputTokenLimit(payload)
	if got := estimateKiroPayloadTokens(payload); got > limit {
		t.Fatalf("tool-result payload tokens=%d exceed limit=%d", got, limit)
	}
	context := payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext
	if context == nil || len(context.ToolResults) != 1 {
		t.Fatalf("active tool result was discarded instead of shrunk: %+v", context)
	}
	if !currentToolResultsMatchLastAssistant(payload.ConversationState.History, collectToolResultIDs(context.ToolResults)) {
		t.Fatal("truncation orphaned the active tool result")
	}
	if got := len([]rune(context.ToolResults[0].Content[0].Text)); got <= 0 || got >= len([]rune(huge)) {
		t.Fatalf("tool result was not meaningfully shrunk: runes=%d", got)
	}
}

func TestPayloadTruncationDropsWholeImagesOnByteOverflow(t *testing.T) {
	payload := &KiroPayload{contextWindowTokens: 200_000}
	payload.ConversationState.CurrentMessage.UserInputMessage = KiroUserInputMessage{
		Content: "describe the images",
		ModelID: "claude-sonnet-4.6",
		Origin:  "AI_EDITOR",
		Images:  make([]KiroImage, 3),
	}
	for i := range payload.ConversationState.CurrentMessage.UserInputMessage.Images {
		payload.ConversationState.CurrentMessage.UserInputMessage.Images[i].Format = "png"
		payload.ConversationState.CurrentMessage.UserInputMessage.Images[i].Source.Bytes = strings.Repeat("A", 400_000)
	}
	truncatePayloadToLimit(payload, false)

	if payloadByteSize(payload) > maxPayloadBytes {
		t.Fatalf("image payload remains oversized: %d", payloadByteSize(payload))
	}
	if len(payload.ConversationState.CurrentMessage.UserInputMessage.Images) != 0 {
		t.Fatal("oversized base64 images must be removed whole")
	}
}

func TestPayloadTruncationRefitsDetachedToolResult(t *testing.T) {
	payload := &KiroPayload{contextWindowTokens: 4_000}
	payload.ConversationState.CurrentMessage.UserInputMessage = KiroUserInputMessage{
		Content: strings.Repeat("current context ", 500),
		ModelID: "claude-sonnet-4.6",
		Origin:  "AI_EDITOR",
		UserInputMessageContext: &UserInputMessageContext{
			ToolResults: []KiroToolResult{{
				ToolUseID: "orphaned-tool",
				Status:    "success",
				Content:   []KiroResultContent{{Text: strings.Repeat("result data ", 800)}},
			}},
		},
	}

	truncatePayloadToLimit(payload, false)

	if context := payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext; context != nil && len(context.ToolResults) > 0 {
		t.Fatal("orphaned tool result remained structured")
	}
	if got, limit := estimateKiroPayloadTokens(payload), payloadInputTokenLimit(payload); got > limit {
		t.Fatalf("detached payload tokens=%d exceed limit=%d", got, limit)
	}
	if got := payloadByteSize(payload); got > maxPayloadBytes {
		t.Fatalf("detached payload bytes=%d exceed limit=%d", got, maxPayloadBytes)
	}
}

func TestTruncateStringToBytesPreservesUTF8(t *testing.T) {
	value := "héllo世界"
	for limit := 0; limit <= len(value); limit++ {
		got := truncateStringToBytes(value, limit)
		if len(got) > limit || !utf8.ValidString(got) {
			t.Fatalf("limit=%d produced invalid value %q", limit, got)
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
