package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	maxDiagnosticResponseBytes = 16 << 20
	maxSSELineBytes            = 4 << 20
	maxCapturedOutputBytes     = 64 << 10
)

// errResponseTooLarge keeps oversized diagnostics distinct from network
// failures. The live service may legitimately stream for a long time, but a
// diagnostic client must still have a finite memory budget.
var errResponseTooLarge = errors.New("response exceeded diagnostic size limit")

func newHTTPClient() *http.Client {
	baseTransport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return &http.Client{CheckRedirect: rejectRedirects}
	}
	transport := baseTransport.Clone()
	transport.MaxIdleConns = 1024
	transport.MaxIdleConnsPerHost = 1024
	transport.IdleConnTimeout = 90 * time.Second
	return &http.Client{
		Transport:     transport,
		CheckRedirect: rejectRedirects,
	}
}

func rejectRedirects(_ *http.Request, _ []*http.Request) error {
	return http.ErrUseLastResponse
}

type apiResponse struct {
	statusCode int
	body       []byte
	stream     sseStats
	headers    time.Duration
	total      time.Duration
	requestID  string
	err        error
}

type tokenUsage struct {
	inputTokens         int
	outputTokens        int
	reasoningTokens     int
	cacheReadTokens     int
	cacheCreationTokens int
}

func (u *tokenUsage) merge(other tokenUsage) {
	u.inputTokens = max(u.inputTokens, other.inputTokens)
	u.outputTokens = max(u.outputTokens, other.outputTokens)
	u.reasoningTokens = max(u.reasoningTokens, other.reasoningTokens)
	u.cacheReadTokens = max(u.cacheReadTokens, other.cacheReadTokens)
	u.cacheCreationTokens = max(u.cacheCreationTokens, other.cacheCreationTokens)
}

func (u tokenUsage) anthropicInputTotal() int {
	return u.inputTokens + u.cacheReadTokens + u.cacheCreationTokens
}

type sseToolState struct {
	id           string
	name         string
	initialInput string
	deltas       strings.Builder
	stopped      bool
}

type sseStats struct {
	events           int
	contentDeltas    int
	thinkingDeltas   int
	toolCalls        int
	serverToolCalls  int
	webSearchResults int
	heartbeats       int
	contentChars     int
	thinkingChars    int
	firstEvent       time.Duration
	firstSemantic    time.Duration
	firstText        time.Duration
	firstThinking    time.Duration
	firstTool        time.Duration
	lastEvent        time.Duration
	maxEventGap      time.Duration
	lastActivity     time.Duration
	maxActivityGap   time.Duration
	activities       int
	semanticOutput   bool
	textOutput       bool
	thinkingOutput   bool
	toolOutput       bool
	terminal         bool
	terminalType     string
	incomplete       bool
	errorEvent       string
	stopReason       string
	usage            tokenUsage
	tools            map[int]*sseToolState
	output           []byte
	outputTruncated  bool
}

func (r *runner) get(ctx context.Context, path string, authenticated bool) apiResponse {
	return r.do(ctx, http.MethodGet, path, nil, authenticated, false)
}

func (r *runner) post(ctx context.Context, path string, payload interface{}, authenticated, stream bool) apiResponse {
	return r.postWithSSEObserver(ctx, path, payload, authenticated, stream, nil)
}

func (r *runner) postWithSSEObserver(ctx context.Context, path string, payload interface{}, authenticated, stream bool, onEvent func()) apiResponse {
	data, err := json.Marshal(payload)
	if err != nil {
		return apiResponse{err: err}
	}
	return r.doWithSSEObserver(ctx, http.MethodPost, path, data, authenticated, stream, onEvent)
}

func (r *runner) do(ctx context.Context, method, path string, body []byte, authenticated, stream bool) apiResponse {
	return r.doWithSSEObserver(ctx, method, path, body, authenticated, stream, nil)
}

