package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
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
	if parent == nil {
		parent = context.Background()
	}
	r.runLoadWarmup(parent)
	execution := r.executeLoad(parent, r.opts.concurrency, r.opts.requests, r.effectiveLoadMaxTokens())
	r.correlateLoadSamples(parent, execution.samples)
	r.add(r.finalizeLoadResult(buildLoadExecutionResult("concurrent-load", r.model, r.opts.concurrency, r.opts.requests, execution)))
	r.runPostLoadRecovery(parent)
}

func (r *runner) runStaircase(parent context.Context) {
	if parent == nil {
		parent = context.Background()
	}
	r.runLoadWarmup(parent)
	for level, concurrency := range r.opts.concurrencySteps {
		requests := max(r.opts.requests, concurrency)
		levelParent := parent
		expected := requests
		holdMode := r.opts.staircaseHold > 0
		var levelCancel context.CancelFunc
		if holdMode {
			levelParent, levelCancel = context.WithTimeout(parent, r.opts.staircaseHold)
			requests = r.opts.staircaseMaxRequests
		}
		execution := r.executeLoadPatternWithParents(levelParent, parent, parent, concurrency, requests, r.effectiveLoadMaxTokens(), "closed", 0, 0, 0)
		if levelCancel != nil {
			levelCancel()
			expected = execution.scheduled
		}
		r.correlateLoadSamples(parent, execution.samples)
		result := r.finalizeLoadResult(buildLoadExecutionResult(fmt.Sprintf("concurrency-staircase-%d", concurrency), r.model, concurrency, expected, execution))
		if holdMode {
			result.Detail += fmt.Sprintf("; level_hold=%s level_request_cap=%d", r.opts.staircaseHold, r.opts.staircaseMaxRequests)
		} else {
			result.Detail += "; level requests are max(--requests, concurrency)"
		}
		r.add(result)
		if parent.Err() != nil {
			return
		}
		if level < len(r.opts.concurrencySteps)-1 && r.opts.staircaseCooldown > 0 {
			if !sleepWithContext(parent, r.opts.staircaseCooldown) {
				return
			}
		}
	}
	r.runPostLoadRecovery(parent)
}

func (r *runner) runSoak(parent context.Context) {
	if parent == nil {
		parent = context.Background()
	}
	r.runLoadWarmup(parent)
	maxTokensPerRequest := r.effectiveLoadMaxTokens()
	target := min(r.opts.soakMaxRequests, r.opts.soakTokenBudget/maxTokensPerRequest)
	execution := r.executeSoakExecution(parent, r.opts.concurrency, target, maxTokensPerRequest, r.opts.soakDuration)
	scheduled := execution.scheduled
	durationReached := execution.soakDurationReached
	r.correlateLoadSamples(parent, execution.samples)
	result := r.finalizeLoadResult(buildLoadExecutionResult("bounded-soak", r.model, r.opts.concurrency, scheduled, execution))
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
	r.runPostLoadRecovery(parent)
}

func (r *runner) runLoadWarmup(parent context.Context) {
	if parent == nil {
		parent = context.Background()
	}
	if r.opts.warmupRequests == 0 || parent.Err() != nil {
		return
	}
	concurrency := min(r.opts.concurrency, r.opts.warmupRequests)
	execution := r.executeLoadPattern(parent, concurrency, r.opts.warmupRequests, r.effectiveLoadMaxTokens(), "closed", 0, 0, 1_000_000)
	result := buildLoadExecutionResult("load-warmup", r.model, concurrency, r.opts.warmupRequests, execution)
	result.WarmupRequests = r.opts.warmupRequests
	r.add(r.finalizeLoadResult(result))
}

func (r *runner) effectiveLoadMaxTokens() int {
	if r.opts.loadMaxTokens > 0 {
		return r.opts.loadMaxTokens
	}
	return 32
}

type loadExecution struct {
	samples                []loadSample
	scheduleWall           time.Duration
	wall                   time.Duration
	scheduled              int
	dropped                int
	pattern                string
	targetRPS              float64
	clientGoroutineDelta   int
	clientHeapDeltaBytes   int64
	clientGoroutinesBefore int
	clientHeapBefore       uint64
	clientGoroutinesAfter  int
	clientHeapAfter        uint64
	clientResourceSamples  int
	clientPeakGoroutines   int
	clientPeakHeapAlloc    uint64
	serverStatsBefore      loadServerStats
	serverStatsAfter       loadServerStats
	serverStatsSamples     int
	serverStatsErrors      int
	soakDurationReached    bool
}

