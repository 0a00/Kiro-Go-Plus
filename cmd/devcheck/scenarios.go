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
	r.resolveSelectedModels()

	switch r.opts.suite {
	case "load":
		r.runUnauthorized(ctx)
		r.runLoad(ctx)
		return
	case "staircase":
		r.runUnauthorized(ctx)
		r.runStaircase(ctx)
		return
	case "soak":
		r.runUnauthorized(ctx)
		r.runSoak(ctx)
		return
	case "matrix":
		r.runUnauthorized(ctx)
		r.runProtocolMatrix(ctx)
		return
	case "smoke":
		r.runUnauthorized(ctx)
		r.runClaudeNonStream(ctx)
		r.runClaudeStream(ctx)
		return
	case "full":
		r.runUnauthorized(ctx)
	}
	r.runIf("anthropic-non-stream", ctx, r.runClaudeNonStream)
	r.runIf("anthropic-stream", ctx, r.runClaudeStream)
	r.runIf("thinking-stream", ctx, r.runThinkingStream)
	r.runIf("thinking-protocols", ctx, r.runThinkingProtocols)
	r.runIf("skill-context", ctx, r.runSkillContext)
	r.runIf("anthropic-tool-roundtrip", ctx, r.runAnthropicFunction)
	r.runIf("mcp-roundtrip", ctx, r.runMCPZeroArgument)
	r.runIf("chat-tool-roundtrip", ctx, r.runOpenAIFunction)
	r.runIf("responses-tool-roundtrip", ctx, r.runResponsesFunction)
	r.runIf("responses-custom-tool", ctx, r.runResponsesCustomTool)
	r.runIf("chat-stream", ctx, r.runOpenAIStream)
	r.runIf("responses-non-stream", ctx, r.runResponses)
	r.runIf("responses-stream", ctx, r.runResponsesStream)
	r.runIf("cache-reuse", ctx, r.runPromptCacheReuse)
	r.runIf("multimodal-accounting", ctx, r.runMultimodalAccounting)
	r.runIf("output-limit", ctx, r.runOutputLimit)
	r.runIf("long-stream", ctx, r.runLongStream)
	if len(r.opts.scenarioFilter) > 0 && r.scenarioEnabled("protocol-matrix") {
		r.runProtocolMatrix(ctx)
	}
	if r.opts.webSearch {
		r.runIf("websearch-non-stream", ctx, r.runWebSearch)
		r.runIf("websearch-stream", ctx, r.runWebSearchStream)
		r.runIf("websearch-multi", ctx, r.runWebSearchMulti)
		r.runIf("websearch-mixed-tools", ctx, r.runWebSearchMixedTools)
	} else {
		for _, name := range []string{"websearch-non-stream", "websearch-stream", "websearch-multi", "websearch-mixed-tools"} {
			if r.scenarioEnabled(name) {
				r.add(scenarioResult{Name: name, Status: statusSkip, Protocol: "anthropic", Detail: "network-dependent; enable with --web-search"})
			}
		}
	}
	if r.opts.cancellation && r.scenarioEnabled("cancellation") {
		r.runCancellation(ctx)
	}
}

func (r *runner) runIf(name string, ctx context.Context, runScenario func(context.Context)) {
	if r.scenarioEnabled(name) {
		runScenario(ctx)
	}
}

func (r *runner) scenarioEnabled(name string) bool {
	if len(r.opts.scenarioFilter) == 0 {
		return true
	}
	_, enabled := r.opts.scenarioFilter[name]
	return enabled
}

func (r *runner) resolveSelectedModels() {
	switch {
	case r.opts.allModels:
		for _, model := range r.models {
			if strings.HasPrefix(strings.ToLower(model), "claude-") {
				r.selected = append(r.selected, model)
			}
		}
	case len(r.opts.models) > 0:
		r.selected = append(r.selected, r.opts.models...)
	default:
		r.selected = append(r.selected, r.model)
		if r.opts.suite == "matrix" && !strings.EqualFold(r.thinking, r.model) {
			r.selected = append(r.selected, r.thinking)
		}
	}
}

