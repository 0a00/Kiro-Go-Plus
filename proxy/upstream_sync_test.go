package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"kiro-go/config"
	"kiro-go/internal/httpbody"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestKiroRetryAfterHeaders(t *testing.T) {
	for _, tc := range []struct {
		name, kiro, standard string
		status               int
		want                 time.Duration
	}{
		{"milliseconds", "1500", "", 429, 1500 * time.Millisecond},
		{"server window", "45000", "2", 429, 45 * time.Second},
		{"standard longer", "1500", "60", 429, time.Minute},
		{"clamp before conversion", "18446744073709551615", "", 429, 5 * time.Minute},
		{"invalid", "not-a-number", "7", 429, 7 * time.Second},
		{"negative", "-1", "", 429, 0},
		{"zero", "0", "2", 429, 2 * time.Second},
		{"wrong status", "1500", "", 403, 0},
		{"standard overflow", "", "9223372036854775807", 429, 24 * time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := &http.Response{StatusCode: tc.status, Header: make(http.Header)}
			response.Header.Set("Retry-After", tc.standard)
			response.Header.Set("x-amzn-kiro-ratelimit-retry-after", tc.kiro)
			err := classifyKiroHTTPResponseError(response, "Kiro", []byte(`{"message":"rate limited"}`))
			if err.RetryAfter != tc.want {
				t.Fatalf("retry delay=%s, want %s", err.RetryAfter, tc.want)
			}
		})
	}
}

