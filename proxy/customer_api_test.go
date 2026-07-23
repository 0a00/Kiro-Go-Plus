package proxy

import (
	"encoding/json"
	"kiro-go/config"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func addCustomerTestKey(t *testing.T, entry config.ApiKeyEntry) config.ApiKeyEntry {
	t.Helper()
	created, err := config.AddApiKey(entry)
	if err != nil {
		t.Fatalf("add API key: %v", err)
	}
	return created
}

func serveCustomerRequest(h *Handler, request *http.Request) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, request)
	return recorder
}

func customerRequest(method, path, key string) *http.Request {
	request := httptest.NewRequest(method, path, nil)
	if key != "" {
		request.Header.Set("Authorization", "Bearer "+key)
	}
	return request
}

func TestCustomerEndpointsRequireKnownAPIKey(t *testing.T) {
	mustInitConfig(t)
	handler := &Handler{}
	for _, path := range []string{"/api/me", "/api/stats", "/api/logs", "/v1/stats"} {
		for _, key := range []string{"", "sk-unknown"} {
			recorder := serveCustomerRequest(handler, customerRequest(http.MethodGet, path, key))
			if recorder.Code != http.StatusUnauthorized {
				t.Fatalf("%s with key %q returned %d, want 401", path, key, recorder.Code)
			}
			if recorder.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("%s missing no-store cache policy", path)
			}
		}
	}
}

func TestCustomerMeWorksForDisabledAndExhaustedKeys(t *testing.T) {
	mustInitConfig(t)
	disabled := addCustomerTestKey(t, config.ApiKeyEntry{
		Name: "disabled", Key: "sk-customer-disabled", Enabled: false,
	})
	exhausted := addCustomerTestKey(t, config.ApiKeyEntry{
		Name: "exhausted", Key: "sk-customer-exhausted", Enabled: true, CreditLimit: 50,
	})
	if err := config.RecordApiKeyUsage(exhausted.ID, 200, 50); err != nil {
		t.Fatalf("record exhausted usage: %v", err)
	}

	tests := []struct {
		entry config.ApiKeyEntry
		want  string
	}{
		{entry: disabled, want: "disabled"},
		{entry: exhausted, want: "exhausted"},
	}
	for _, test := range tests {
		recorder := serveCustomerRequest(&Handler{}, customerRequest(http.MethodGet, "/api/me", test.entry.Key))
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s key returned %d: %s", test.want, recorder.Code, recorder.Body.String())
		}
		var body customerMeView
		if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode %s response: %v", test.want, err)
		}
		if body.Status != test.want {
			t.Fatalf("status = %q, want %q", body.Status, test.want)
		}
		if strings.Contains(recorder.Body.String(), test.entry.Key) || strings.Contains(recorder.Body.String(), test.entry.ID) {
			t.Fatalf("credential or internal key ID leaked in /api/me: %s", recorder.Body.String())
		}
	}
}

func TestCustomerStatsAndLegacyAliasAreKeyScoped(t *testing.T) {
	mustInitConfig(t)
	current := addCustomerTestKey(t, config.ApiKeyEntry{
		Name: "current", Key: "sk-customer-current", Enabled: true, CreditLimit: 1000,
	})
	other := addCustomerTestKey(t, config.ApiKeyEntry{
		Name: "other", Key: "sk-customer-other", Enabled: true, CreditLimit: 1000,
	})
	if err := config.RecordApiKeyUsage(current.ID, 120, 12.5); err != nil {
		t.Fatalf("record current usage: %v", err)
	}
	if err := config.RecordApiKeyUsage(other.ID, 900, 90); err != nil {
		t.Fatalf("record other usage: %v", err)
	}

	for _, path := range []string{"/api/stats", "/v1/stats"} {
		handler := &Handler{}
		handler.totalRequests.Store(9999)
		recorder := serveCustomerRequest(handler, customerRequest(http.MethodGet, path, current.Key))
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s returned %d: %s", path, recorder.Code, recorder.Body.String())
		}
		var body map[string]interface{}
		if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
		if body["tokensUsed"] != float64(120) || body["creditsUsed"] != 12.5 {
			t.Fatalf("%s returned non-scoped usage: %+v", path, body)
		}
		for _, forbidden := range []string{"accounts", "available", "totalRequests", "successRequests", "failedRequests", "totalCredits"} {
			if _, exists := body[forbidden]; exists {
				t.Fatalf("%s exposed global field %q", path, forbidden)
			}
		}
	}
}