type loadJob struct {
	index       int
	scheduledAt time.Time
}

type loadSample struct {
	duration        int64
	headers         int64
	ttft            int64
	maxStreamGap    int64
	maxWireGap      int64
	queueDelay      int64
	stream          bool
	protocol        string
	workload        string
	requestID       string
	outputTokens    int
	success         bool
	category        string
	endpoint        string
	selectionMS     int64
	accountAttempts int
	affinityHit     bool
	cacheStatus     string
	toolUses        int
	correlated      bool
}

func (r *runner) executeLoad(parent context.Context, concurrency, requests, maxTokens int) loadExecution {
	return r.executeLoadPattern(parent, concurrency, requests, maxTokens, r.opts.loadPattern, r.opts.targetRPS, r.opts.rampDuration, 0)
}

func (r *runner) executeLoadPattern(parent context.Context, concurrency, requests, maxTokens int, pattern string, targetRPS float64, rampDuration time.Duration, indexOffset int) loadExecution {
	return r.executeLoadPatternWithParents(parent, parent, parent, concurrency, requests, maxTokens, pattern, targetRPS, rampDuration, indexOffset)
}

// executeLoadPatternWithParents separates the scheduling window from the
// request lifetime. A staircase hold should stop admitting new work at the
// level deadline, but requests already admitted must get their normal request
// timeout to finish and be measured accurately.
func (r *runner) executeLoadPatternWithParents(scheduleParent, requestParent, statsParent context.Context, concurrency, requests, maxTokens int, pattern string, targetRPS float64, rampDuration time.Duration, indexOffset int) loadExecution {
	pattern = normalizeLoadPattern(pattern)
	if pattern != "closed" && (!finitePositive(targetRPS)) {
		pattern = "closed"
		targetRPS = 0
		rampDuration = 0
	}
	if pattern == "closed" {
		targetRPS = 0
		rampDuration = 0
	}
	if concurrency < 1 || requests < 1 {
		return loadExecution{pattern: pattern, targetRPS: targetRPS}
	}
	if maxTokens < 1 {
		maxTokens = 1
	}
	if scheduleParent == nil {
		scheduleParent = context.Background()
	}
	if requestParent == nil {
		requestParent = scheduleParent
	}
	if statsParent == nil {
		statsParent = requestParent
	}
	if requestParent.Err() != nil {
		return loadExecution{pattern: pattern, targetRPS: targetRPS}
	}
	suiteTimeout := loadSuiteTimeout(pattern, concurrency, requests, targetRPS, r.opts.timeout, rampDuration)
	ctx, cancel := context.WithTimeout(scheduleParent, suiteTimeout)
	defer cancel()
	queueCapacity := 0
	if pattern != "closed" {
		queueCapacity = concurrency
	}
	jobs := make(chan loadJob, queueCapacity)
	results := make(chan loadSample, requests)
	var workers sync.WaitGroup
	resourcesBefore := readLoadResourceSnapshot()
	resourceSampler := newLoadResourceSampler(requestParent, r.opts.resourceSampleInterval)
	var serverBefore loadServerStats
	var serverBeforeErr error
	if r.opts.collectServerStats {
		serverBefore, serverBeforeErr = r.fetchLoadServerStats(loadStatsContext(statsParent))
	}
	serverStatsSamples := 0
	serverStatsErrors := 0
	if r.opts.collectServerStats && serverBeforeErr == nil {
		serverStatsSamples++
	} else if r.opts.collectServerStats {
		serverStatsErrors++
	}
	startedAt := time.Now()
	for worker := 0; worker < concurrency; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for job := range jobs {
				requestStartedAt := time.Now()
				probe := r.buildConfiguredLoadProbe(job.index, maxTokens)
				requestCtx, requestCancel := r.scenarioContext(requestParent)
				response := r.post(requestCtx, probe.path, probe.payload, true, probe.stream)
				requestCancel()
				sample := classifyLoadSample(response, probe)
				if !job.scheduledAt.IsZero() {
					sample.queueDelay = max(requestStartedAt.Sub(job.scheduledAt).Milliseconds(), 0)
				}
				results <- sample
			}
		}()
	}

	scheduled := 0
	dropped := 0
	scheduleStartedAt := time.Now()
	nextArrival := scheduleStartedAt
	for requestIndex := 0; requestIndex < requests; requestIndex++ {
		index := indexOffset + requestIndex
		if pattern == "closed" {
			select {
			case jobs <- loadJob{index: index, scheduledAt: time.Now()}:
				scheduled++
			case <-ctx.Done():
				requestIndex = requests
			}
			continue
		}

		if !waitUntil(ctx, nextArrival) {
			break
		}
		scheduled++
		job := loadJob{index: index, scheduledAt: nextArrival}
		select {
		case jobs <- job:
		default:
			probe := r.buildConfiguredLoadProbe(index, maxTokens)
			results <- loadSample{
				protocol: probe.protocol, workload: probe.workload, stream: probe.stream,
				queueDelay: max(time.Since(nextArrival).Milliseconds(), 0), category: "client_overload",
			}
			dropped++
		}
		nextArrival = nextArrival.Add(loadArrivalInterval(pattern, targetRPS, time.Since(scheduleStartedAt), rampDuration))
	}
	scheduleWall := time.Since(scheduleStartedAt)
	close(jobs)
	workers.Wait()
	close(results)
	samples := make([]loadSample, 0, requests)
	for sample := range results {
		samples = append(samples, sample)
	}
	markDuplicateLoadRequestIDs(samples)
	resourceSampler.stop()
	resourceSamples, peakGoroutines, peakHeapAlloc := resourceSampler.summary()
	resourcesAfter := readLoadResourceSnapshot()
	loadWall := time.Since(startedAt)
	var serverAfter loadServerStats
	var serverAfterErr error
	if r.opts.collectServerStats {
		serverAfter, serverAfterErr = r.fetchLoadServerStats(loadStatsContext(statsParent))
	}
	if r.opts.collectServerStats && serverAfterErr == nil {
		serverStatsSamples++
	} else if r.opts.collectServerStats {
		serverStatsErrors++
	}
	return loadExecution{
		samples: samples, scheduleWall: scheduleWall, wall: loadWall, scheduled: scheduled, dropped: dropped,
		pattern: pattern, targetRPS: targetRPS,
		clientGoroutineDelta:   resourcesAfter.goroutines - resourcesBefore.goroutines,
		clientHeapDeltaBytes:   signedUint64Delta(resourcesAfter.heapAlloc, resourcesBefore.heapAlloc),
		clientGoroutinesBefore: resourcesBefore.goroutines, clientHeapBefore: resourcesBefore.heapAlloc,
		clientGoroutinesAfter: resourcesAfter.goroutines, clientHeapAfter: resourcesAfter.heapAlloc,
		clientResourceSamples: resourceSamples, clientPeakGoroutines: peakGoroutines, clientPeakHeapAlloc: peakHeapAlloc,
		serverStatsBefore: serverBefore, serverStatsAfter: serverAfter,
		serverStatsSamples: serverStatsSamples, serverStatsErrors: serverStatsErrors,
	}
}

