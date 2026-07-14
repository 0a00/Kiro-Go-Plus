package proxy

import (
	"errors"
	"kiro-go/config"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAppendLongToolPolicyOnlyForHighRiskTools(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}

	base := "Keep the answer concise."
	got := appendLongToolPolicy(base, "claude-sonnet-4.6", []string{"Read", "Write"})
	if !strings.Contains(got, longToolPolicyMarker) || !strings.Contains(got, "8192 output tokens") || !strings.Contains(got, "multiple Edit/patch calls") {
		t.Fatalf("long-tool policy is incomplete: %q", got)
	}
	if repeated := appendLongToolPolicy(got, "claude-sonnet-4.6", []string{"Write"}); strings.Count(repeated, longToolPolicyMarker) != 1 {
		t.Fatalf("long-tool policy was duplicated: %q", repeated)
	}
	if lowRisk := appendLongToolPolicy(base, "claude-sonnet-4.6", []string{"Read", "WebSearch"}); lowRisk != base {
		t.Fatalf("low-risk tools unexpectedly changed the prompt: %q", lowRisk)
	}
	if isHighRiskToolName("credit_check") {
		t.Fatal("credit_check was incorrectly classified as an edit tool")
	}
	for _, name := range []string{"mcp__workspace__Write", "strReplaceEditor", "apply_patch", "executeCommand", "notebookedit"} {
		if !isHighRiskToolName(name) {
			t.Fatalf("expected %q to be classified as high risk", name)
		}
	}
}

func TestMaybeLongToolFallbackIsOptionalAndModelAware(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	settings := config.GetLongToolConfig()
	settings.FallbackEnabled = true
	if err := config.UpdateLongToolConfig(settings); err != nil {
		t.Fatalf("UpdateLongToolConfig: %v", err)
	}

	if got, changed := maybeLongToolFallback("claude-sonnet-4.6", 64000, []string{"Write"}); !changed || got != "claude-sonnet-5" {
		t.Fatalf("expected fallback to claude-sonnet-5, got %q changed=%v", got, changed)
	}
	if got, changed := maybeLongToolFallback("claude-sonnet-4.6", 4096, []string{"Write"}); changed || got != "claude-sonnet-4.6" {
		t.Fatalf("small tool budget unexpectedly fell back: %q changed=%v", got, changed)
	}
	if got, changed := maybeLongToolFallback("claude-sonnet-4.6", 64000, []string{"WebSearch"}); changed || got != "claude-sonnet-4.6" {
		t.Fatalf("low-risk tool unexpectedly fell back: %q changed=%v", got, changed)
	}
	settings.FallbackModel = "claude-haiku-4.5"
	if err := config.UpdateLongToolConfig(settings); err != nil {
		t.Fatalf("UpdateLongToolConfig lower-capacity fallback: %v", err)
	}
	if got, changed := maybeLongToolFallback("claude-sonnet-4.6", 64000, []string{"Write"}); changed || got != "claude-sonnet-4.6" {
		t.Fatalf("lower-capacity model unexpectedly selected: %q changed=%v", got, changed)
	}
}

func TestToolTruncationRecoveryIsBoundedAndAddsHint(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	payload := &KiroPayload{}
	payload.ConversationState.CurrentMessage.UserInputMessage.ModelID = "claude-sonnet-4.6"
	payload.ConversationState.CurrentMessage.UserInputMessage.Content = "Create the file."
	payload.beginStreamMetrics(time.Now())

	if !payload.recordToolTruncation(12000, 20, true) {
		t.Fatal("first truncation should receive one recovery attempt")
	}
	if payload.recordToolTruncation(16000, 30, true) {
		t.Fatal("second truncation exceeded the configured recovery limit")
	}
	if !strings.Contains(payload.ConversationState.CurrentMessage.UserInputMessage.Content, toolRecoveryHintMarker) {
		t.Fatal("recovery hint was not added to the retried payload")
	}
	_, _, bytes, fragments, truncations, recoveries := payload.streamMetrics()
	if bytes != 16000 || fragments != 30 || truncations != 2 || recoveries != 1 {
		t.Fatalf("unexpected truncation metrics: bytes=%d fragments=%d truncations=%d recoveries=%d", bytes, fragments, truncations, recoveries)
	}

	committed := &KiroPayload{}
	committed.ConversationState.CurrentMessage.UserInputMessage.Content = "Already streamed."
	committed.beginStreamMetrics(time.Now())
	if committed.recordToolTruncation(8000, 10, false) {
		t.Fatal("committed output must not schedule a transparent recovery")
	}
	_, _, _, _, truncations, recoveries = committed.streamMetrics()
	if truncations != 1 || recoveries != 0 || strings.Contains(committed.ConversationState.CurrentMessage.UserInputMessage.Content, toolRecoveryHintMarker) {
		t.Fatalf("committed truncation was recorded as a recovery: truncations=%d recoveries=%d content=%q", truncations, recoveries, committed.ConversationState.CurrentMessage.UserInputMessage.Content)
	}
}

func TestToolOutputTruncationDoesNotPenalizeAccountOrEndpoint(t *testing.T) {
	streamErr := &EventStreamError{Kind: EventStreamIncompleteToolUse, ToolName: "Write", ArgumentBytes: 12000, FragmentCount: 20}
	err := newToolOutputTruncatedError("Kiro Runtime", streamErr)
	if circuitEligibleFailure(err) {
		t.Fatal("tool truncation must not open the shared endpoint circuit")
	}
	if _, eligible := endpointRouteFailure(err); eligible {
		t.Fatal("tool truncation must not cool the account/model endpoint route")
	}
	(&Handler{}).handleAccountFailure(&config.Account{ID: "account"}, err)
	if !errors.Is(err, streamErr) {
		t.Fatal("tool truncation did not preserve its EventStream cause")
	}
}
