package proxy

import (
	"net/http"
	"testing"
)

func TestValidateKiroPayloadToolHistoryRejectsOrphanAndDuplicateResults(t *testing.T) {
	cases := []struct {
		name    string
		results []KiroToolResult
	}{
		{
			name:    "orphan",
			results: []KiroToolResult{pairingResult("missing", "result")},
		},
		{
			name: "duplicate",
			results: []KiroToolResult{
				pairingResult("tool_a", "first"),
				pairingResult("tool_a", "second"),
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload := &KiroPayload{}
			payload.ConversationState.History = pairingHistory("tool_a")
			payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext = &UserInputMessageContext{
				ToolResults: tc.results,
			}
			err := validateKiroPayloadToolHistory(payload)
			if err == nil || err.Kind != UpstreamErrorClientRequest || err.StatusCode != http.StatusBadRequest {
				t.Fatalf("validation error = %#v, want local 400 client request", err)
			}
			if err.Reason != "TOOL_HISTORY_INVALID" || err.RetryAcrossAccounts || err.RetryAcrossEndpoints {
				t.Fatalf("unexpected validation classification: %#v", err)
			}
		})
	}
}

func TestValidateKiroPayloadToolHistoryAcceptsReorderedCompleteResults(t *testing.T) {
	payload := &KiroPayload{}
	payload.ConversationState.History = pairingHistory("tool_a", "tool_b")
	payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext = &UserInputMessageContext{
		ToolResults: []KiroToolResult{
			pairingResult("tool_b", "b"),
			pairingResult("tool_a", "a"),
		},
	}

	if err := validateKiroPayloadToolHistory(payload); err != nil {
		t.Fatalf("reordered complete results rejected: %v", err)
	}
}

func TestShouldFallbackTransparentHistoryForGenericRejectedRequest(t *testing.T) {
	payload := &KiroPayload{transparentClaudeCode: true}
	payload.ConversationState.History = pairingHistory("tool_a")

	err := &UpstreamError{
		Kind:       UpstreamErrorClientRequest,
		StatusCode: http.StatusBadRequest,
		Message:    "The request was rejected by the upstream service. Check the input and tool history.",
	}
	if !shouldFallbackTransparentHistory(payload, err) {
		t.Fatal("generic tool-history rejection did not trigger one compatibility fallback")
	}
}

func TestShouldNotFallbackTransparentHistoryForGenericModel400(t *testing.T) {
	payload := &KiroPayload{transparentClaudeCode: true}
	payload.ConversationState.History = pairingHistory("tool_a")

	err := &UpstreamError{
		Kind:       UpstreamErrorClientRequest,
		StatusCode: http.StatusBadRequest,
		Message:    "INVALID_MODEL_ID: Invalid model",
	}
	if shouldFallbackTransparentHistory(payload, err) {
		t.Fatal("model rejection incorrectly triggered history fallback")
	}
}

func TestShouldFallbackTransparentHistoryRunsOnlyOnce(t *testing.T) {
	payload := &KiroPayload{
		transparentClaudeCode:          true,
		transparentHistoryFallbackUsed: true,
	}
	payload.ConversationState.History = pairingHistory("tool_a")
	err := &UpstreamError{
		Kind:       UpstreamErrorClientRequest,
		StatusCode: http.StatusBadRequest,
		Message:    "The request was rejected by the upstream service. Check the input and tool history.",
	}
	if shouldFallbackTransparentHistory(payload, err) {
		t.Fatal("transparent history fallback repeated after it was consumed")
	}
}