func TestCustomerLogsAreIsolatedAndSanitized(t *testing.T) {
	mustInitConfig(t)
	current := addCustomerTestKey(t, config.ApiKeyEntry{
		Name: "current-private-name", Key: "sk-customer-logs", Enabled: false,
	})
	other := addCustomerTestKey(t, config.ApiKeyEntry{
		Name: "other-private-name", Key: "sk-customer-other-logs", Enabled: true,
	})
	firstContent := int64(125)
	handler := &Handler{requestLog: newRequestLog(100)}
	handler.requestLog.add(requestLogEntry{
		Timestamp: 1, RequestID: "request-secret", APIKeyID: current.ID, APIKeyName: current.Name,
		Protocol: "claude.messages.stream", Model: "claude-sonnet", AccountID: "account-secret",
		AccountEmail: "account@example.com", Endpoint: "runtime-secret", Status: "error",
		StatusCode: http.StatusBadGateway, Error: "upstream response contained credential-secret",
		RequestToolNames: []string{"private_tool_name"}, FirstContentMs: &firstContent,
		DurationMs: 500, InputTokens: 20, OutputTokens: 10, Credits: 1.5,
	})
	handler.requestLog.add(requestLogEntry{
		Timestamp: 2, APIKeyID: other.ID, Protocol: "openai.chat", Model: "other-private-model",
		Status: "success", StatusCode: http.StatusOK,
	})
	handler.requestLog.add(requestLogEntry{
		Timestamp: 3, APIKeyID: current.ID, Protocol: "openai.responses", Model: "claude-haiku",
		Status: "success", StatusCode: http.StatusOK, DurationMs: 250,
	})

	recorder := serveCustomerRequest(handler, customerRequest(http.MethodGet, "/api/logs?limit=10", current.Key))
	if recorder.Code != http.StatusOK {
		t.Fatalf("logs returned %d: %s", recorder.Code, recorder.Body.String())
	}
	var body struct {
		Logs  []customerRequestLogView `json:"logs"`
		Count int                      `json:"count"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode logs: %v", err)
	}
	if body.Count != 2 || len(body.Logs) != 2 || body.Logs[0].Timestamp != 3 || body.Logs[1].Timestamp != 1 {
		t.Fatalf("unexpected isolated log order: %+v", body.Logs)
	}
	if body.Logs[1].ErrorCategory != "upstream" || body.Logs[1].FirstContentMs == nil || *body.Logs[1].FirstContentMs != 125 {
		t.Fatalf("unexpected sanitized failure view: %+v", body.Logs[1])
	}
	response := recorder.Body.String()
	for _, forbidden := range []string{
		current.ID, current.Key, current.Name, other.ID, other.Name, "account-secret",
		"account@example.com", "runtime-secret", "credential-secret", "private_tool_name", "other-private-model",
	} {
		if strings.Contains(response, forbidden) {
			t.Fatalf("customer log response leaked %q: %s", forbidden, response)
		}
	}
}

func TestCustomerLogsRejectInvalidLimit(t *testing.T) {
	mustInitConfig(t)
	entry := addCustomerTestKey(t, config.ApiKeyEntry{Key: "sk-customer-limit", Enabled: true})
	recorder := serveCustomerRequest(&Handler{}, customerRequest(http.MethodGet, "/api/logs?limit=invalid", entry.Key))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("invalid limit returned %d, want 400", recorder.Code)
	}
}
