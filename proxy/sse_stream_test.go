package proxy

import (
	"bufio"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestPrepareSSEHeadersDisableIntermediaryBuffering(t *testing.T) {
	recorder := httptest.NewRecorder()
	prepareSSEHeaders(recorder)

	want := map[string]string{
		"Content-Type":      "text/event-stream; charset=utf-8",
		"Cache-Control":     "no-cache, no-transform",
		"Connection":        "keep-alive",
		"X-Accel-Buffering": "no",
	}
	for name, value := range want {
		if got := recorder.Header().Get(name); got != value {
			t.Fatalf("header %s = %q, want %q", name, got, value)
		}
	}
}

func TestResponsesStreamEmitsDoneAfterUpstreamFailure(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeIntegrityText(t, w, "partial response", false)
	}))
	defer upstream.Close()
	h := setupStreamIntegrityPathTest(t, upstream)

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"claude-sonnet-4.5",
		"input":"hello",
		"stream":true,
		"store":false
	}`))
	h.handleOpenAIResponses(recorder, request)

	body := recorder.Body.String()
	if !strings.Contains(body, "event: response.failed") {
		t.Fatalf("missing response.failed event:\n%s", body)
	}
	if count := strings.Count(body, "data: [DONE]"); count != 1 {
		t.Fatalf("expected one terminal marker, got %d:\n%s", count, body)
	}
	failedAt := strings.Index(body, "event: response.failed")
	doneAt := strings.LastIndex(body, "data: [DONE]")
	if failedAt < 0 || doneAt < failedAt {
		t.Fatalf("terminal marker did not follow failure event:\n%s", body)
	}
}

func TestResponsesStreamEmitsHeartbeatWhileUpstreamIsDelayed(t *testing.T) {
	t.Setenv("ALLOW_UNAUTHENTICATED_API", "true")
	oldInterval := claudeStreamHeartbeatInterval
	claudeStreamHeartbeatInterval = 10 * time.Millisecond
	t.Cleanup(func() { claudeStreamHeartbeatInterval = oldInterval })

	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
			"content": "responses heartbeat complete",
		}))
		_, _ = w.Write(awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{
			"stopReason": "end_turn",
		}))
	}))
	defer upstream.Close()
	h := setupStreamIntegrityPathTest(t, upstream)
	server := httptest.NewServer(h)
	defer server.Close()

	request, err := http.NewRequest(http.MethodPost, server.URL+"/v1/responses", strings.NewReader(`{
		"model":"claude-sonnet-4.5",
		"input":"wait for upstream",
		"stream":true,
		"store":false
	}`))
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := (&http.Client{Timeout: 3 * time.Second}).Do(request)
	if err != nil {
		t.Fatalf("start stream: %v", err)
	}
	defer response.Body.Close()
	if got := response.Header.Get("X-Accel-Buffering"); got != "no" {
		t.Fatalf("X-Accel-Buffering = %q, want no", got)
	}

	reader := bufio.NewReader(response.Body)
	var preamble strings.Builder
	deadline := time.After(1 * time.Second)
	for !strings.Contains(preamble.String(), ": keep-alive") {
		read := make(chan string, 1)
		readErr := make(chan error, 1)
		go func() {
			line, err := reader.ReadString('\n')
			read <- line
			readErr <- err
		}()
		select {
		case line := <-read:
			preamble.WriteString(line)
			if err := <-readErr; err != nil {
				t.Fatalf("read heartbeat: %v body=%s", err, preamble.String())
			}
		case <-deadline:
			t.Fatalf("no heartbeat before deadline; body=%s", preamble.String())
		}
	}

	close(release)
	rest, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read completed stream: %v", err)
	}
	body := preamble.String() + string(rest)
	if !strings.Contains(body, "event: response.completed") || !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("completed Responses stream is incomplete:\n%s", body)
	}
}

func TestOpenAIChatStreamsBeforeUpstreamEOF(t *testing.T) {
	finish := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
			"content": "chat output before eof",
		}))
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		select {
		case <-finish:
		case <-r.Context().Done():
			return
		}
		_, _ = w.Write(awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{
			"stopReason": "end_turn",
		}))
	}))
	defer upstream.Close()
	h := setupStreamIntegrityPathTest(t, upstream)
	server := httptest.NewServer(h)
	defer server.Close()

	response, err := (&http.Client{Timeout: 3 * time.Second}).Post(server.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{
		"model":"claude-sonnet-4.5",
		"stream":true,
		"max_tokens":128,
		"messages":[{"role":"user","content":"stream immediately"}]
	}`))
	if err != nil {
		t.Fatalf("start stream: %v", err)
	}
	defer response.Body.Close()

	reader := bufio.NewReader(response.Body)
	contentSeen := make(chan struct{})
	bodyDone := make(chan string, 1)
	go func() {
		var body strings.Builder
		seen := false
		for {
			line, readErr := reader.ReadString('\n')
			body.WriteString(line)
			if !seen && strings.Contains(body.String(), "chat output before eof") {
				seen = true
				close(contentSeen)
			}
			if readErr != nil {
				bodyDone <- body.String()
				return
			}
		}
	}()

	select {
	case <-contentSeen:
	case <-time.After(1 * time.Second):
		t.Fatal("OpenAI Chat content was held until upstream EOF")
	}
	close(finish)
	body := <-bodyDone
	if !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("OpenAI Chat stream is missing [DONE]: %s", body)
	}
}

func TestSafeUTF8PrefixBytesNeverSplitsRunes(t *testing.T) {
	text := "你好<thi"
	for length := 0; length <= len(text)+2; length++ {
		prefixLength := safeUTF8PrefixBytes(text, length)
		if prefixLength < 0 || prefixLength > len(text) {
			t.Fatalf("length %d produced invalid prefix length %d", length, prefixLength)
		}
		if !utf8.ValidString(text[:prefixLength]) {
			t.Fatalf("length %d split UTF-8 at prefix %d", length, prefixLength)
		}
	}
	if got := safeUTF8PrefixBytes(text, 4); got != len("你") {
		t.Fatalf("partial second rune backed to %d, want %d", got, len("你"))
	}
	if got := safeUTF8PrefixBytes(text, len(text)+2); got != len(text) {
		t.Fatalf("oversized prefix = %d, want %d", got, len(text))
	}
}