func loadSuiteTimeout(pattern string, concurrency, requests int, targetRPS float64, requestTimeout time.Duration, rampDurations ...time.Duration) time.Duration {
	pattern = normalizeLoadPattern(pattern)
	if requests < 1 {
		requests = 1
	}
	if concurrency < 1 {
		concurrency = 1
	}
	if requestTimeout <= 0 {
		requestTimeout = time.Second
	}
	if pattern == "closed" || !finitePositive(targetRPS) {
		waves := (requests-1)/concurrency + 1
		return saturatingLoadDuration(float64(waves)*requestTimeout.Seconds() + 1)
	}
	scheduleSeconds := float64(requests) / targetRPS
	if pattern == "ramp" {
		if len(rampDurations) > 0 && rampDurations[0] > 0 {
			// The rate is never below 10% of target. Adding one ramp
			// duration is a conservative upper bound for filling the ramp,
			// followed by the target-rate schedule.
			scheduleSeconds += rampDurations[0].Seconds()
		} else {
			// Preserve a safe bound for direct callers that do not provide
			// the ramp duration.
			scheduleSeconds *= 10
		}
	}
	return saturatingLoadDuration(scheduleSeconds + requestTimeout.Seconds() + 1)
}

func loadArrivalInterval(pattern string, targetRPS float64, elapsed, rampDuration time.Duration) time.Duration {
	rate := targetRPS
	if pattern == "ramp" && rampDuration > 0 && elapsed >= 0 && elapsed < rampDuration {
		progress := float64(elapsed) / float64(rampDuration)
		progress = min(max(progress, 0), 1)
		rate *= 0.1 + 0.9*progress
	}
	if !finitePositive(rate) {
		return 0
	}
	intervalNanos := float64(time.Second) / rate
	maxDuration := float64((1 << 63) - 1)
	if math.IsInf(intervalNanos, 0) || intervalNanos >= maxDuration {
		return time.Duration((1 << 63) - 1)
	}
	return max(time.Duration(intervalNanos), time.Microsecond)
}

