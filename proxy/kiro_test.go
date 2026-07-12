package proxy

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"hash/crc32"
	"kiro-go/config"
	"net/http"
	"net/url"
	"testing"
	"time"
)

func TestNormalizeChunkBasicProgression(t *testing.T) {
	prev := ""

	if got := normalizeChunk("abc", &prev); got != "abc" {
		t.Fatalf("expected first chunk to pass through, got %q", got)
	}
	if got := normalizeChunk("abcde", &prev); got != "de" {
		t.Fatalf("expected appended delta, got %q", got)
	}
}

func TestNormalizeChunkPrefixRewindDoesNotReplay(t *testing.T) {
	prev := ""

	_ = normalizeChunk("abcde", &prev)
	if got := normalizeChunk("abc", &prev); got != "" {
		t.Fatalf("expected rewind chunk to be ignored, got %q", got)
	}
	if prev != "abcde" {
		t.Fatalf("expected previous snapshot to remain longest version, got %q", prev)
	}
	if got := normalizeChunk("abcdef", &prev); got != "f" {
		t.Fatalf("expected only unseen suffix after rewind, got %q", got)
	}
}

func TestNormalizeChunkOverlapDelta(t *testing.T) {
	prev := "hello world"

	if got := normalizeChunk("world!!!", &prev); got != "!!!" {
		t.Fatalf("expected overlap suffix delta, got %q", got)
	}
}

func TestParseEventStreamRejectsPendingToolUseOnEOF(t *testing.T) {
	stream := bytes.NewReader(awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
		"toolUseId": "toolu_1",
		"name":      "mcpIdaProMcpStatus",
		"input":     `{"server":"ida-pro-mcp"}`,
	}))

	var toolUses []KiroToolUse
	err := parseEventStream(stream, &KiroStreamCallback{
		OnToolUse: func(toolUse KiroToolUse) {
			toolUses = append(toolUses, toolUse)
		},
	})
	var streamErr *EventStreamError
	if !errors.As(err, &streamErr) || streamErr.Kind != EventStreamIncompleteToolUse {
		t.Fatalf("expected incomplete tool-use error, got %#v", err)
	}
	if len(toolUses) != 0 {
		t.Fatalf("incomplete tool use must not be emitted, got %d", len(toolUses))
	}
}

func TestParseEventStreamNilCallbackIsNoOp(t *testing.T) {
	stream := bytes.NewReader(bytes.Join([][]byte{
		awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "hello"}),
		awsEventStreamFrame(t, "reasoningContentEvent", map[string]interface{}{"text": "thinking"}),
		awsEventStreamFrame(t, "contextUsageEvent", map[string]interface{}{"contextUsagePercentage": 12.5}),
		awsEventStreamFrame(t, "meteringEvent", map[string]interface{}{"usage": 1.25}),
		awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
			"name":  "mcpIdaProMcpStatus",
			"input": `{"server":"ida-pro-mcp"}`,
			"stop":  true,
		}),
	}, nil))

	if err := parseEventStream(stream, nil); err != nil {
		t.Fatalf("expected nil callback to be a no-op, got %v", err)
	}
}

func TestParseEventStreamNilCallbackFieldsAreNoOp(t *testing.T) {
	stream := bytes.NewReader(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
		"content": "hello",
	}))

	if err := parseEventStream(stream, &KiroStreamCallback{}); err != nil {
		t.Fatalf("expected empty callback to be a no-op, got %v", err)
	}
}

func TestHandleToolUseEventGeneratesMissingToolUseID(t *testing.T) {
	var toolUses []KiroToolUse
	current, err := handleToolUseEvent(map[string]interface{}{
		"name":  "mcpIdaProMcpStatus",
		"input": `{"server":"ida-pro-mcp"}`,
		"stop":  true,
	}, nil, &KiroStreamCallback{
		OnToolUse: func(toolUse KiroToolUse) {
			toolUses = append(toolUses, toolUse)
		},
	})
	if err != nil {
		t.Fatalf("unexpected tool-use error: %v", err)
	}

	if current != nil {
		t.Fatalf("expected stopped tool use to clear current state")
	}
	if len(toolUses) != 1 {
		t.Fatalf("expected one tool use, got %d", len(toolUses))
	}
	if toolUses[0].ToolUseID == "" {
		t.Fatalf("expected generated tool use id")
	}
	if toolUses[0].Name != "mcpIdaProMcpStatus" {
		t.Fatalf("unexpected tool name: %q", toolUses[0].Name)
	}
}