func (r *runner) runHealth(parent context.Context) {
	ctx, cancel := r.scenarioContext(parent)
	defer cancel()
	response := r.get(ctx, "/health", false)
	result := responseScenarioResult("health", "http", "", response, false)
	var health struct {
		Status  string `json:"status"`
		Version string `json:"version"`
	}
	if !validJSONResponse(response) || json.Unmarshal(response.body, &health) != nil || health.Status != "ok" {
		result.Status = statusFail
		result.Detail = responseErrorDetail(response)
	} else {
		result.Status = statusPass
		r.serverVersion = strings.TrimSpace(health.Version)
		result.Detail = "service is ready"
		if r.serverVersion != "" {
			result.Detail += "; version=" + r.serverVersion
		}
	}
	r.add(result)
}

func (r *runner) runModels(parent context.Context) {
	ctx, cancel := r.scenarioContext(parent)
	defer cancel()
	response := r.get(ctx, "/v1/models", true)
	result := responseScenarioResult("model-discovery", "openai", "", response, false)
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
	result := responseScenarioResult("api-auth-rejection", "security", r.model, response, false)
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
	result := responseScenarioResult("claude-non-stream", "anthropic", r.model, response, false)
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
	result := streamScenarioResult("claude-live-stream", "anthropic", r.model, response)
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
	result := streamScenarioResult("thinking-stream", "anthropic", r.thinking, response)
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
	result := responseScenarioResult("skill-instruction-context", "anthropic", r.model, response, false)
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
	result := streamScenarioResult("anthropic-function-call", "anthropic", r.model, response)
	var input map[string]interface{}
	var call sseToolState
	if result.Status == statusPass {
		var found bool
		call, found = response.stream.toolCall(name)
		arguments, hasArguments := response.stream.toolArguments(name)
		if !response.stream.hasSingleCompleteTool(name) || !found || call.id == "" || !hasArguments || json.Unmarshal([]byte(arguments), &input) != nil || input["value"] != "FUNCTION_OK" {
			result.Status = statusFail
			result.Detail += "; expected exactly one complete forced function call with an ID"
		}
	}
	if result.Status == statusPass {
		continuation := claudePayload(r.model, false, "", 64)
		continuation["messages"] = anthropicToolRoundTripMessages(
			"Call dev_echo once with value FUNCTION_OK. Do not answer in text.", call.id, name, input, "FUNCTION_RESULT_OK",
		)
		followCtx, followCancel := r.scenarioContext(parent)
		follow := r.post(followCtx, "/v1/messages", continuation, true, false)
		followCancel()
		applyRoundTripResult(&result, follow, "FUNCTION_RESULT_OK")
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
	result := streamScenarioResult("mcp-zero-argument-tool", "anthropic", r.model, response)
	var call sseToolState
	if result.Status == statusPass {
		arguments, found := response.stream.toolArguments(name)
		var input map[string]interface{}
		call, _ = response.stream.toolCall(name)
		if !response.stream.hasSingleCompleteTool(name) || !found || call.id == "" || json.Unmarshal([]byte(arguments), &input) != nil || len(input) != 0 {
			result.Status = statusFail
			result.Detail += "; expected one complete tool call with an ID and {} input"
		} else {
			result.Detail += "; empty input recovered safely"
		}
	}
	if result.Status == statusPass {
		continuation := claudePayload(r.model, false, "", 64)
		continuation["messages"] = anthropicToolRoundTripMessages(
			"Call the memory graph tool once. Do not answer in text.", call.id, name, map[string]interface{}{}, "MCP_RESULT_OK",
		)
		followCtx, followCancel := r.scenarioContext(parent)
		follow := r.post(followCtx, "/v1/messages", continuation, true, false)
		followCancel()
		applyRoundTripResult(&result, follow, "MCP_RESULT_OK")
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
	result := responseScenarioResult("openai-function-call", "openai", r.model, response, false)
	callID, arguments, found := openAIToolCall(response.body, name)
	var input map[string]interface{}
	if !validJSONResponse(response) || !found || callID == "" || json.Unmarshal([]byte(arguments), &input) != nil {
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
	if result.Status == statusPass {
		followPayload := openAIChatPayload(r.model, false, "", 64)
		followPayload["messages"] = []interface{}{
			map[string]interface{}{"role": "user", "content": "Call dev_sum with a=7 and b=9. Do not answer in text."},
			map[string]interface{}{
				"role": "assistant", "content": nil,
				"tool_calls": []interface{}{map[string]interface{}{
					"id": callID, "type": "function", "function": map[string]interface{}{"name": name, "arguments": arguments},
				}},
			},
			map[string]interface{}{"role": "tool", "tool_call_id": callID, "content": "OPENAI_RESULT_OK"},
			map[string]interface{}{"role": "user", "content": "Reply with exactly OPENAI_RESULT_OK."},
		}
		followCtx, followCancel := r.scenarioContext(parent)
		follow := r.post(followCtx, "/v1/chat/completions", followPayload, true, false)
		followCancel()
		applyRoundTripResult(&result, follow, "OPENAI_RESULT_OK")
	}
	r.add(result)
}

func (r *runner) runOpenAIStream(parent context.Context) {
	ctx, cancel := r.scenarioContext(parent)
	defer cancel()
	payload := openAIChatPayload(r.model, true, "Write six short numbered lines without using tools.", 192)
	response := r.post(ctx, "/v1/chat/completions", payload, true, true)
	result := streamScenarioResult("openai-chat-stream", "openai", r.model, response)
	if result.Status == statusPass && response.stream.contentDeltas <= 1 && response.total > 300*time.Millisecond {
		result.Status = statusWarn
		result.Detail += "; output arrived in one content delta"
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
	result := responseScenarioResult("openai-responses", "responses", r.model, response, false)
	if !validJSONResponse(response) || !strings.Contains(string(response.body), "RESPONSES_OK") {
		result.Status = statusFail
		result.Detail = responseErrorDetail(response)
	} else {
		result.Status = statusPass
		result.Detail = "response output item completed"
	}
	r.add(result)
}

func (r *runner) runResponsesStream(parent context.Context) {
	ctx, cancel := r.scenarioContext(parent)
	defer cancel()
	payload := responsesPayload(r.model, true, "Write six short numbered lines without using tools.", 192)
	response := r.post(ctx, "/v1/responses", payload, true, true)
	result := streamScenarioResult("openai-responses-stream", "responses", r.model, response)
	if result.Status == statusPass && response.stream.contentDeltas <= 1 && response.total > 300*time.Millisecond {
		result.Status = statusWarn
		result.Detail += "; output arrived in one content delta"
	}
	r.add(result)
}

func (r *runner) runProtocolMatrix(parent context.Context) {
	if len(r.selected) == 0 {
		r.add(scenarioResult{Name: "protocol-matrix", Status: statusFail, Protocol: "matrix", Detail: "no Claude models selected"})
		return
	}
	type matrixCase struct {
		name     string
		protocol string
		path     string
		stream   bool
		payload  func(string, bool, string, int) map[string]interface{}
	}
	cases := []matrixCase{
		{name: "matrix-anthropic-non-stream", protocol: "anthropic", path: "/v1/messages", payload: claudePayload},
		{name: "matrix-anthropic-stream", protocol: "anthropic", path: "/v1/messages", stream: true, payload: claudePayload},
		{name: "matrix-chat-non-stream", protocol: "openai", path: "/v1/chat/completions", payload: openAIChatPayload},
		{name: "matrix-chat-stream", protocol: "openai", path: "/v1/chat/completions", stream: true, payload: openAIChatPayload},
		{name: "matrix-responses-non-stream", protocol: "responses", path: "/v1/responses", payload: responsesPayload},
		{name: "matrix-responses-stream", protocol: "responses", path: "/v1/responses", stream: true, payload: responsesPayload},
	}
	for _, model := range r.selected {
		for _, matrix := range cases {
			if err := parent.Err(); err != nil {
				r.add(scenarioResult{Name: matrix.name, Model: model, Protocol: matrix.protocol, Stream: matrix.stream, Status: statusSkip, Detail: err.Error()})
				continue
			}
			ctx, cancel := r.scenarioContext(parent)
			maxTokens := 64
			if strings.HasSuffix(strings.ToLower(model), "-thinking") {
				maxTokens = 1024
			}
			response := r.post(ctx, matrix.path, matrix.payload(model, matrix.stream, "Reply with exactly MATRIX_OK.", maxTokens), true, matrix.stream)
			cancel()
			if matrix.stream {
				r.add(streamScenarioResult(matrix.name, matrix.protocol, model, response))
				continue
			}
			result := responseScenarioResult(matrix.name, matrix.protocol, model, response, false)
			if !validJSONResponse(response) || strings.TrimSpace(responseText(response.body)) == "" {
				result.Status = statusFail
				result.Detail = responseErrorDetail(response)
			} else {
				result.Status = statusPass
				result.Detail = fmt.Sprintf("text_chars=%d", len([]rune(responseText(response.body))))
			}
			r.add(result)
		}
	}
}

func (r *runner) runWebSearch(parent context.Context) {
	ctx, cancel := r.scenarioContext(parent)
	defer cancel()
	payload := claudePayload(r.model, false, "Search the web for the official Kiro IDE homepage and summarize it briefly.", 512)
	payload["tools"] = []interface{}{map[string]interface{}{
		"type": "web_search_20250305", "name": "web_search", "max_uses": 1,
	}}
	response := r.post(ctx, "/v1/messages", payload, true, false)
	result := responseScenarioResult("native-web-search", "anthropic", r.model, response, false)
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

func (r *runner) runWebSearchStream(parent context.Context) {
	ctx, cancel := r.scenarioContext(parent)
	defer cancel()
	payload := claudePayload(r.model, true, "Search the web for the official Kiro IDE homepage and summarize it in one sentence.", 512)
	payload["tools"] = []interface{}{webSearchTool(1)}
	response := r.post(ctx, "/v1/messages", payload, true, true)
	result := streamScenarioResult("native-web-search-stream", "anthropic", r.model, response)
	if result.Status == statusPass && (response.stream.serverToolCalls == 0 || response.stream.webSearchResults == 0) {
		result.Status = statusFail
		result.Detail += "; linked server_tool_use and web_search_tool_result were not both observed"
	} else if result.Status == statusPass {
		result.Detail += fmt.Sprintf("; server_tools=%d search_results=%d", response.stream.serverToolCalls, response.stream.webSearchResults)
	}
	r.add(result)
}

func (r *runner) runWebSearchMulti(parent context.Context) {
	ctx, cancel := r.scenarioContext(parent)
	defer cancel()
	prompt := "Search separately for the official Kiro IDE homepage and the official AWS Kiro documentation. Compare the two sources briefly."
	payload := claudePayload(r.model, false, prompt, 768)
	payload["tools"] = []interface{}{webSearchTool(2)}
	response := r.post(ctx, "/v1/messages", payload, true, false)
	result := responseScenarioResult("native-web-search-multi", "anthropic", r.model, response, false)
	uses := strings.Count(string(response.body), `"type":"server_tool_use"`)
	results := strings.Count(string(response.body), `"type":"web_search_tool_result"`)
	switch {
	case !validJSONResponse(response) || uses == 0 || results == 0:
		result.Status = statusFail
		result.Detail = responseErrorDetail(response)
	case uses < 2 || results < 2:
		result.Status = statusWarn
		result.Detail = fmt.Sprintf("request succeeded but model used only %d search round(s)", min(uses, results))
	default:
		result.Status = statusPass
		result.Detail = fmt.Sprintf("search_uses=%d search_results=%d", uses, results)
	}
	r.add(result)
}

func (r *runner) runWebSearchMixedTools(parent context.Context) {
	ctx, cancel := r.scenarioContext(parent)
	defer cancel()
	payload := claudePayload(r.model, false, "Use web search to find the official Kiro IDE homepage, then summarize it. Do not call dev_note.", 512)
	payload["tools"] = []interface{}{
		webSearchTool(1),
		map[string]interface{}{
			"name": "dev_note", "description": "Store a development note only when explicitly requested.",
			"input_schema": map[string]interface{}{
				"type": "object", "properties": map[string]interface{}{"text": map[string]interface{}{"type": "string"}}, "required": []string{"text"},
			},
		},
	}
	response := r.post(ctx, "/v1/messages", payload, true, false)
	result := responseScenarioResult("native-web-search-mixed-tools", "anthropic", r.model, response, false)
	body := string(response.body)
	if !validJSONResponse(response) || !strings.Contains(body, `"type":"server_tool_use"`) || !strings.Contains(body, `"type":"web_search_tool_result"`) {
		result.Status = statusFail
		result.Detail = responseErrorDetail(response)
	} else {
		result.Status = statusPass
		result.Detail = "native search remained functional beside a client tool schema"
	}
	r.add(result)
}

func webSearchTool(maxUses int) map[string]interface{} {
	return map[string]interface{}{"type": "web_search_20250305", "name": "web_search", "max_uses": maxUses}
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
	result := streamScenarioResult("stream-cancellation-recovery", "timeout", r.model, response)
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
	samples := r.executeLoad(parent, r.opts.concurrency, r.opts.requests, 32)
	r.add(buildLoadResult("concurrent-load", r.model, r.opts.concurrency, r.opts.requests, samples))
}

func (r *runner) runStaircase(parent context.Context) {
	for _, concurrency := range r.opts.concurrencySteps {
		requests := max(r.opts.requests, concurrency)
		samples := r.executeLoad(parent, concurrency, requests, 32)
		result := buildLoadResult(fmt.Sprintf("concurrency-staircase-%d", concurrency), r.model, concurrency, requests, samples)
		result.Detail += "; level requests are max(--requests, concurrency)"
		r.add(result)
		if parent.Err() != nil {
			return
		}
	}
}

func (r *runner) runSoak(parent context.Context) {
	const maxTokensPerRequest = 32
	target := min(r.opts.soakMaxRequests, r.opts.soakTokenBudget/maxTokensPerRequest)
	samples, scheduled, durationReached := r.executeSoak(parent, r.opts.concurrency, target, maxTokensPerRequest, r.opts.soakDuration)
	result := buildLoadResult("bounded-soak", r.model, r.opts.concurrency, scheduled, samples)
	stopReason := "request/token cap"
	if durationReached {
		stopReason = "duration cap"
	}
	if parent.Err() != nil {
		stopReason = "parent cancellation"
	}
	if scheduled == 0 {
		result.Status = statusFail
		result.Detail = "no soak requests were scheduled"
	}
	result.Detail += fmt.Sprintf("; stop=%s duration_cap=%s request_cap=%d token_budget=%d reserved_tokens=%d",
		stopReason, r.opts.soakDuration, r.opts.soakMaxRequests, r.opts.soakTokenBudget, scheduled*maxTokensPerRequest)
	r.add(result)
}

type loadSample struct {
	duration  int64
	ttft      int64
	stream    bool
	protocol  string
	requestID string
	success   bool
	category  string
}

func (r *runner) executeLoad(parent context.Context, concurrency, requests, maxTokens int) []loadSample {
	waves := (requests + concurrency - 1) / concurrency
	suiteTimeout := time.Duration(waves) * r.opts.timeout
	ctx, cancel := context.WithTimeout(parent, suiteTimeout)
	defer cancel()
	jobs := make(chan int)
	results := make(chan loadSample, requests)
	var workers sync.WaitGroup
	for worker := 0; worker < concurrency; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for index := range jobs {
				probe := buildLoadProbe(r.model, index, maxTokens)
				requestCtx, requestCancel := r.scenarioContext(ctx)
				response := r.post(requestCtx, probe.path, probe.payload, true, probe.stream)
				requestCancel()
				results <- classifyLoadSample(response, probe.stream, probe.protocol)
			}
		}()
	}
	go func() {
		defer close(jobs)
		for index := 0; index < requests; index++ {
			select {
			case jobs <- index:
			case <-ctx.Done():
				return
			}
		}
	}()
	workers.Wait()
	close(results)
	samples := make([]loadSample, 0, requests)
	for sample := range results {
		samples = append(samples, sample)
	}
	return samples
}

func (r *runner) executeSoak(parent context.Context, concurrency, target, maxTokens int, duration time.Duration) ([]loadSample, int, bool) {
	scheduleCtx, stopScheduling := context.WithTimeout(parent, duration)
	defer stopScheduling()
	jobs := make(chan int)
	results := make(chan loadSample, target)
	var workers sync.WaitGroup
	for worker := 0; worker < concurrency; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for index := range jobs {
				probe := buildLoadProbe(r.model, index, maxTokens)
				requestCtx, requestCancel := r.scenarioContext(parent)
				response := r.post(requestCtx, probe.path, probe.payload, true, probe.stream)
				requestCancel()
				results <- classifyLoadSample(response, probe.stream, probe.protocol)
			}
		}()
	}
	type scheduleResult struct {
		count           int
		durationReached bool
	}
	scheduledResult := make(chan scheduleResult, 1)
	go func() {
		defer close(jobs)
		scheduled := 0
		for index := 0; index < target; index++ {
			select {
			case jobs <- index:
				scheduled++
			case <-scheduleCtx.Done():
				scheduledResult <- scheduleResult{count: scheduled, durationReached: errors.Is(scheduleCtx.Err(), context.DeadlineExceeded)}
				return
			}
		}
		scheduledResult <- scheduleResult{count: scheduled}
	}()
	workers.Wait()
	close(results)
	schedule := <-scheduledResult
	samples := make([]loadSample, 0, schedule.count)
	for sample := range results {
		samples = append(samples, sample)
	}
	return samples, schedule.count, schedule.durationReached
}

func classifyLoadSample(response apiResponse, stream bool, protocols ...string) loadSample {
	protocol := "anthropic"
	if len(protocols) > 0 && protocols[0] != "" {
		protocol = protocols[0]
	}
	sample := loadSample{duration: response.total.Milliseconds(), stream: stream, protocol: protocol, requestID: response.requestID}
	if stream {
		sample.ttft = response.stream.firstSemantic.Milliseconds()
	}
	switch {
	case errors.Is(response.err, context.DeadlineExceeded):
		sample.category = "timeout"
	case errors.Is(response.err, context.Canceled):
		sample.category = "canceled"
	case response.err != nil:
		sample.category = "transport"
	case response.statusCode == http.StatusTooManyRequests:
		sample.category = "http_429"
	case response.statusCode >= 500:
		sample.category = "http_5xx"
	case response.statusCode < 200 || response.statusCode >= 300:
		sample.category = "http_other"
	case stream && response.stream.errorEvent != "":
		sample.category = "sse_error"
	case stream && (!response.stream.terminal || !response.stream.semanticOutput):
		sample.category = "stream_protocol"
	case !stream && responseText(response.body) == "":
		sample.category = "empty_response"
	default:
		sample.success = true
	}
	return sample
}

func buildLoadResult(name, model string, concurrency, expected int, samples []loadSample) scenarioResult {
	durations := make([]int64, 0, len(samples))
	ttfts := make([]int64, 0, len(samples))
	successes := 0
	streamRequests := 0
	streamSuccesses := 0
	nonStreamSuccesses := 0
	failures := make(map[string]int)
	protocolSuccesses := make(map[string]int)
	requestIDs := make(map[string]struct{})
	for _, sample := range samples {
		durations = append(durations, sample.duration)
		if sample.ttft > 0 {
			ttfts = append(ttfts, sample.ttft)
		}
		if sample.stream {
			streamRequests++
		}
		if sample.success {
			successes++
			protocolSuccesses[sample.protocol]++
			if sample.stream {
				streamSuccesses++
			} else {
				nonStreamSuccesses++
			}
		} else {
			category := sample.category
			if sample.protocol != "" {
				category = sample.protocol + "_" + category
			}
			failures[category]++
		}
		if sample.requestID != "" {
			requestIDs[sample.requestID] = struct{}{}
		}
	}
	if missing := expected - len(samples); missing > 0 {
		failures["not_started_or_canceled"] += missing
	}
	result := scenarioResult{
		Name: name, Protocol: "load", Model: model, Requests: expected, Successes: successes,
		DistinctRequestIDs: len(requestIDs),
		FailureCategories:  failures, P50Millis: percentile(durations, 0.50), P95Millis: percentile(durations, 0.95), P99Millis: percentile(durations, 0.99),
	}
	result.TotalMillis = result.P95Millis
	result.Detail = fmt.Sprintf(
		"success=%d/%d completed=%d stream=%d/%d nonstream=%d/%d concurrency=%d p50=%dms p95=%dms p99=%dms ttft_p95=%dms failures=%s",
		successes, expected, len(samples),
		streamSuccesses, streamRequests,
		nonStreamSuccesses, len(samples)-streamRequests,
		concurrency, result.P50Millis, result.P95Millis, result.P99Millis, percentile(ttfts, 0.95), formatFailureCategories(failures),
	)
	result.Detail += "; protocols=" + formatFailureCategories(protocolSuccesses)
	if successes == expected {
		result.Status = statusPass
	} else {
		result.Status = statusFail
	}
	if len(failures) == 0 {
		result.FailureCategories = nil
	}
	return result
}

func formatFailureCategories(categories map[string]int) string {
	if len(categories) == 0 {
		return "none"
	}
	names := make([]string, 0, len(categories))
	for name := range categories {
		names = append(names, name)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, name := range names {
		parts = append(parts, fmt.Sprintf("%s:%d", name, categories[name]))
	}
	return strings.Join(parts, ",")
}

func streamScenarioResult(name, protocol, model string, response apiResponse) scenarioResult {
	result := responseScenarioResult(name, protocol, model, response, true)
	result.Events = response.stream.events
	result.Heartbeats = response.stream.heartbeats
	result.ContentDeltas = response.stream.contentDeltas
	result.ThinkingDeltas = response.stream.thinkingDeltas
	result.ToolCalls = response.stream.toolCalls
	result.StopReason = response.stream.stopReason
	result.InputTokens = response.stream.usage.inputTokens
	result.OutputTokens = response.stream.usage.outputTokens
	result.ReasoningTokens = response.stream.usage.reasoningTokens
	result.CacheReadTokens = response.stream.usage.cacheReadTokens
	result.CacheCreateTokens = response.stream.usage.cacheCreationTokens
	result.FirstEventMillis = response.stream.firstEvent.Milliseconds()
	result.TTFTMillis = response.stream.firstSemantic.Milliseconds()
	result.FirstTextMillis = response.stream.firstText.Milliseconds()
	result.FirstThinkMillis = response.stream.firstThinking.Milliseconds()
	result.FirstToolMillis = response.stream.firstTool.Milliseconds()
	result.MaxStreamGapMS = response.stream.maxEventGap.Milliseconds()
	result.MaxWireGapMS = response.stream.maxActivityGap.Milliseconds()
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
	case !response.stream.semanticOutput:
		result.Status = statusFail
		result.Detail = "stream contained no semantic output"
	default:
		result.Status = statusPass
		result.Detail = fmt.Sprintf("content=%d thinking=%d tools=%d", response.stream.contentChars, response.stream.thinkingChars, response.stream.toolCalls)
	}
	if result.Status == statusPass && response.stream.incomplete {
		result.Status = statusWarn
		result.Detail += "; output limit reached before a complete response"
	}
	return result
}

func responseScenarioResult(name, protocol, model string, response apiResponse, stream bool) scenarioResult {
	usage := extractTokenUsageFromBody(response.body)
	result := scenarioResult{
		Name: name, Protocol: protocol, Model: model, Stream: stream,
		HTTPStatus: response.statusCode, RequestID: response.requestID,
		ResponseHeaderMS: response.headers.Milliseconds(), TotalMillis: response.total.Milliseconds(),
		InputTokens: usage.inputTokens, OutputTokens: usage.outputTokens, ReasoningTokens: usage.reasoningTokens,
		CacheReadTokens: usage.cacheReadTokens, CacheCreateTokens: usage.cacheCreationTokens,
	}
	if response.requestID != "" {
		result.RequestIDs = []string{response.requestID}
	}
	return result
}

func extractTokenUsageFromBody(body []byte) tokenUsage {
	var value interface{}
	if len(body) == 0 || json.Unmarshal(body, &value) != nil {
		return tokenUsage{}
	}
	return extractTokenUsage(value)
}

func claudePayload(model string, stream bool, prompt string, maxTokens int) map[string]interface{} {
	return map[string]interface{}{
		"model": model, "stream": stream, "max_tokens": maxTokens,
		"messages": []interface{}{map[string]interface{}{"role": "user", "content": prompt}},
	}
}

func openAIChatPayload(model string, stream bool, prompt string, maxTokens int) map[string]interface{} {
	return map[string]interface{}{
		"model": model, "stream": stream, "max_tokens": maxTokens,
		"messages": []interface{}{map[string]interface{}{"role": "user", "content": prompt}},
	}
}

func responsesPayload(model string, stream bool, prompt string, maxTokens int) map[string]interface{} {
	return map[string]interface{}{
		"model": model, "stream": stream, "input": prompt, "max_output_tokens": maxTokens, "store": false,
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

func anthropicToolRoundTripMessages(prompt, callID, name string, input map[string]interface{}, result string) []interface{} {
	return []interface{}{
		map[string]interface{}{"role": "user", "content": prompt},
		map[string]interface{}{
			"role": "assistant",
			"content": []interface{}{map[string]interface{}{
				"type": "tool_use", "id": callID, "name": name, "input": input,
			}},
		},
		map[string]interface{}{
			"role": "user",
			"content": []interface{}{
				map[string]interface{}{"type": "tool_result", "tool_use_id": callID, "content": result},
				map[string]interface{}{"type": "text", "text": "Reply with exactly " + result + "."},
			},
		},
	}
}

func applyRoundTripResult(result *scenarioResult, response apiResponse, marker string) {
	if result == nil {
		return
	}
	result.TotalMillis += response.total.Milliseconds()
	result.RequestIDs = append(result.RequestIDs, response.requestID)
	text := responseText(response.body)
	switch {
	case !validJSONResponse(response) || strings.TrimSpace(text) == "":
		result.Status = statusFail
		result.HTTPStatus = response.statusCode
		result.Detail += "; tool_result continuation failed: " + responseErrorDetail(response)
	case !strings.Contains(text, marker):
		result.Status = statusWarn
		result.Detail += "; continuation succeeded but did not repeat the deterministic marker"
	default:
		result.Detail += "; tool_result continuation succeeded"
	}
}

func openAIToolCall(data []byte, name string) (string, string, bool) {
	var response struct {
		Choices []struct {
			Message struct {
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(data, &response) != nil {
		return "", "", false
	}
	for _, choice := range response.Choices {
		for _, tool := range choice.Message.ToolCalls {
			if tool.Function.Name == name {
				return tool.ID, tool.Function.Arguments, true
			}
		}
	}
	return "", "", false
}

func openAIToolArguments(data []byte, name string) (string, bool) {
	_, arguments, found := openAIToolCall(data, name)
	return arguments, found
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