func (r *runner) doWithSSEObserver(ctx context.Context, method, path string, body []byte, authenticated, stream bool, onEvent func()) apiResponse {
	startedAt := time.Now()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, r.opts.baseURL+path, reader)
	if err != nil {
		return apiResponse{err: err}
	}
	req.Header.Set("User-Agent", r.userAgent)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if strings.HasPrefix(path, "/v1/messages") {
		req.Header.Set("anthropic-version", "2023-06-01")
	}
	if authenticated {
		req.Header.Set("Authorization", "Bearer "+r.apiKey)
		req.Header.Set("X-Api-Key", r.apiKey)
	}

	resp, err := r.client.Do(req)
	if err != nil {
		return apiResponse{total: time.Since(startedAt), err: err}
	}
	defer resp.Body.Close()
	result := apiResponse{
		statusCode: resp.StatusCode,
		headers:    time.Since(startedAt),
		requestID:  strings.TrimSpace(resp.Header.Get("X-Request-Id")),
	}
	if stream && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		result.stream, result.err = consumeSSEWithObserver(resp.Body, startedAt, onEvent)
		result.total = time.Since(startedAt)
		return result
	}
	result.body, result.err = readBounded(resp.Body, maxDiagnosticResponseBytes)
	result.total = time.Since(startedAt)
	return result
}

func readBounded(reader io.Reader, limit int64) ([]byte, error) {
	if reader == nil {
		return nil, fmt.Errorf("response reader is nil")
	}
	if limit < 0 {
		return nil, fmt.Errorf("response limit must not be negative: %d", limit)
	}
	if limit == int64(^uint64(0)>>1) {
		return nil, fmt.Errorf("response limit is too large")
	}
	data, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%w: response exceeded %d bytes", errResponseTooLarge, limit)
	}
	return data, nil
}

func consumeSSE(reader io.Reader, startedAt time.Time) (sseStats, error) {
	return consumeSSEWithObserver(reader, startedAt, nil)
}