func TestHandleToolUseEventReplacesGeneratedIDWhenRealIDArrives(t *testing.T) {
	var toolUses []KiroToolUse
	callback := &KiroStreamCallback{
		OnToolUse: func(toolUse KiroToolUse) {
			toolUses = append(toolUses, toolUse)
		},
	}

	current, err := handleToolUseEvent(map[string]interface{}{
		"name":  "mcpIdaProMcpStatus",
		"input": `{"server":`,
	}, nil, callback)
	if err != nil {
		t.Fatalf("unexpected first tool-use error: %v", err)
	}
	current, err = handleToolUseEvent(map[string]interface{}{
		"toolUseId": "toolu_real",
		"name":      "mcpIdaProMcpStatus",
		"input":     `"ida-pro-mcp"}`,
		"stop":      true,
	}, current, callback)
	if err != nil {
		t.Fatalf("unexpected completed tool-use error: %v", err)
	}

	if current != nil {
		t.Fatalf("expected stopped tool use to clear current state")
	}
	if len(toolUses) != 1 {
		t.Fatalf("expected one completed tool use, got %d", len(toolUses))
	}
	if toolUses[0].ToolUseID != "toolu_real" {
		t.Fatalf("expected real tool id to replace generated id, got %q", toolUses[0].ToolUseID)
	}
	if got := toolUses[0].Input["server"]; got != "ida-pro-mcp" {
		t.Fatalf("expected joined tool input, got %#v", toolUses[0].Input)
	}
}

func TestHandleToolUseEventIgnoresEmptyObjectBeforeArgumentFragments(t *testing.T) {
	var toolUses []KiroToolUse
	callback := &KiroStreamCallback{OnToolUse: func(toolUse KiroToolUse) {
		toolUses = append(toolUses, toolUse)
	}}

	current, err := handleToolUseEvent(map[string]interface{}{
		"toolUseId": "toolu_fragmented",
		"name":      "read_file",
		"input":     map[string]interface{}{},
	}, nil, callback)
	if err != nil {
		t.Fatalf("unexpected initial tool-use error: %v", err)
	}
	current, err = handleToolUseEvent(map[string]interface{}{
		"toolUseId": "toolu_fragmented",
		"name":      "read_file",
		"input":     `{"path":"README.md"}`,
		"stop":      true,
	}, current, callback)
	if err != nil {
		t.Fatalf("unexpected completed tool-use error: %v", err)
	}
	if current != nil || len(toolUses) != 1 {
		t.Fatalf("expected one completed tool use, current=%+v uses=%d", current, len(toolUses))
	}
	if got := toolUses[0].Input["path"]; got != "README.md" {
		t.Fatalf("expected valid joined arguments, got %#v", toolUses[0].Input)
	}
}

func TestBuildKiroTransportUsesExplicitProxyURL(t *testing.T) {
	transport, err := buildKiroTransport("http://proxy.local:8080")
	if err != nil {
		t.Fatalf("build transport: %v", err)
	}
	req := &http.Request{URL: mustParseURL(t, "https://q.us-east-1.amazonaws.com")}

	got, err := transport.Proxy(req)
	if err != nil {
		t.Fatalf("unexpected proxy error: %v", err)
	}
	assertProxyURL(t, got, "http://proxy.local:8080")
}

func TestBuildKiroTransportFallsBackToEnvironmentProxy(t *testing.T) {
	transport, err := buildKiroTransport("")
	if err != nil {
		t.Fatalf("build transport: %v", err)
	}
	if transport.Proxy == nil {
		t.Fatal("expected empty proxy setting to retain environment proxy resolution")
	}
}

func TestBuildKiroTransportDirectBypassesEnvironmentProxy(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://env-proxy.local:2323")
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")

	transport, err := buildKiroTransport("direct")
	if err != nil {
		t.Fatalf("build transport: %v", err)
	}
	if transport.Proxy != nil {
		t.Fatalf("expected direct transport to have no proxy function")
	}
}

func TestBuildKiroTransportRejectsMalformedProxyInsteadOfDirectFallback(t *testing.T) {
	if _, err := buildKiroTransport("http://proxy-without-port"); err == nil {
		t.Fatal("expected malformed proxy to fail transport construction")
	}
	if _, err := GetClientForProxy("socks5://:1080"); err == nil {
		t.Fatal("expected malformed account proxy to fail closed")
	}
}

