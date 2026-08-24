package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

func (r *runner) runSuite(ctx context.Context) {
	r.runHealth(ctx)
	r.runModels(ctx)
	if r.model == "" {
		r.add(scenarioResult{Name: "model-selection", Status: statusFail, Detail: "no Claude model was discovered; pass --model explicitly"})
		return
	}
	if r.thinking == "" {
		r.thinking = strings.TrimSuffix(r.model, "-thinking") + "-thinking"
	}

	switch r.opts.suite {
	case "load":
		r.runUnauthorized(ctx)
		r.runLoad(ctx)
		return
	case "smoke", "full":
		r.runUnauthorized(ctx)
		r.runClaudeNonStream(ctx)
		r.runClaudeStream(ctx)
	}
	if r.opts.suite != "full" {
		return
	}
	r.runThinkingStream(ctx)
	r.runSkillContext(ctx)
	r.runAnthropicFunction(ctx)
	r.runMCPZeroArgument(ctx)
	r.runOpenAIFunction(ctx)
	r.runResponses(ctx)
	if r.opts.webSearch {
		r.runWebSearch(ctx)
	} else {
		r.add(scenarioResult{Name: "native-web-search", Status: statusSkip, Protocol: "anthropic", Detail: "network-dependent; enable with --web-search"})
	}
	if r.opts.cancellation {
		r.runCancellation(ctx)
	}
}

func (r *runner) runHealth(parent context.Context) {
	ctx, cancel := r.scenarioContext(parent)
	defer cancel()
	response := r.get(ctx, "/health", false)
	result := scenarioResult{Name: "health", Protocol: "http", HTTPStatus: response.statusCode, TotalMillis: response.total.Milliseconds()}
	if !validJSONResponse(response) || !strings.Contains(string(response.body), "\"status\":\"ok\"") {
		result.Status = statusFail
		result.Detail = responseErrorDetail(response)
	} else {
		result.Status = statusPass
		result.Detail = "service is ready"
	}
	r.add(result)
}

func (r *runner) runModels(parent context.Context) {
	ctx, cancel := r.scenarioContext(parent)
	defer cancel()
	response := r.get(ctx, "/v1/models", true)
	result := scenarioResult{Name: "model-discovery", Protocol: "openai", HTTPStatus: response.statusCode, TotalMillis: response.total.Milliseconds()}
	if !validJSONResponse(response) {
		result.Status = statusFail
		result.Detail = responseErrorDetail(response)
		r.add(result)
		return
	}
	r.models = extractModelIDs(response.body)
	if len(r.models) == 0 {
		result.Status = statusFail
		result.Detail = "response did not contain model IDs"
		r.add(result)
		return
	}
	if r.model == "" {
		r.model = selectClaudeModel(r.models)
	}
	result.Status = statusPass
	result.Detail = fmt.Sprintf("discovered=%d selected=%s", len(r.models), r.model)
	r.add(result)
}

func (r *runner) runUnauthorized(parent context.Context) {
	ctx, cancel := r.scenarioContext(parent)
	defer cancel()
	payload := claudePayload(r.model, false, "Reply with OK.", 16)
	response := r.post(ctx, "/v1/messages", payload, false, false)
	result := scenarioResult{Name: "api-auth-rejection", Protocol: "security", HTTPStatus: response.statusCode, TotalMillis: response.total.Milliseconds()}
	if response.err != nil {
		result.Status = statusFail
		result.Detail = response.err.Error()
	} else if isExpectedAuthenticationRejection(response) {
		result.Status = statusPass
		result.Detail = "missing API key rejected by the local authentication layer"
	} else {
		result.Status = statusFail
		result.Detail = "unexpected authentication response: " + responseErrorDetail(response)
	}
	r.add(result)
}

func isExpectedAuthenticationRejection(response apiResponse) bool {
	if response.err != nil || response.statusCode != http.StatusUnauthorized {
		return false
	}
	var payload struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(response.body, &payload) != nil {
		return false
	}
	message := strings.ToLower(payload.Error.Message)
	return payload.Type == "error" && payload.Error.Type == "authentication_error" && strings.Contains(message, "api key")
}