func normalizeLoadPattern(pattern string) string {
	switch strings.ToLower(strings.TrimSpace(pattern)) {
	case "fixed", "ramp":
		return strings.ToLower(strings.TrimSpace(pattern))
	default:
		return "closed"
	}
}

func finitePositive(value float64) bool {
	return value > 0 && !math.IsNaN(value) && !math.IsInf(value, 0)
}

func saturatingLoadDuration(seconds float64) time.Duration {
	if math.IsNaN(seconds) || seconds <= 0 {
		return time.Second
	}
	maxDuration := float64((1<<63)-1) / float64(time.Second)
	if math.IsInf(seconds, 0) || seconds >= maxDuration {
		return time.Duration((1 << 63) - 1)
	}
	duration := time.Duration(math.Ceil(seconds * float64(time.Second)))
	if duration < time.Nanosecond {
		return time.Nanosecond
	}
	return duration
}

func waitUntil(ctx context.Context, target time.Time) bool {
	delay := time.Until(target)
	if delay <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func sleepWithContext(ctx context.Context, duration time.Duration) bool {
	if duration <= 0 {
		return ctx == nil || ctx.Err() == nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func markDuplicateLoadRequestIDs(samples []loadSample) {
	seen := make(map[string]struct{}, len(samples))
	for index := range samples {
		requestID := strings.TrimSpace(samples[index].requestID)
		if requestID == "" {
			continue
		}
		if _, exists := seen[requestID]; exists {
			samples[index].success = false
			samples[index].category = "duplicate_request_id"
			continue
		}
		seen[requestID] = struct{}{}
	}
}

func (r *runner) executeSoak(parent context.Context, concurrency, target, maxTokens int, duration time.Duration) ([]loadSample, int, bool) {
	execution := r.executeSoakExecution(parent, concurrency, target, maxTokens, duration)
	return execution.samples, execution.scheduled, execution.soakDurationReached
}

func (r *runner) executeSoakExecution(parent context.Context, concurrency, target, maxTokens int, duration time.Duration) loadExecution {
	if concurrency < 1 || target < 1 {
		return loadExecution{pattern: "soak"}
	}
	if maxTokens < 1 {
		maxTokens = 1
	}
	if duration <= 0 {
		return loadExecution{pattern: "soak", soakDurationReached: true}
	}
	if parent == nil {
		parent = context.Background()
	}
	resourcesBefore := readLoadResourceSnapshot()
	resourceSampler := newLoadResourceSampler(parent, r.opts.resourceSampleInterval)
	var serverBefore loadServerStats
	var serverBeforeErr error
	if r.opts.collectServerStats {
		serverBefore, serverBeforeErr = r.fetchLoadServerStats(loadStatsContext(parent))
	}
	serverStatsSamples := 0
	serverStatsErrors := 0
	if r.opts.collectServerStats && serverBeforeErr == nil {
		serverStatsSamples++
	} else if r.opts.collectServerStats {
		serverStatsErrors++
	}
	startedAt := time.Now()
	scheduleStartedAt := startedAt
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
				probe := r.buildConfiguredLoadProbe(index, maxTokens)
				requestCtx, requestCancel := r.scenarioContext(parent)
				response := r.post(requestCtx, probe.path, probe.payload, true, probe.stream)
				requestCancel()
				results <- classifyLoadSample(response, probe)
			}
		}()
	}
	type scheduleResult struct {
		count           int
		durationReached bool
		wall            time.Duration
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
				scheduledResult <- scheduleResult{count: scheduled, durationReached: errors.Is(scheduleCtx.Err(), context.DeadlineExceeded), wall: time.Since(scheduleStartedAt)}
				return
			}
		}
		scheduledResult <- scheduleResult{count: scheduled, wall: time.Since(scheduleStartedAt)}
	}()
	workers.Wait()
	close(results)
	schedule := <-scheduledResult
	samples := make([]loadSample, 0, schedule.count)
	for sample := range results {
		samples = append(samples, sample)
	}
	markDuplicateLoadRequestIDs(samples)
	resourceSampler.stop()
	resourceSamples, peakGoroutines, peakHeapAlloc := resourceSampler.summary()
	resourcesAfter := readLoadResourceSnapshot()
	loadWall := time.Since(startedAt)
	var serverAfter loadServerStats
	var serverAfterErr error
	if r.opts.collectServerStats {
		serverAfter, serverAfterErr = r.fetchLoadServerStats(loadStatsContext(parent))
	}
	if r.opts.collectServerStats && serverAfterErr == nil {
		serverStatsSamples++
	} else if r.opts.collectServerStats {
		serverStatsErrors++
	}
	return loadExecution{
		samples: samples, scheduleWall: schedule.wall, wall: loadWall, scheduled: schedule.count,
		pattern: "soak", clientGoroutineDelta: resourcesAfter.goroutines - resourcesBefore.goroutines,
		clientHeapDeltaBytes:   signedUint64Delta(resourcesAfter.heapAlloc, resourcesBefore.heapAlloc),
		clientGoroutinesBefore: resourcesBefore.goroutines, clientHeapBefore: resourcesBefore.heapAlloc,
		clientGoroutinesAfter: resourcesAfter.goroutines, clientHeapAfter: resourcesAfter.heapAlloc,
		clientResourceSamples: resourceSamples, clientPeakGoroutines: peakGoroutines, clientPeakHeapAlloc: peakHeapAlloc,
		serverStatsBefore: serverBefore, serverStatsAfter: serverAfter,
		serverStatsSamples: serverStatsSamples, serverStatsErrors: serverStatsErrors,
		soakDurationReached: schedule.durationReached,
	}
}