func TestInitKiroHttpClientUsesIdleTimeoutForStreamsAndShortRestTimeout(t *testing.T) {
	InitKiroHttpClient("")
	t.Cleanup(func() { InitKiroHttpClient("") })

	streamClient := kiroHttpStore.Load()
	restClient := kiroRestHttpStore.Load()

	if streamClient.Timeout != 0 {
		t.Fatalf("expected no total streaming timeout, got %s", streamClient.Timeout)
	}
	if restClient.Timeout != 30*time.Second {
		t.Fatalf("expected REST timeout to stay 30s, got %s", restClient.Timeout)
	}
}

func TestSetPayloadProfileArnForAccountUsesAccountArn(t *testing.T) {
	payload := &KiroPayload{ProfileArn: "arn:aws:codewhisperer:profile/stale"}

	setPayloadProfileArnForAccount(payload, &config.Account{ProfileArn: " arn:aws:codewhisperer:profile/current "})
	if payload.ProfileArn != "arn:aws:codewhisperer:profile/current" {
		t.Fatalf("expected current account profile ARN, got %q", payload.ProfileArn)
	}
}

func TestSetPayloadProfileArnForAccountPreservesExplicitPayloadArn(t *testing.T) {
	payload := &KiroPayload{ProfileArn: " arn:aws:codewhisperer:profile/explicit "}

	setPayloadProfileArnForAccount(payload, &config.Account{})
	if payload.ProfileArn != "arn:aws:codewhisperer:profile/explicit" {
		t.Fatalf("expected explicit payload profile ARN to be preserved, got %q", payload.ProfileArn)
	}
}

func TestKiroIDEEndpointResolvesAccountRegion(t *testing.T) {
	ep := kiroEndpoint{
		URL:    "https://q.us-east-1.amazonaws.com/generateAssistantResponse",
		Origin: "AI_EDITOR",
		Name:   "Kiro IDE",
	}

	got := ep.ResolveURL(&config.Account{Region: "eu-west-1"})
	if got != "https://q.eu-west-1.amazonaws.com/generateAssistantResponse" {
		t.Fatalf("expected account region endpoint, got %q", got)
	}
}

func TestKiroIDEEndpointDefaultsToUSEast1(t *testing.T) {
	ep := kiroEndpoint{
		URL:    "https://q.us-east-1.amazonaws.com/generateAssistantResponse",
		Origin: "AI_EDITOR",
		Name:   "Kiro IDE",
	}

	got := ep.ResolveURL(&config.Account{})
	if got != "https://q.us-east-1.amazonaws.com/generateAssistantResponse" {
		t.Fatalf("expected default endpoint, got %q", got)
	}
}

func TestNonKiroIDEEndpointKeepsConfiguredURL(t *testing.T) {
	ep := kiroEndpoint{
		URL:    "http://example.test/generate",
		Origin: "AI_EDITOR",
		Name:   "test",
	}

	got := ep.ResolveURL(&config.Account{Region: "eu-west-1"})
	if got != ep.URL {
		t.Fatalf("expected configured URL to be preserved, got %q", got)
	}
}

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("invalid test URL: %v", err)
	}
	return parsed
}

func assertProxyURL(t *testing.T, got *url.URL, want string) {
	t.Helper()
	if got == nil {
		t.Fatalf("expected proxy URL %q, got nil", want)
	}
	if got.String() != want {
		t.Fatalf("expected proxy URL %q, got %q", want, got.String())
	}
}

func awsEventStreamFrame(t *testing.T, eventType string, payload map[string]interface{}) []byte {
	t.Helper()

	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	headerValue := []byte(eventType)
	headers := make([]byte, 0, 1+len(":event-type")+1+2+len(headerValue))
	headers = append(headers, byte(len(":event-type")))
	headers = append(headers, []byte(":event-type")...)
	headers = append(headers, byte(7))
	headers = append(headers, byte(len(headerValue)>>8), byte(len(headerValue)))
	headers = append(headers, headerValue...)

	totalLength := 12 + len(headers) + len(payloadBytes) + 4
	frame := make([]byte, 12, totalLength)
	binary.BigEndian.PutUint32(frame[0:4], uint32(totalLength))
	binary.BigEndian.PutUint32(frame[4:8], uint32(len(headers)))
	binary.BigEndian.PutUint32(frame[8:12], crc32.ChecksumIEEE(frame[0:8]))
	frame = append(frame, headers...)
	frame = append(frame, payloadBytes...)
	frame = append(frame, 0, 0, 0, 0)
	binary.BigEndian.PutUint32(frame[len(frame)-4:], crc32.ChecksumIEEE(frame[:len(frame)-4]))
	return frame
}