func (r *runner) runClaudeNonStream(parent context.Context) {
	ctx, cancel := r.scenarioContext(parent)
	defer cancel()
	response := r.post(ctx, "/v1/messages", claudePayload(r.model, false, "Reply with exactly KIRO_DEV_OK.", 64), true, false)
	result := scenarioResult{Name: "claude-non-stream", Protocol: "anthropic", HTTPStatus: response.statusCode, TotalMillis: response.total.Milliseconds()}
	text := responseText(response.body)
	if !validJSONResponse(response) || strings.TrimSpace(text) == "" {
		result.Status = statusFail
		result.Detail = responseErrorDetail(response)
	} else {
		result.Status = statusPass
		result.Detail = fmt.Sprintf("text_chars=%d", len([]rune(text)))
	}
	r.add(result)
}

func (r *runner) runClaudeStream(parent context.Context) {
	ctx, cancel := r.scenarioContext(parent)
	defer cancel()
	prompt := "Write eight numbered lines. Each line must contain four different short words. Do not use tools."
	response := r.post(ctx, "/v1/messages", claudePayload(r.model, true, prompt, 256), true, true)
	result := streamScenarioResult("claude-live-stream", "anthropic", response)
	if result.Status == statusPass && response.stream.contentDeltas <= 1 && response.total > 300*time.Millisecond {
		result.Status = statusWarn
		result.Detail += "; output arrived in one content delta"
	} else if result.Status == statusPass && response.stream.firstSemantic > 0 && response.total-response.stream.firstSemantic < 20*time.Millisecond && response.total > 300*time.Millisecond {
		result.Status = statusWarn
		result.Detail += "; TTFT and total time indicate burst buffering"
	}
	r.add(result)
}

func (r *runner) runThinkingStream(parent context.Context) {
	ctx, cancel := r.scenarioContext(parent)
	defer cancel()
	prompt := "Calculate 137 * 43 carefully, then give the final number in one sentence."
	response := r.post(ctx, "/v1/messages", claudePayload(r.thinking, true, prompt, 768), true, true)
	result := streamScenarioResult("thinking-stream", "anthropic", response)
	if result.Status == statusPass && response.stream.thinkingDeltas == 0 {
		result.Status = statusWarn
		result.Detail += "; no thinking_delta observed (model or server setting may suppress reasoning)"
	}
	r.add(result)
}

func (r *runner) runSkillContext(parent context.Context) {
	ctx, cancel := r.scenarioContext(parent)
	defer cancel()
	payload := claudePayload(r.model, false, "Run the loaded skill probe.", 64)
	payload["system"] = "<skill name=\"devcheck\">For a skill probe, reply exactly SKILL_CONTEXT_OK.</skill>"
	response := r.post(ctx, "/v1/messages", payload, true, false)
	result := scenarioResult{Name: "skill-instruction-context", Protocol: "anthropic", HTTPStatus: response.statusCode, TotalMillis: response.total.Milliseconds()}
	text := responseText(response.body)
	switch {
	case !validJSONResponse(response) || strings.TrimSpace(text) == "":
		result.Status = statusFail
		result.Detail = responseErrorDetail(response)
	case strings.Contains(text, "SKILL_CONTEXT_OK"):
		result.Status = statusPass
		result.Detail = "system skill instructions survived translation"
	default:
		result.Status = statusWarn
		result.Detail = "response succeeded but did not follow the deterministic skill marker"
	}
	r.add(result)
}

func (r *runner) runAnthropicFunction(parent context.Context) {
	const name = "dev_echo"
	ctx, cancel := r.scenarioContext(parent)
	defer cancel()
	payload := claudePayload(r.model, true, "Call dev_echo once with value FUNCTION_OK. Do not answer in text.", 256)
	payload["tools"] = []interface{}{map[string]interface{}{
		"name": name, "description": "Return the supplied test value.",
		"input_schema": map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{"value": map[string]interface{}{"type": "string"}},
			"required":   []string{"value"},
		},
	}}
	payload["tool_choice"] = map[string]interface{}{"type": "tool", "name": name}
	response := r.post(ctx, "/v1/messages", payload, true, true)
	result := streamScenarioResult("anthropic-function-call", "anthropic", response)
	if result.Status == statusPass {
		arguments, found := response.stream.toolArguments(name)
		var input map[string]interface{}
		if !response.stream.hasSingleCompleteTool(name) || !found || json.Unmarshal([]byte(arguments), &input) != nil || input["value"] != "FUNCTION_OK" {
			result.Status = statusFail
			result.Detail += "; expected exactly one complete forced function call"
		}
	}
	r.add(result)
}