func classifyLoadSample(response apiResponse, probe loadProbe) loadSample {
	protocol := probe.protocol
	if protocol == "" {
		protocol = "anthropic"
	}
	usage := extractTokenUsageFromBody(response.body)
	if probe.stream {
		usage = response.stream.usage
	}
	sample := loadSample{
		duration: response.total.Milliseconds(), headers: response.headers.Milliseconds(), stream: probe.stream,
		protocol: protocol, workload: probe.workload, requestID: response.requestID, outputTokens: usage.outputTokens,
	}
	if probe.stream {
		sample.ttft = response.stream.firstSemantic.Milliseconds()
		sample.maxStreamGap = response.stream.maxEventGap.Milliseconds()
		sample.maxWireGap = response.stream.maxActivityGap.Milliseconds()
	}
	switch {
	case errors.Is(response.err, errResponseTooLarge):
		sample.category = "response_too_large"
	case response.err != nil && probe.stream && response.stream.semanticOutput && !response.stream.terminal:
		// Keep the observable failure mode: once semantic data reached the
		// client, an ended stream is truncated even when the transport reports
		// the underlying deadline or cancellation cause.
		sample.category = "stream_truncated"
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
	case probe.stream && response.stream.errorEvent != "":
		sample.category = "sse_error"
	case probe.stream && response.stream.incomplete:
		sample.category = "output_limit"
	case probe.stream && (!response.stream.terminal || !response.stream.semanticOutput):
		sample.category = "stream_protocol"
	case !probe.stream && responseText(response.body) == "":
		sample.category = "empty_response"
	case probe.expectedMarker != "" && !loadResponseMatchesMarker(response, probe):
		sample.category = "marker_mismatch"
	default:
		sample.success = true
	}
	return sample
}

func loadResponseMatchesMarker(response apiResponse, probe loadProbe) bool {
	if probe.validation == loadValidationTool {
		arguments, found := response.stream.toolArguments(probe.expectedTool)
		var input map[string]interface{}
		return found && response.stream.hasSingleCompleteTool(probe.expectedTool) && json.Unmarshal([]byte(arguments), &input) == nil && input["value"] == probe.expectedMarker
	}
	text := responseText(response.body)
	if probe.stream {
		text = response.stream.outputText()
	}
	if probe.minOutputChars > 0 && len([]rune(text)) < probe.minOutputChars {
		return false
	}
	if probe.validation == loadValidationContains {
		return containsLoadMarker(text, probe.expectedMarker)
	}
	return strings.TrimSpace(text) == probe.expectedMarker
}

func containsLoadMarker(text, marker string) bool {
	if marker == "" {
		return false
	}
	for offset := 0; offset < len(text); {
		index := strings.Index(text[offset:], marker)
		if index < 0 {
			return false
		}
		end := offset + index + len(marker)
		if end == len(text) || text[end] < '0' || text[end] > '9' {
			return true
		}
		offset = end
	}
	return false
}