func TestKiroRateLimitWindowReachesEndpointCooldown(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("x-amzn-kiro-ratelimit-retry-after", "45000")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"message":"rate limited"}`)
	}))
	defer upstream.Close()
	_ = config.UpdatePreferredEndpoint("kiro")
	_ = config.UpdateEndpointFallback(false)
	oldEndpoints := kiroEndpoints
	kiroEndpoints = []kiroEndpoint{{Key: "kiro", Name: "Kiro", URL: upstream.URL}}
	t.Cleanup(func() { kiroEndpoints = oldEndpoints })
	sharedAccountEndpointRoutes.reset()
	t.Cleanup(sharedAccountEndpointRoutes.reset)
	payload := &KiroPayload{}
	payload.ConversationState.CurrentMessage.UserInputMessage.ModelID = "claude-sonnet-4.5"
	account := &config.Account{ID: "header-cooldown", AccessToken: "fixture-token"}
	err := CallKiroAPI(account, payload, &KiroStreamCallback{})
	classified, ok := asUpstreamError(err)
	if !ok || classified.RetryAfter != 45*time.Second {
		t.Fatalf("missing header delay: %v", err)
	}
	_, err = sharedAccountEndpointRoutes.availableEndpoints(account.ID, "claude-sonnet-4.5", "auto", kiroEndpoints)
	cooled, ok := asUpstreamError(err)
	if !ok || cooled.RetryAfter < 44*time.Second {
		t.Fatalf("route not cooling for server window: %v", err)
	}
}

func TestMCPResponseFormatsAndDates(t *testing.T) {
	for _, tc := range []struct {
		name, body, wantTitle string
		wantError             bool
	}{
		{"structured", `{"result":{"structuredContent":{"results":[{"title":"structured","url":"https://example.invalid","publishedDate":"2026-09-28T12:00:00Z"}]},"content":[{"type":"text","text":"not json"}]}}`, "structured", false},
		{"structured empty", `{"result":{"structuredContent":{"results":[]},"content":[{"type":"text","text":"not json"}]}}`, "", false},
		{"legacy date", `{"result":{"content":[{"type":"text","text":"{\"results\":[{\"title\":\"legacy\",\"publishedDate\":\"2026-09-28\"}]}"}]}}`, "legacy", false},
		{"invalid structured fallback", `{"result":{"structuredContent":{},"content":[{"type":"text","text":"{\"results\":[{\"title\":\"fallback\"}]}"}]}}`, "fallback", false},
		{"isError", `{"result":{"isError":true,"structuredContent":{"results":[]}}}`, "", true},
		{"rpc error", `{"error":{"code":-32000,"message":"fixture failure"}}`, "", true},
		{"structured error not hidden", `{"result":{"structuredContent":{"error":"search failed"},"content":[{"type":"text","text":"{\"results\":[]}"}]}}`, "", true},
		{"text error not hidden", `{"result":{"content":[{"type":"text","text":"{\"error\":\"search failed\"}"},{"type":"text","text":"{\"results\":[]}"}]}}`, "", true},
		{"missing results", `{"result":{"structuredContent":{}}}`, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, sse := range []bool{false, true} {
				body, contentType := tc.body, "application/json"
				if sse {
					body = ": heartbeat\n\nevent: message\ndata: {\"method\":\"notifications/progress\"}\n\ndata: " + body + "\n\n"
					contentType = "text/event-stream"
				}
				mcp, err := decodeMCPResponse(strings.NewReader(body), contentType, "")
				if err != nil {
					t.Fatal(err)
				}
				result, err := parseMCPWebSearchResults(mcp, "query")
				if (err != nil) != tc.wantError {
					t.Fatalf("sse=%v err=%v", sse, err)
				}
				if err != nil {
					continue
				}
				if result.Query != "query" || result.Results == nil {
					t.Fatalf("invalid result: %+v", result)
				}
				if tc.wantTitle != "" && (len(result.Results) != 1 || result.Results[0].Title != tc.wantTitle) {
					t.Fatalf("unexpected results: %+v", result)
				}
				if (tc.name == "structured" || tc.name == "legacy date") && result.Results[0].PublishedAt <= 0 {
					t.Fatal("date was lost")
				}
			}
		})
	}
}

func TestMCPWebSearchStopsAfterMatchingSSEEvent(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") != "application/json, text/event-stream" {
			t.Error("missing MCP Accept header")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"id\":\"other\",\"result\":{\"structuredContent\":{\"results\":[{\"title\":\"wrong\"}]}}}\n\n")
		_, _ = io.WriteString(w, "data: {\"id\":\"requested\",\n"+"data: \"result\":{\"structuredContent\":{\"results\":[]}}}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	defer close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	results, err := callMCPWebSearchURL(ctx, &config.Account{AccessToken: "fixture-token"}, server.URL, []byte(`{"id":"requested"}`), "query")
	if err != nil || len(results.Results) != 0 {
		t.Fatalf("search didn't finish on matching result: %+v %v", results, err)
	}
}

func TestMCPResponseIsBoundedAndRejectsUnmatchedID(t *testing.T) {
	for _, body := range []string{
		`{"id":"other","result":{"structuredContent":{"results":[]}}}`,
		"data: {\"id\":\"other\",\"result\":{}}\n\n",
		"data: {\"result\":{\"ignored\":\"" + strings.Repeat("x", int(httpbody.DefaultLimit)) + "\"}}\n\n",
	} {
		if _, err := decodeMCPResponse(strings.NewReader(body), "", "requested"); err == nil {
			t.Fatal("invalid response accepted")
		}
	}
}

func TestNativeSearchQueryRuneLimit(t *testing.T) {
	query := normalizeWebSearchQuery("Perform a web search for the query: " + strings.Repeat("中", 201))
	if !utf8.ValidString(query) || utf8.RuneCountInString(query) != 200 {
		t.Fatal("invalid query truncation")
	}
}

func TestClaudeServerToolsValidateBeforeUpstreamDispatch(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"web_fetch_20250910", "code_execution_20250522", "bash_code_execution_20250825", "text_editor_code_execution_20250825"} {
		rec := httptest.NewRecorder()
		body := fmt.Sprintf(`{"model":"claude-sonnet-4.5","max_tokens":128,"messages":[{"role":"user","content":"hello"}],"tools":[{"type":%q,"name":"server_tool"}]}`, kind)
		(&Handler{}).handleClaudeMessages(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body)))
		if rec.Code != 400 || !strings.Contains(rec.Body.String(), "server tool type") {
			t.Fatalf("unsupported tool response: %d %s", rec.Code, rec.Body.String())
		}
	}
	for _, kind := range []string{"", "custom", "bash_20250124", "computer_20250124", "text_editor_20250124"} {
		if err := validateClaudeServerTools([]ClaudeTool{{Name: "client_tool", Type: kind}}); err != nil {
			t.Fatal(err)
		}
	}
	req := &ClaudeRequest{Tools: []ClaudeTool{{Name: "ignored", Type: "code_execution_20250522"}}, ToolChoice: "none"}
	if err := prepareClaudeToolPolicy(req, false); err != nil || len(req.Tools) != 0 {
		t.Fatalf("tool_choice=none: %v", err)
	}
	settings := config.GetWebSearchConfig()
	settings.Enabled = false
	if err := config.UpdateWebSearchConfig(settings); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"web_search_20250305", "WEB-SEARCH-20250305"} {
		if err := validateClaudeServerTools([]ClaudeTool{{Name: "web_search", Type: kind}}); err == nil {
			t.Fatal("disabled search escaped validation")
		}
	}
}

func TestForcedSearchDistinguishesNativeFromClientTools(t *testing.T) {
	var req ClaudeRequest
	if err := json.Unmarshal([]byte(`{"tools":[{"name":"web_search","type":"web_search_20250305"},{"name":"Bash"}],"tool_choice":{"type":"tool","name":"web_search"}}`), &req); err != nil {
		t.Fatal(err)
	}
	if !hasForcedNativeWebSearchTool(&req) {
		t.Fatal("forced native search missed")
	}
	req.Tools[0].Type = "custom"
	if hasForcedNativeWebSearchTool(&req) {
		t.Fatal("client web_search hijacked")
	}
}