func (r *runner) runMCPZeroArgument(parent context.Context) {
	const name = "mcp__memory__read_graph"
	ctx, cancel := r.scenarioContext(parent)
	defer cancel()
	payload := claudePayload(r.model, true, "Call the memory graph tool once. Do not answer in text.", 256)
	payload["tools"] = []interface{}{map[string]interface{}{
		"name": name, "description": "Read the complete memory graph. This tool takes no arguments.",
		"input_schema": map[string]interface{}{
			"type": "object", "properties": map[string]interface{}{}, "additionalProperties": false,
		},
	}}
	payload["tool_choice"] = map[string]interface{}{"type": "tool", "name": name}
	response := r.post(ctx, "/v1/messages", payload, true, true)
	result := streamScenarioResult("mcp-zero-argument-tool", "anthropic", response)
	if result.Status == statusPass {
		arguments, found := response.stream.toolArguments(name)
		var input map[string]interface{}
		if !response.stream.hasSingleCompleteTool(name) || !found || json.Unmarshal([]byte(arguments), &input) != nil || len(input) != 0 {
			result.Status = statusFail
			result.Detail += "; expected one complete tool call with {} input"
		} else {
			result.Detail += "; empty input recovered safely"
		}
	}
	r.add(result)
}

func (r *runner) runOpenAIFunction(parent context.Context) {
	const name = "dev_sum"
	ctx, cancel := r.scenarioContext(parent)
	defer cancel()
	payload := map[string]interface{}{
		"model": r.model, "stream": false, "max_tokens": 256,
		"messages": []interface{}{map[string]interface{}{"role": "user", "content": "Call dev_sum with a=7 and b=9. Do not answer in text."}},
		"tools": []interface{}{map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name": name, "description": "Add two test integers.",
				"parameters": map[string]interface{}{
					"type":       "object",
					"properties": map[string]interface{}{"a": map[string]interface{}{"type": "integer"}, "b": map[string]interface{}{"type": "integer"}},
					"required":   []string{"a", "b"},
				},
			},
		}},
		"tool_choice": map[string]interface{}{"type": "function", "function": map[string]interface{}{"name": name}},
	}
	response := r.post(ctx, "/v1/chat/completions", payload, true, false)
	result := scenarioResult{Name: "openai-function-call", Protocol: "openai", HTTPStatus: response.statusCode, TotalMillis: response.total.Milliseconds()}
	arguments, found := openAIToolArguments(response.body, name)
	var input map[string]interface{}
	if !validJSONResponse(response) || !found || json.Unmarshal([]byte(arguments), &input) != nil {
		result.Status = statusFail
		result.Detail = responseErrorDetail(response)
	} else if number(input["a"]) != 7 || number(input["b"]) != 9 {
		result.Status = statusFail
		result.Detail = "function arguments were incomplete or incorrect"
	} else {
		result.Status = statusPass
		result.ToolCalls = 1
		result.Detail = "forced OpenAI function arguments are valid"
	}
	r.add(result)
}

func (r *runner) runResponses(parent context.Context) {
	ctx, cancel := r.scenarioContext(parent)
	defer cancel()
	payload := map[string]interface{}{
		"model": r.model, "input": "Reply with exactly RESPONSES_OK.", "max_output_tokens": 64, "store": false,
	}
	response := r.post(ctx, "/v1/responses", payload, true, false)
	result := scenarioResult{Name: "openai-responses", Protocol: "responses", HTTPStatus: response.statusCode, TotalMillis: response.total.Milliseconds()}
	if !validJSONResponse(response) || !strings.Contains(string(response.body), "RESPONSES_OK") {
		result.Status = statusFail
		result.Detail = responseErrorDetail(response)
	} else {
		result.Status = statusPass
		result.Detail = "response output item completed"
	}
	r.add(result)
}

func (r *runner) runWebSearch(parent context.Context) {
	ctx, cancel := r.scenarioContext(parent)
	defer cancel()
	payload := claudePayload(r.model, false, "Search the web for the official Kiro IDE homepage and summarize it briefly.", 512)
	payload["tools"] = []interface{}{map[string]interface{}{
		"type": "web_search_20250305", "name": "web_search", "max_uses": 1,
	}}
	response := r.post(ctx, "/v1/messages", payload, true, false)
	result := scenarioResult{Name: "native-web-search", Protocol: "anthropic", HTTPStatus: response.statusCode, TotalMillis: response.total.Milliseconds()}
	body := string(response.body)
	if !validJSONResponse(response) || !strings.Contains(body, "\"server_tool_use\"") || !strings.Contains(body, "\"web_search_tool_result\"") {
		result.Status = statusFail
		result.Detail = responseErrorDetail(response)
	} else {
		result.Status = statusPass
		result.Detail = "server tool use and linked search result observed"
	}
	r.add(result)
}