func buildLoadResult(name, model string, concurrency, expected int, samples []loadSample) scenarioResult {
	markDuplicateLoadRequestIDs(samples)
	return buildLoadExecutionResult(name, model, concurrency, expected, loadExecution{samples: samples, scheduled: len(samples), pattern: "closed"})
}

func buildLoadExecutionResult(name, model string, concurrency, expected int, execution loadExecution) scenarioResult {
	if expected < 0 {
		expected = 0
	}
	samples := execution.samples
	durations := make([]int64, 0, len(samples))
	successDurations := make([]int64, 0, len(samples))
	failureDurations := make([]int64, 0, len(samples))
	headers := make([]int64, 0, len(samples))
	ttfts := make([]int64, 0, len(samples))
	queueDelays := make([]int64, 0, len(samples))
	streamGaps := make([]int64, 0, len(samples))
	wireGaps := make([]int64, 0, len(samples))
	selectionTimes := make([]int64, 0, len(samples))
	successes := 0
	streamRequests := 0
	streamSuccesses := 0
	nonStreamSuccesses := 0
	failures := make(map[string]int)
	protocolSuccesses := make(map[string]int)
	workloadSuccesses := make(map[string]int)
	workloadFailures := make(map[string]int)
	endpointCounts := make(map[string]int)
	requestIDs := make(map[string]struct{})
	correlated := 0
	accountAttempts := 0
	affinityHits := 0
	cacheHits := 0
	toolUses := 0
	outputTokens := 0
	for _, sample := range samples {
		if sample.duration > 0 {
			durations = append(durations, sample.duration)
		}
		if sample.headers > 0 {
			headers = append(headers, sample.headers)
		}
		if sample.ttft > 0 {
			ttfts = append(ttfts, sample.ttft)
		}
		if sample.queueDelay > 0 {
			queueDelays = append(queueDelays, sample.queueDelay)
		}
		if sample.maxStreamGap > 0 {
			streamGaps = append(streamGaps, sample.maxStreamGap)
		}
		if sample.maxWireGap > 0 {
			wireGaps = append(wireGaps, sample.maxWireGap)
		}
		if sample.stream {
			streamRequests++
		}
		workload := sample.workload
		if workload == "" {
			workload = "marker"
		}
		if sample.success {
			successes++
			protocolSuccesses[sample.protocol]++
			workloadSuccesses[workload]++
			if sample.duration > 0 {
				successDurations = append(successDurations, sample.duration)
			}
			if sample.stream {
				streamSuccesses++
			} else {
				nonStreamSuccesses++
			}
		} else {
			workloadFailures[workload]++
			if sample.duration > 0 {
				failureDurations = append(failureDurations, sample.duration)
			}
			category := sample.category
			if category == "" {
				category = "unknown"
			}
			if sample.protocol != "" {
				category = sample.protocol + "_" + category
			}
			failures[category]++
		}
		if sample.requestID != "" {
			requestIDs[sample.requestID] = struct{}{}
		}
		outputTokens += sample.outputTokens
		if sample.correlated {
			correlated++
			accountAttempts += sample.accountAttempts
			if sample.selectionMS > 0 {
				selectionTimes = append(selectionTimes, sample.selectionMS)
			}
			if sample.affinityHit {
				affinityHits++
			}
			if sample.cacheStatus == "hit" || sample.cacheStatus == "partial_hit" {
				cacheHits++
			}
			toolUses += sample.toolUses
			if sample.endpoint != "" {
				endpointCounts[sample.endpoint]++
			}
		}
	}
	if missing := expected - len(samples); missing > 0 {
		failures["not_started_or_canceled"] += missing
	}
	completed := max(len(samples)-execution.dropped, 0)
	serverDelta := loadServerStats{}
	serverCounterReset := false
	if execution.serverStatsSamples >= 2 {
		serverDelta, serverCounterReset = loadServerStatsDelta(execution.serverStatsBefore, execution.serverStatsAfter)
	}
	peakGoroutineGrowth := max(execution.clientPeakGoroutines-execution.clientGoroutinesBefore, 0)
	peakHeapGrowth := signedUint64Delta(execution.clientPeakHeapAlloc, execution.clientHeapBefore)
	result := scenarioResult{
		Name: name, Protocol: "load", Model: model, Requests: expected, Successes: successes,
		ScheduledRequests: execution.scheduled, DroppedRequests: execution.dropped,
		SampleCount: len(samples), CompletedRequests: completed,
		DistinctRequestIDs: len(requestIDs), OutputTokens: outputTokens,
		FailureCategories: failures, WorkloadSuccesses: workloadSuccesses, WorkloadFailures: workloadFailures,
		EndpointCounts: endpointCounts, CorrelatedRequests: correlated, AccountAttempts: accountAttempts,
		AffinityHits: affinityHits, CacheHits: cacheHits, ToolUses: toolUses,
		P50Millis: percentile(durations, 0.50), P95Millis: percentile(durations, 0.95), P99Millis: percentile(durations, 0.99),
		SuccessP50Millis: percentile(successDurations, 0.50), SuccessP95Millis: percentile(successDurations, 0.95), SuccessP99Millis: percentile(successDurations, 0.99),
		FailureP50Millis: percentile(failureDurations, 0.50), FailureP95Millis: percentile(failureDurations, 0.95), FailureP99Millis: percentile(failureDurations, 0.99),
		HeaderP95Millis: percentile(headers, 0.95), TTFTP50Millis: percentile(ttfts, 0.50), TTFTP95Millis: percentile(ttfts, 0.95), TTFTP99Millis: percentile(ttfts, 0.99),
		QueueP95Millis: percentile(queueDelays, 0.95), StreamGapP95MS: percentile(streamGaps, 0.95), WireGapP95MS: percentile(wireGaps, 0.95),
		ScheduleMillis: execution.scheduleWall.Milliseconds(), WallMillis: execution.wall.Milliseconds(), TargetRPS: execution.targetRPS, SelectionP95MS: percentile(selectionTimes, 0.95),
		ClientGoroutineDelta: execution.clientGoroutineDelta, ClientHeapDeltaBytes: execution.clientHeapDeltaBytes,
		ClientResourceSamples: execution.clientResourceSamples, ClientPeakGoroutines: execution.clientPeakGoroutines,
		ClientPeakHeapAllocBytes:  serverSafeInt64(execution.clientPeakHeapAlloc),
		ClientPeakGoroutineGrowth: peakGoroutineGrowth,
		ClientPeakHeapGrowthBytes: peakHeapGrowth,
		ServerStatsSamples:        execution.serverStatsSamples, ServerStatsRequestsDelta: serverDelta.requests,
		ServerStatsTokensDelta: serverDelta.tokens, ServerStatsErrors: execution.serverStatsErrors, ServerStatsCounterReset: serverCounterReset,
	}
	if execution.wall > 0 {
		seconds := execution.wall.Seconds()
		result.AchievedRPS = float64(completed) / seconds
		result.SuccessRPS = float64(successes) / seconds
	}
	arrivalWindow := loadArrivalWindow(execution)
	if arrivalWindow > 0 {
		result.ArrivalRPS = float64(execution.scheduled) / arrivalWindow.Seconds()
	}
	if expected > 0 {
		result.SuccessRate = 100 * float64(successes) / float64(expected)
	}
	if execution.scheduled > 0 {
		result.CompletionRate = 100 * float64(completed) / float64(execution.scheduled)
		result.ClientOverloadRate = 100 * float64(execution.dropped) / float64(execution.scheduled)
	}
	result.TotalMillis = execution.wall.Milliseconds()
	result.Detail = fmt.Sprintf(
		"success=%d/%d completed=%d samples=%d success_rate=%.1f%% completion_rate=%.1f%% overload_rate=%.1f%% stream=%d/%d nonstream=%d/%d concurrency=%d pattern=%s arrival_rps=%.2f achieved_rps=%.2f success_rps=%.2f p95=%dms ttft_p95=%dms schedule=%dms wall=%dms failures=%s",
		successes, expected, completed,
		len(samples), result.SuccessRate, result.CompletionRate, result.ClientOverloadRate,
		streamSuccesses, streamRequests,
		nonStreamSuccesses, len(samples)-streamRequests,
		concurrency, execution.pattern, result.ArrivalRPS, result.AchievedRPS, result.SuccessRPS, result.P95Millis, result.TTFTP95Millis, result.ScheduleMillis, result.WallMillis, formatFailureCategories(failures),
	)
	result.Detail += "; protocols=" + formatFailureCategories(protocolSuccesses)
	result.Detail += fmt.Sprintf("; client_goroutines_delta=%d peak_growth=%d heap_delta=%dB", result.ClientGoroutineDelta, result.ClientPeakGoroutineGrowth, result.ClientHeapDeltaBytes)
	if result.ServerStatsErrors > 0 {
		result.Detail += fmt.Sprintf("; server_stats_errors=%d", result.ServerStatsErrors)
	}
	if result.ServerStatsCounterReset {
		result.Detail += "; server_counters=reset"
	}
	if expected == 0 {
		result.Status = statusFail
		result.Detail += "; no requests were expected"
	} else if successes == expected && len(samples) == expected {
		result.Status = statusPass
	} else {
		result.Status = statusFail
	}
	if len(failures) == 0 {
		result.FailureCategories = nil
	}
	if len(workloadFailures) == 0 {
		result.WorkloadFailures = nil
	}
	if len(endpointCounts) == 0 {
		result.EndpointCounts = nil
	}
	return result
}