func consumeSSEWithObserver(reader io.Reader, startedAt time.Time, onEvent func()) (sseStats, error) {
	stats := sseStats{tools: make(map[int]*sseToolState)}
	if reader == nil {
		return stats, fmt.Errorf("response reader is nil")
	}
	limited := &io.LimitedReader{R: reader, N: maxDiagnosticResponseBytes + 1}
	scanner := bufio.NewScanner(limited)
	scanner.Buffer(make([]byte, 64<<10), maxSSELineBytes)
	eventName := ""
	dataLines := make([]string, 0, 1)
	flush := func() error {
		if len(dataLines) == 0 {
			eventName = ""
			return nil
		}
		data := strings.Join(dataLines, "\n")
		hadSemanticOutput := stats.semanticOutput
		if err := stats.record(eventName, data, time.Since(startedAt)); err != nil {
			return err
		}
		if onEvent != nil && !hadSemanticOutput && stats.semanticOutput {
			onEvent()
		}
		eventName = ""
		dataLines = dataLines[:0]
		return nil
	}
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if line == "" {
			if err := flush(); err != nil {
				return stats, err
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			stats.noteHeartbeat(time.Since(startedAt))
			continue
		}
		if strings.HasPrefix(line, "event:") {
			eventName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}
		if strings.HasPrefix(line, "data:") {
			dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	if err := flush(); err != nil {
		return stats, err
	}
	if err := scanner.Err(); err != nil {
		if strings.Contains(err.Error(), "token too long") {
			return stats, fmt.Errorf("%w: SSE event line exceeded %d bytes", errResponseTooLarge, maxSSELineBytes)
		}
		return stats, err
	}
	if limited.N == 0 {
		return stats, fmt.Errorf("%w: stream response exceeded %d bytes", errResponseTooLarge, maxDiagnosticResponseBytes)
	}
	return stats, nil
}

func (s *sseStats) record(eventName, data string, elapsed time.Duration) error {
	if strings.TrimSpace(data) == "[DONE]" {
		s.noteEvent(elapsed)
		s.terminal = true
		return nil
	}
	var event map[string]interface{}
	if err := json.Unmarshal([]byte(data), &event); err != nil {
		return fmt.Errorf("SSE event %q contained invalid JSON: %w", eventName, err)
	}
	if event == nil {
		return fmt.Errorf("SSE event %q was not a JSON object", eventName)
	}
	s.usage.merge(extractTokenUsage(event))
	s.noteEvent(elapsed)
	eventType := stringField(event, "type")
	if eventType == "" {
		eventType = eventName
	}
	switch eventType {
	case "content_block_start":
		index := intField(event, "index")
		block := mapField(event, "content_block")
		switch stringField(block, "type") {
		case "tool_use":
			if _, exists := s.tools[index]; exists {
				return fmt.Errorf("tool content block %d started more than once", index)
			}
			name := stringField(block, "name")
			if name == "" {
				return fmt.Errorf("tool content block %d is missing a name", index)
			}
			tool := &sseToolState{id: stringField(block, "id"), name: name}
			if input, ok := block["input"]; ok {
				if encoded, err := json.Marshal(input); err == nil {
					tool.initialInput = string(encoded)
				}
			}
			s.tools[index] = tool
			s.toolCalls++
			s.markTool(elapsed)
		case "server_tool_use":
			s.serverToolCalls++
			s.markTool(elapsed)
		case "web_search_tool_result":
			s.webSearchResults++
			s.markSemantic(elapsed)
		}
	case "content_block_delta":
		index := intField(event, "index")
		delta := mapField(event, "delta")
		switch stringField(delta, "type") {
		case "text_delta":
			text := stringField(delta, "text")
			s.contentDeltas++
			s.contentChars += len([]rune(text))
			if text != "" {
				s.captureOutput(text)
				s.markText(elapsed)
			}
		case "thinking_delta":
			text := stringField(delta, "thinking")
			s.thinkingDeltas++
			s.thinkingChars += len([]rune(text))
			if text != "" {
				s.markThinking(elapsed)
			}
		case "input_json_delta":
			partial := stringField(delta, "partial_json")
			tool := s.tools[index]
			if tool == nil {
				return fmt.Errorf("tool content block %d received input before start", index)
			}
			if tool.stopped {
				return fmt.Errorf("tool content block %d received input after stop", index)
			}
			tool.deltas.WriteString(partial)
			if partial != "" {
				s.markTool(elapsed)
			}
		}
	case "content_block_stop":
		index := intField(event, "index")
		if tool := s.tools[index]; tool != nil {
			if tool.stopped {
				return fmt.Errorf("tool content block %d stopped more than once", index)
			}
			tool.stopped = true
		}
	case "message_stop", "response.completed", "response.done", "response.incomplete":
		if eventType == "response.incomplete" {
			s.incomplete = true
			response := mapField(event, "response")
			details := mapField(response, "incomplete_details")
			s.stopReason = firstNonEmpty(stringField(details, "reason"), "incomplete")
		}
		if err := s.markTerminal(eventType); err != nil {
			return err
		}
	case "message_delta":
		delta := mapField(event, "delta")
		if reason := strings.ToLower(stringField(delta, "stop_reason")); reason != "" {
			s.stopReason = reason
			if reason == "max_tokens" || reason == "length" {
				s.incomplete = true
			}
		}
	case "response.output_text.delta":
		text := stringField(event, "delta")
		s.contentDeltas++
		s.contentChars += len([]rune(text))
		if text != "" {
			s.captureOutput(text)
			s.markText(elapsed)
		}
	case "response.reasoning_summary_text.delta":
		text := stringField(event, "delta")
		s.thinkingDeltas++
		s.thinkingChars += len([]rune(text))
		if text != "" {
			s.markThinking(elapsed)
		}
	case "response.output_item.added":
		index := intField(event, "output_index")
		item := mapField(event, "item")
		kind := stringField(item, "type")
		if kind == "function_call" || kind == "custom_tool_call" {
			if _, exists := s.tools[index]; !exists {
				s.tools[index] = &sseToolState{id: firstNonEmpty(stringField(item, "call_id"), stringField(item, "id")), name: stringField(item, "name")}
				s.toolCalls++
			}
			s.markTool(elapsed)
		}
	case "response.function_call_arguments.delta", "response.custom_tool_call_input.delta":
		index := intField(event, "output_index")
		tool := s.tools[index]
		if tool == nil {
			return fmt.Errorf("response tool item %d received input before start", index)
		}
		partial := stringField(event, "delta")
		tool.deltas.WriteString(partial)
		if partial != "" {
			s.markTool(elapsed)
		}
	case "response.output_item.done":
		index := intField(event, "output_index")
		if tool := s.tools[index]; tool != nil {
			item := mapField(event, "item")
			if tool.name == "" {
				tool.name = stringField(item, "name")
			}
			if tool.id == "" {
				tool.id = firstNonEmpty(stringField(item, "call_id"), stringField(item, "id"))
			}
			if tool.deltas.Len() == 0 {
				tool.initialInput = firstNonEmpty(stringField(item, "arguments"), stringField(item, "input"))
			}
			tool.stopped = true
		}
	case "error", "response.failed":
		s.errorEvent = compactDetail(data)
		if eventType == "response.failed" {
			if err := s.markTerminal(eventType); err != nil {
				return err
			}
		}
	}
	if choices, ok := event["choices"].([]interface{}); ok && len(choices) > 0 {
		choice, _ := choices[0].(map[string]interface{})
		delta := mapField(choice, "delta")
		if content := stringField(delta, "content"); content != "" {
			s.contentDeltas++
			s.contentChars += len([]rune(content))
			s.captureOutput(content)
			s.markText(elapsed)
		}
		if reasoning := firstNonEmpty(stringField(delta, "reasoning_content"), stringField(delta, "reasoning")); reasoning != "" {
			s.thinkingDeltas++
			s.thinkingChars += len([]rune(reasoning))
			s.markThinking(elapsed)
		}
		if calls, ok := delta["tool_calls"].([]interface{}); ok {
			for _, rawCall := range calls {
				call, _ := rawCall.(map[string]interface{})
				index := intField(call, "index")
				tool := s.tools[index]
				if tool == nil {
					tool = &sseToolState{}
					s.tools[index] = tool
					s.toolCalls++
				}
				if id := stringField(call, "id"); id != "" {
					tool.id = id
				}
				function := mapField(call, "function")
				if name := stringField(function, "name"); name != "" {
					tool.name += name
				}
				if arguments := stringField(function, "arguments"); arguments != "" {
					tool.deltas.WriteString(arguments)
				}
				s.markTool(elapsed)
			}
		}
		if finish, exists := choice["finish_reason"]; exists && finish != nil {
			finishReason := fmt.Sprint(finish)
			s.stopReason = finishReason
			if strings.EqualFold(finishReason, "length") || strings.EqualFold(finishReason, "max_tokens") {
				s.incomplete = true
			}
			for _, tool := range s.tools {
				tool.stopped = true
			}
			if err := s.markTerminal("chat." + finishReason); err != nil {
				return err
			}
		}
	}
	return nil
}

func extractTokenUsage(value interface{}) tokenUsage {
	var result tokenUsage
	collectTokenUsage(value, &result)
	return result
}

func collectTokenUsage(value interface{}, result *tokenUsage) {
	switch typed := value.(type) {
	case map[string]interface{}:
		if rawUsage, ok := typed["usage"].(map[string]interface{}); ok {
			result.merge(tokenUsageFromMap(rawUsage))
		}
		for key, child := range typed {
			if key != "usage" {
				collectTokenUsage(child, result)
			}
		}
	case []interface{}:
		for _, child := range typed {
			collectTokenUsage(child, result)
		}
	}
}

func tokenUsageFromMap(usage map[string]interface{}) tokenUsage {
	result := tokenUsage{
		inputTokens:         max(intField(usage, "input_tokens"), intField(usage, "prompt_tokens")),
		outputTokens:        max(intField(usage, "output_tokens"), intField(usage, "completion_tokens")),
		cacheReadTokens:     intField(usage, "cache_read_input_tokens"),
		cacheCreationTokens: intField(usage, "cache_creation_input_tokens"),
	}
	for _, key := range []string{"input_tokens_details", "prompt_tokens_details"} {
		details := mapField(usage, key)
		result.cacheReadTokens = max(result.cacheReadTokens, intField(details, "cached_tokens"))
		result.cacheCreationTokens = max(result.cacheCreationTokens, intField(details, "cache_creation_tokens"))
	}
	for _, key := range []string{"output_tokens_details", "completion_tokens_details"} {
		result.reasoningTokens = max(result.reasoningTokens, intField(mapField(usage, key), "reasoning_tokens"))
	}
	return result
}

func (s *sseStats) noteEvent(elapsed time.Duration) {
	s.noteActivity(elapsed)
	if s.events == 0 {
		s.firstEvent = elapsed
	} else if gap := elapsed - s.lastEvent; gap > s.maxEventGap {
		s.maxEventGap = gap
	}
	s.lastEvent = elapsed
	s.events++
}

func (s *sseStats) noteHeartbeat(elapsed time.Duration) {
	s.noteActivity(elapsed)
	s.heartbeats++
}

func (s *sseStats) noteActivity(elapsed time.Duration) {
	if s.activities > 0 {
		if gap := elapsed - s.lastActivity; gap > s.maxActivityGap {
			s.maxActivityGap = gap
		}
	}
	s.lastActivity = elapsed
	s.activities++
}

func (s *sseStats) markTerminal(kind string) error {
	if s.terminalType != "" && s.terminalType != kind {
		return fmt.Errorf("conflicting terminal SSE events %q and %q", s.terminalType, kind)
	}
	s.terminal = true
	s.terminalType = kind
	return nil
}

func (s *sseStats) markSemantic(elapsed time.Duration) {
	if !s.semanticOutput {
		s.firstSemantic = elapsed
		s.semanticOutput = true
	}
}

func (s *sseStats) captureOutput(text string) {
	if text == "" || s.outputTruncated {
		return
	}
	remaining := maxCapturedOutputBytes - len(s.output)
	if remaining <= 0 {
		s.outputTruncated = true
		return
	}
	if len(text) > remaining {
		cut := remaining
		for cut > 0 && !utf8.ValidString(text[:cut]) {
			cut--
		}
		s.output = append(s.output, text[:cut]...)
		s.outputTruncated = true
		return
	}
	s.output = append(s.output, text...)
}

func (s *sseStats) outputText() string {
	if s == nil {
		return ""
	}
	return string(s.output)
}

func (s *sseStats) markText(elapsed time.Duration) {
	if !s.textOutput {
		s.firstText = elapsed
		s.textOutput = true
	}
	s.markSemantic(elapsed)
}

func (s *sseStats) markThinking(elapsed time.Duration) {
	if !s.thinkingOutput {
		s.firstThinking = elapsed
		s.thinkingOutput = true
	}
	s.markSemantic(elapsed)
}

func (s *sseStats) markTool(elapsed time.Duration) {
	if !s.toolOutput {
		s.firstTool = elapsed
		s.toolOutput = true
	}
	s.markSemantic(elapsed)
}

func (s sseStats) toolArguments(name string) (string, bool) {
	for _, tool := range s.tools {
		if tool.name != name {
			continue
		}
		if tool.deltas.Len() > 0 {
			return tool.deltas.String(), true
		}
		if tool.initialInput != "" {
			return tool.initialInput, true
		}
		return "", true
	}
	return "", false
}

func (s sseStats) toolCall(name string) (sseToolState, bool) {
	for _, tool := range s.tools {
		if tool != nil && tool.name == name {
			return *tool, true
		}
	}
	return sseToolState{}, false
}

func (s sseStats) hasSingleCompleteTool(name string) bool {
	if s.toolCalls != 1 {
		return false
	}
	for _, tool := range s.tools {
		if tool.name == name {
			return tool.stopped
		}
	}
	return false
}

func mapField(value map[string]interface{}, key string) map[string]interface{} {
	result, _ := value[key].(map[string]interface{})
	return result
}

func stringField(value map[string]interface{}, key string) string {
	result, _ := value[key].(string)
	return result
}

func intField(value map[string]interface{}, key string) int {
	switch number := value[key].(type) {
	case float64:
		return int(number)
	case int:
		return number
	case json.Number:
		parsed, _ := number.Int64()
		return int(parsed)
	default:
		return 0
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func validJSONResponse(response apiResponse) bool {
	if response.err != nil || response.statusCode < 200 || response.statusCode >= 300 {
		return false
	}
	var value interface{}
	return json.Unmarshal(response.body, &value) == nil
}

func responseErrorDetail(response apiResponse) string {
	if response.err != nil {
		return response.err.Error()
	}
	if len(response.body) == 0 {
		return fmt.Sprintf("HTTP %d with an empty body", response.statusCode)
	}
	return fmt.Sprintf("HTTP %d: %s", response.statusCode, compactDetail(string(response.body)))
}
