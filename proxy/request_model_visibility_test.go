package proxy

import (
	"context"
	"encoding/json"
	"kiro-go/config"
	"strings"
	"testing"
)

func TestPublicErrorTextHidesFallbackModelAliases(t *testing.T) {
	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatalf("init config: %v", err)
	}
	ctx := withRequestedModel(context.Background(), "claude-opus-5-thinking")
	markModelRoute(ctx, "claude-sonnet-4.5")

	message := "INVALID_MODEL_ID: claude-sonnet-4.5 and claude-sonnet-4-5-thinking are unavailable"
	got := publicErrorText(ctx, message)
	if strings.Contains(strings.ToLower(got), "claude-sonnet-4.5") || strings.Contains(strings.ToLower(got), "claude-sonnet-4-5") {
		t.Fatalf("fallback target leaked in error: %q", got)
	}
	if !strings.Contains(got, "claude-opus-5-thinking") {
		t.Fatalf("requested model was not retained in public error: %q", got)
	}
}

func TestReplaceFoldDoesNotReprocessReplacement(t *testing.T) {
	got := replaceFold("claude-sonnet-4", "claude-sonnet-4", "claude-sonnet-4.5")
	if got != "claude-sonnet-4.5" {
		t.Fatalf("unexpected replacement: %q", got)
	}
}

func TestRequestLogMigratesLegacyFallbackMetadata(t *testing.T) {
	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatalf("init config: %v", err)
	}
	legacy := []byte(`{"id":7,"protocol":"claude.messages","model":"claude-sonnet-4.5","modelFallbackApplied":true,"modelFallbackFrom":"claude-opus-5","modelFallbackTo":"claude-sonnet-4.5","modelFallbackRuleId":"always","error":"upstream model claude-sonnet-4.5 unavailable"}`)
	var entry requestLogEntry
	if err := json.Unmarshal(legacy, &entry); err != nil {
		t.Fatalf("decode legacy request log: %v", err)
	}
	if entry.Model != "claude-opus-5" {
		t.Fatalf("legacy public model was not restored: %+v", entry)
	}
	if strings.Contains(entry.Error, "claude-sonnet-4.5") {
		t.Fatalf("legacy fallback target remains in error: %q", entry.Error)
	}
	encoded, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("encode migrated request log: %v", err)
	}
	for _, field := range []string{"modelFallbackApplied", "modelFallbackFrom", "modelFallbackTo", "modelFallbackRuleId"} {
		if strings.Contains(string(encoded), field) {
			t.Fatalf("internal fallback field was emitted: %s", encoded)
		}
	}
}

func TestPublicRequestDetailUsesOriginalModelAndCleansErrors(t *testing.T) {
	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatalf("init config: %v", err)
	}
	detail := requestDetail{
		Model: "claude-sonnet-4.5",
		Request: requestDetailRequest{
			BodyJSON: `{"model":"claude-opus-5-thinking","messages":[]}`,
		},
		Response: requestDetailResponse{Error: "target claude-sonnet-4.5 failed"},
		Attempts: []requestDetailAttempt{{Error: "retrying claude-sonnet-4-5"}},
	}
	clean := publicRequestDetail(context.Background(), detail)
	if clean.Model != "claude-opus-5-thinking" {
		t.Fatalf("detail model leaked fallback target: %+v", clean)
	}
	if strings.Contains(clean.Response.Error, "claude-sonnet-4.5") || strings.Contains(clean.Attempts[0].Error, "claude-sonnet-4-5") {
		t.Fatalf("detail error leaked fallback target: %+v", clean)
	}
}

func TestSanitizeLogArchiveLineHidesLegacyRequestTarget(t *testing.T) {
	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatalf("init config: %v", err)
	}
	record := logArchiveRecord{
		Version:   1,
		Kind:      "request",
		RequestID: "req-legacy",
		Data:      json.RawMessage(`{"model":"claude-sonnet-4.5","modelFallbackApplied":true,"modelFallbackFrom":"claude-opus-5","modelFallbackTo":"claude-sonnet-4.5","error":"claude-sonnet-4.5 failed"}`),
	}
	raw, err := json.Marshal(record)
	if err != nil {
		t.Fatalf("encode archive fixture: %v", err)
	}
	clean, err := sanitizeLogArchiveLine(append(raw, '\n'))
	if err != nil {
		t.Fatalf("sanitize archive fixture: %v", err)
	}
	if strings.Contains(string(clean), "claude-sonnet-4.5") || strings.Contains(string(clean), "modelFallbackTo") {
		t.Fatalf("legacy archive target leaked: %s", clean)
	}
	if !strings.Contains(string(clean), "claude-opus-5") {
		t.Fatalf("archive lost public requested model: %s", clean)
	}
}