func loadArrivalWindow(execution loadExecution) time.Duration {
	if execution.pattern == "fixed" || execution.pattern == "ramp" {
		if execution.scheduleWall > 0 {
			return execution.scheduleWall
		}
	}
	return execution.wall
}

func serverSafeInt64(value uint64) int64 {
	maxInt64 := uint64(^uint64(0) >> 1)
	if value > maxInt64 {
		return int64(maxInt64)
	}
	return int64(value)
}

func (r *runner) finalizeLoadResult(result scenarioResult) scenarioResult {
	if r == nil || result.Protocol != "load" || result.Requests <= 0 {
		return result
	}
	violations := make([]string, 0, 7)
	if r.opts.minSuccessRate > 0 && result.SuccessRate < r.opts.minSuccessRate {
		violations = append(violations, fmt.Sprintf("success_rate %.1f%% < %.1f%%", result.SuccessRate, r.opts.minSuccessRate))
	}
	if r.opts.maxP95Millis > 0 && result.P95Millis > r.opts.maxP95Millis {
		violations = append(violations, fmt.Sprintf("p95 %dms > %dms", result.P95Millis, r.opts.maxP95Millis))
	}
	if r.opts.maxTTFTP95Millis > 0 && result.TTFTP95Millis > r.opts.maxTTFTP95Millis {
		violations = append(violations, fmt.Sprintf("ttft_p95 %dms > %dms", result.TTFTP95Millis, r.opts.maxTTFTP95Millis))
	}
	if r.opts.maxStreamGapMillis > 0 && result.StreamGapP95MS > r.opts.maxStreamGapMillis {
		violations = append(violations, fmt.Sprintf("stream_gap_p95 %dms > %dms", result.StreamGapP95MS, r.opts.maxStreamGapMillis))
	}
	if r.opts.maxClientOverloadRate >= 0 && result.ClientOverloadRate > r.opts.maxClientOverloadRate {
		violations = append(violations, fmt.Sprintf("client_overload_rate %.1f%% > %.1f%%", result.ClientOverloadRate, r.opts.maxClientOverloadRate))
	}
	if r.opts.maxClientGoroutineGrowth > 0 && result.ClientGoroutineDelta > r.opts.maxClientGoroutineGrowth {
		violations = append(violations, fmt.Sprintf("client_goroutine_delta %d > %d", result.ClientGoroutineDelta, r.opts.maxClientGoroutineGrowth))
	}
	if r.opts.maxClientHeapGrowthMB > 0 {
		limit := r.opts.maxClientHeapGrowthMB * 1024 * 1024
		if result.ClientHeapDeltaBytes > limit {
			violations = append(violations, fmt.Sprintf("client_heap_delta %dB > %dB", result.ClientHeapDeltaBytes, limit))
		}
	}
	if r.opts.requireServerStats {
		if result.ServerStatsErrors > 0 {
			violations = append(violations, fmt.Sprintf("server_stats_errors=%d", result.ServerStatsErrors))
		}
		if result.ServerStatsSamples < 2 {
			violations = append(violations, fmt.Sprintf("server_stats_samples=%d < 2", result.ServerStatsSamples))
		}
		if result.ServerStatsCounterReset {
			violations = append(violations, "server_stats_counter_reset")
		}
	}
	if len(violations) > 0 {
		result.Status = statusFail
		result.ThresholdFailures = append(result.ThresholdFailures, violations...)
		result.Detail += "; threshold_failures=" + strings.Join(violations, " | ")
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