func (r *runner) runCancellation(parent context.Context) {
	timeoutCtx, timeoutCancel := r.scenarioContext(parent)
	defer timeoutCancel()
	ctx, cancelRequest := context.WithCancel(timeoutCtx)
	defer cancelRequest()
	prompt := "Write a detailed 2000-word technical essay with many numbered sections. Begin immediately."
	observedEvent := false
	var cancelOnce sync.Once
	response := r.postWithSSEObserver(ctx, "/v1/messages", claudePayload(r.model, true, prompt, 4096), true, true, func() {
		cancelOnce.Do(func() {
			observedEvent = true
			cancelRequest()
		})
	})
	result := scenarioResult{Name: "stream-cancellation-recovery", Protocol: "timeout", HTTPStatus: response.statusCode, TotalMillis: response.total.Milliseconds(), Events: response.stream.events}
	if !observedEvent {
		result.Status = statusFail
		result.Detail = "request ended before any valid SSE event was observed"
	} else if response.err != nil && (errors.Is(response.err, context.DeadlineExceeded) || errors.Is(response.err, context.Canceled)) {
		result.Status = statusPass
		result.Detail = fmt.Sprintf("client canceled the established stream after %d event(s)", response.stream.events)
	} else if response.err == nil && response.stream.terminal {
		result.Status = statusWarn
		result.Detail = "stream completed from buffered data before cancellation reached the transport"
	} else {
		result.Status = statusFail
		result.Detail = responseErrorDetail(response)
	}

	healthCtx, healthCancel := r.scenarioContext(parent)
	health := r.get(healthCtx, "/health", false)
	healthCancel()
	if !validJSONResponse(health) {
		result.Status = statusFail
		result.Detail += "; health check failed after cancellation"
	} else {
		result.Detail += "; service remained healthy"
	}
	r.add(result)
}

func (r *runner) runLoad(parent context.Context) {
	waves := (r.opts.requests + r.opts.concurrency - 1) / r.opts.concurrency
	ctx, cancel := context.WithTimeout(parent, time.Duration(waves)*r.opts.timeout)
	defer cancel()
	type sample struct {
		duration int64
		success  bool
		stream   bool
	}
	jobs := make(chan int)
	results := make(chan sample, r.opts.requests)
	var workers sync.WaitGroup
	for worker := 0; worker < r.opts.concurrency; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for index := range jobs {
				stream := index%2 == 1
				requestCtx, requestCancel := r.scenarioContext(ctx)
				payload := claudePayload(r.model, stream, fmt.Sprintf("Reply with OK %d.", index), 32)
				response := r.post(requestCtx, "/v1/messages", payload, true, stream)
				requestCancel()
				success := response.err == nil && response.statusCode >= 200 && response.statusCode < 300
				if stream {
					success = success && response.stream.terminal && response.stream.errorEvent == "" && response.stream.firstSemantic > 0
				} else {
					success = success && responseText(response.body) != ""
				}
				results <- sample{duration: response.total.Milliseconds(), success: success, stream: stream}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for index := 0; index < r.opts.requests; index++ {
			select {
			case jobs <- index:
			case <-ctx.Done():
				return
			}
		}
	}()
	workers.Wait()
	close(results)

	durations := make([]int64, 0, r.opts.requests)
	successes := 0
	streamRequests := 0
	streamSuccesses := 0
	nonStreamSuccesses := 0
	for sample := range results {
		durations = append(durations, sample.duration)
		if sample.stream {
			streamRequests++
		}
		if sample.success {
			successes++
			if sample.stream {
				streamSuccesses++
			} else {
				nonStreamSuccesses++
			}
		}
	}
	result := scenarioResult{Name: "concurrent-load", Protocol: "load"}
	if len(durations) > 0 {
		result.TotalMillis = percentile(durations, 0.95)
	}
	result.Detail = fmt.Sprintf(
		"success=%d/%d stream=%d/%d nonstream=%d/%d concurrency=%d p50=%dms p95=%dms",
		successes, r.opts.requests,
		streamSuccesses, streamRequests,
		nonStreamSuccesses, r.opts.requests-streamRequests,
		r.opts.concurrency, percentile(durations, 0.50), percentile(durations, 0.95),
	)
	if successes == r.opts.requests {
		result.Status = statusPass
	} else {
		result.Status = statusFail
	}
	r.add(result)
}

