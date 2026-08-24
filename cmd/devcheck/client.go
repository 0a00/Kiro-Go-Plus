package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const maxDiagnosticResponseBytes = 16 << 20

func newHTTPClient() *http.Client {
	return &http.Client{CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}}
}

type apiResponse struct {
	statusCode int
	body       []byte
	stream     sseStats
	total      time.Duration
	err        error
}

type sseToolState struct {
	name         string
	initialInput string
	deltas       strings.Builder
}

type sseStats struct {
	events         int
	contentDeltas  int
	thinkingDeltas int
	toolCalls      int
	contentChars   int
	thinkingChars  int
	firstSemantic  time.Duration
	terminal       bool
	errorEvent     string
	tools          map[int]*sseToolState
}

func (r *runner) get(ctx context.Context, path string, authenticated bool) apiResponse {
	return r.do(ctx, http.MethodGet, path, nil, authenticated, false)
}

func (r *runner) post(ctx context.Context, path string, payload interface{}, authenticated, stream bool) apiResponse {
	data, err := json.Marshal(payload)
	if err != nil {
		return apiResponse{err: err}
	}
	return r.do(ctx, http.MethodPost, path, data, authenticated, stream)
}

func (r *runner) do(ctx context.Context, method, path string, body []byte, authenticated, stream bool) apiResponse {
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
	result := apiResponse{statusCode: resp.StatusCode}
	if stream && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		result.stream, result.err = consumeSSE(resp.Body, startedAt)
		result.total = time.Since(startedAt)
		return result
	}
	result.body, result.err = readBounded(resp.Body, maxDiagnosticResponseBytes)
	result.total = time.Since(startedAt)
	return result
}

func readBounded(reader io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("response exceeded %d bytes", limit)
	}
	return data, nil
}

func consumeSSE(reader io.Reader, startedAt time.Time) (sseStats, error) {
	stats := sseStats{tools: make(map[int]*sseToolState)}
	scanner := bufio.NewScanner(io.LimitReader(reader, maxDiagnosticResponseBytes+1))
	scanner.Buffer(make([]byte, 64<<10), 4<<20)
	eventName := ""
	dataLines := make([]string, 0, 1)
	flush := func() {
		if len(dataLines) == 0 {
			eventName = ""
			return
		}
		data := strings.Join(dataLines, "\n")
		stats.record(eventName, data, time.Since(startedAt))
		eventName = ""
		dataLines = dataLines[:0]
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			flush()
			continue
		}
		if strings.HasPrefix(line, ":") {
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
	flush()
	if err := scanner.Err(); err != nil {
		return stats, err
	}
	return stats, nil
}

func (s *sseStats) record(eventName, data string, elapsed time.Duration) {
	if strings.TrimSpace(data) == "[DONE]" {
		s.events++
		s.terminal = true
		return
	}
	var event map[string]interface{}
	if err := json.Unmarshal([]byte(data), &event); err != nil {
		return
	}
	s.events++
	eventType := stringField(event, "type")
	if eventType == "" {
		eventType = eventName
	}
	switch eventType {
	case "content_block_start":
		index := intField(event, "index")
		block := mapField(event, "content_block")
		if stringField(block, "type") == "tool_use" {
			tool := &sseToolState{name: stringField(block, "name")}
			if input, ok := block["input"]; ok {
				if encoded, err := json.Marshal(input); err == nil {
					tool.initialInput = string(encoded)
				}
			}
			s.tools[index] = tool
			s.toolCalls++
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
				s.markSemantic(elapsed)
			}
		case "thinking_delta":
			text := stringField(delta, "thinking")
			s.thinkingDeltas++
			s.thinkingChars += len([]rune(text))
			if text != "" {
				s.markSemantic(elapsed)
			}
		case "input_json_delta":
			partial := stringField(delta, "partial_json")
			tool := s.tools[index]
			if tool == nil {
				tool = &sseToolState{}
				s.tools[index] = tool
			}
			tool.deltas.WriteString(partial)
			if partial != "" {
				s.markSemantic(elapsed)
			}
		}
	case "message_stop", "response.completed", "response.done":
		s.terminal = true
	case "error", "response.failed":
		s.errorEvent = compactDetail(data)
	}
	if choices, ok := event["choices"].([]interface{}); ok && len(choices) > 0 {
		choice, _ := choices[0].(map[string]interface{})
		delta := mapField(choice, "delta")
		if content := stringField(delta, "content"); content != "" {
			s.contentDeltas++
			s.contentChars += len([]rune(content))
			s.markSemantic(elapsed)
		}
		if finish, exists := choice["finish_reason"]; exists && finish != nil {
			s.terminal = true
		}
	}
}

func (s *sseStats) markSemantic(elapsed time.Duration) {
	if s.firstSemantic == 0 {
		s.firstSemantic = elapsed
	}
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