func streamScenarioResult(name, protocol string, response apiResponse) scenarioResult {
	result := scenarioResult{
		Name: name, Protocol: protocol, HTTPStatus: response.statusCode,
		TotalMillis: response.total.Milliseconds(), Events: response.stream.events,
		ContentDeltas: response.stream.contentDeltas, ThinkingDeltas: response.stream.thinkingDeltas,
		ToolCalls: response.stream.toolCalls,
	}
	if response.stream.firstSemantic > 0 {
		result.TTFTMillis = response.stream.firstSemantic.Milliseconds()
	}
	switch {
	case response.err != nil:
		result.Status = statusFail
		result.Detail = response.err.Error()
	case response.statusCode < 200 || response.statusCode >= 300:
		result.Status = statusFail
		result.Detail = responseErrorDetail(response)
	case response.stream.errorEvent != "":
		result.Status = statusFail
		result.Detail = "SSE error: " + response.stream.errorEvent
	case !response.stream.terminal:
		result.Status = statusFail
		result.Detail = "stream ended without a terminal event"
	case response.stream.firstSemantic == 0:
		result.Status = statusFail
		result.Detail = "stream contained no semantic output"
	default:
		result.Status = statusPass
		result.Detail = fmt.Sprintf("content=%d thinking=%d tools=%d", response.stream.contentChars, response.stream.thinkingChars, response.stream.toolCalls)
	}
	return result
}

func claudePayload(model string, stream bool, prompt string, maxTokens int) map[string]interface{} {
	return map[string]interface{}{
		"model": model, "stream": stream, "max_tokens": maxTokens,
		"messages": []interface{}{map[string]interface{}{"role": "user", "content": prompt}},
	}
}

func extractModelIDs(data []byte) []string {
	var response struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if json.Unmarshal(data, &response) != nil {
		return nil
	}
	seen := make(map[string]struct{})
	models := make([]string, 0, len(response.Data))
	for _, model := range response.Data {
		id := strings.TrimSpace(model.ID)
		if id == "" {
			continue
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		models = append(models, id)
	}
	sort.Strings(models)
	return models
}

func selectClaudeModel(models []string) string {
	for _, preferred := range []string{"claude-sonnet-5", "claude-sonnet-4-6", "claude-sonnet-4.6", "claude-opus-5", "claude-opus-4-8", "claude-opus-4.8"} {
		for _, model := range models {
			if strings.EqualFold(model, preferred) {
				return model
			}
		}
	}
	for _, model := range models {
		lower := strings.ToLower(model)
		if strings.HasPrefix(lower, "claude-") && !strings.HasSuffix(lower, "-thinking") {
			return model
		}
	}
	return ""
}

func responseText(data []byte) string {
	var value interface{}
	if json.Unmarshal(data, &value) != nil {
		return ""
	}
	var texts []string
	collectResponseText(value, &texts)
	return strings.Join(texts, "")
}

func collectResponseText(value interface{}, texts *[]string) {
	switch typed := value.(type) {
	case map[string]interface{}:
		if blockType, _ := typed["type"].(string); blockType == "text" || blockType == "output_text" {
			if text, _ := typed["text"].(string); text != "" {
				*texts = append(*texts, text)
			}
		}
		if message, ok := typed["message"].(map[string]interface{}); ok {
			if content, _ := message["content"].(string); content != "" {
				*texts = append(*texts, content)
			}
		}
		for key, child := range typed {
			if key == "input" || key == "arguments" || key == "system" {
				continue
			}
			collectResponseText(child, texts)
		}
	case []interface{}:
		for _, child := range typed {
			collectResponseText(child, texts)
		}
	}
}

func openAIToolArguments(data []byte, name string) (string, bool) {
	var response struct {
		Choices []struct {
			Message struct {
				ToolCalls []struct {
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(data, &response) != nil {
		return "", false
	}
	for _, choice := range response.Choices {
		for _, tool := range choice.Message.ToolCalls {
			if tool.Function.Name == name {
				return tool.Function.Arguments, true
			}
		}
	}
	return "", false
}

func number(value interface{}) int {
	switch typed := value.(type) {
	case float64:
		return int(typed)
	case json.Number:
		parsed, _ := typed.Int64()
		return int(parsed)
	case int:
		return typed
	default:
		return 0
	}
}
