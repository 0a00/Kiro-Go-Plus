package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"strings"
	"time"
)

type loadProbe struct {
	protocol       string
	path           string
	stream         bool
	payload        map[string]interface{}
	expectedMarker string
	workload       string
	validation     string
	expectedTool   string
	minOutputChars int
}

const (
	loadValidationExact    = "exact"
	loadValidationContains = "contains"
	loadValidationTool     = "tool"
	realisticLoadCycle     = 14
)

func buildLoadProbe(model string, index, maxTokens int) loadProbe {
	stream := index%2 == 1
	marker := fmt.Sprintf("LOAD_OK_%d", index)
	prompt := "Reply with exactly " + marker + "."
	switch (index / 2) % 3 {
	case 1:
		return loadProbe{protocol: "openai", path: "/v1/chat/completions", stream: stream, payload: openAIChatPayload(model, stream, prompt, maxTokens), expectedMarker: marker, workload: "marker", validation: loadValidationExact}
	case 2:
		return loadProbe{protocol: "responses", path: "/v1/responses", stream: stream, payload: responsesPayload(model, stream, prompt, maxTokens), expectedMarker: marker, workload: "marker", validation: loadValidationExact}
	default:
		return loadProbe{protocol: "anthropic", path: "/v1/messages", stream: stream, payload: claudePayload(model, stream, prompt, maxTokens), expectedMarker: marker, workload: "marker", validation: loadValidationExact}
	}
}

func (r *runner) buildConfiguredLoadProbe(index, maxTokens int) loadProbe {
	if r.opts.loadProfile != "realistic" {
		return buildLoadProbe(r.model, index, maxTokens)
	}
	marker := fmt.Sprintf("LOAD_OK_%d", index)
	exactPrompt := "Reply with exactly " + marker + "."
	switch index % realisticLoadCycle {
	case 0, 1, 2, 3, 4, 5:
		probe := buildLoadProbe(r.model, index, maxTokens)
		probe.workload = "protocol-marker"
		return probe
	case 6:
		return loadProbe{
			protocol: "anthropic", path: "/v1/messages", stream: true,
			payload:        claudePayload(r.thinking, true, "Think briefly, then "+exactPrompt, maxTokens),
			expectedMarker: marker, workload: "thinking", validation: loadValidationExact,
		}
	case 7:
		minimum := min(512, max(64, maxTokens))
		prompt := "Begin with " + marker + ", then write a numbered technical explanation until near the output limit."
		return loadProbe{
			protocol: "anthropic", path: "/v1/messages", stream: true,
			payload:        claudePayload(r.model, true, prompt, maxTokens),
			expectedMarker: marker, workload: "long-stream", validation: loadValidationContains, minOutputChars: minimum,
		}
	case 8:
		return loadToolProbe(r.model, marker, maxTokens, "load_echo", "function-tool")
	case 9:
		return loadToolProbe(r.model, marker, maxTokens, "mcp__devcheck__load_echo", "mcp-tool")
	case 10:
		encoded, _ := pngBase64(false)
		return loadProbe{
			protocol: "anthropic", path: "/v1/messages", stream: false,
			payload:        loadImagePayload(r.model, encoded, exactPrompt, maxTokens),
			expectedMarker: marker, workload: "image", validation: loadValidationExact,
		}
	case 11:
		payload := claudePayload(r.model, false, exactPrompt, maxTokens)
		payload["system"] = []interface{}{map[string]interface{}{
			"type":          "text",
			"text":          fmt.Sprintf("Load cache %x. ", r.startedAt.UnixNano()) + strings.Repeat("Stable shared repository context. ", 500),
			"cache_control": map[string]interface{}{"type": "ephemeral"},
		}}
		return loadProbe{
			protocol: "anthropic", path: "/v1/messages", payload: payload,
			expectedMarker: marker, workload: "cache", validation: loadValidationExact,
		}
	case 12:
		payload := claudePayload(r.model, false, exactPrompt, maxTokens)
		payload["system"] = "Loaded development skill: preserve the exact LOAD_OK marker requested by the user."
		return loadProbe{
			protocol: "anthropic", path: "/v1/messages", payload: payload,
			expectedMarker: marker, workload: "skill-context", validation: loadValidationExact,
		}
	default:
		if !r.opts.webSearch {
			probe := buildLoadProbe(r.model, index, maxTokens)
			probe.workload = "protocol-marker"
			return probe
		}
		payload := claudePayload(r.model, true, "Use web search to find the official Kiro IDE homepage. End your answer with "+marker+".", maxTokens)
		payload["tools"] = []interface{}{webSearchTool(1)}
		return loadProbe{
			protocol: "anthropic", path: "/v1/messages", stream: true, payload: payload,
			expectedMarker: marker, workload: "websearch", validation: loadValidationContains,
		}
	}
}

func loadToolProbe(model, marker string, maxTokens int, name, workload string) loadProbe {
	payload := claudePayload(model, true, "Call "+name+" once with value "+marker+". Do not answer in text.", maxTokens)
	payload["tools"] = []interface{}{map[string]interface{}{
		"name": name, "description": "Return the supplied load-test value.",
		"input_schema": map[string]interface{}{
			"type": "object", "properties": map[string]interface{}{"value": map[string]interface{}{"type": "string"}}, "required": []string{"value"},
		},
	}}
	payload["tool_choice"] = map[string]interface{}{"type": "tool", "name": name}
	return loadProbe{
		protocol: "anthropic", path: "/v1/messages", stream: true, payload: payload,
		expectedMarker: marker, workload: workload, validation: loadValidationTool, expectedTool: name,
	}
}

func loadImagePayload(model, encoded, prompt string, maxTokens int) map[string]interface{} {
	payload := claudePayload(model, false, "", maxTokens)
	payload["messages"] = []interface{}{map[string]interface{}{
		"role": "user", "content": []interface{}{
			map[string]interface{}{"type": "image", "source": map[string]interface{}{"type": "base64", "media_type": "image/png", "data": encoded}},
			map[string]interface{}{"type": "text", "text": prompt},
		},
	}}
	return payload
}

func (r *runner) runThinkingProtocols(parent context.Context) {
	tests := []struct {
		name     string
		protocol string
		path     string
		payload  map[string]interface{}
	}{
		{
			name: "chat-thinking-stream", protocol: "openai", path: "/v1/chat/completions",
			payload: openAIChatPayload(r.thinking, true, "Calculate 211 * 37 carefully, then answer in one sentence.", 768),
		},
		{
			name: "responses-thinking-stream", protocol: "responses", path: "/v1/responses",
			payload: responsesPayload(r.thinking, true, "Calculate 211 * 37 carefully, then answer in one sentence.", 768),
		},
	}
	tests[0].payload["reasoning_effort"] = "medium"
	tests[1].payload["reasoning"] = map[string]interface{}{"effort": "medium"}
	for _, test := range tests {
		ctx, cancel := r.scenarioContext(parent)
		response := r.post(ctx, test.path, test.payload, true, true)
		cancel()
		result := streamScenarioResult(test.name, test.protocol, r.thinking, response)
		if result.Status == statusPass && response.stream.thinkingDeltas == 0 {
			result.Status = statusWarn
			result.Detail += "; no protocol-visible reasoning delta observed"
		}
		r.add(result)
	}
}

func (r *runner) runResponsesFunction(parent context.Context) {
	const name = "responses_echo"
	prompt := "Call responses_echo once with value RESPONSES_FUNCTION_OK. Do not answer in text."
	payload := responsesPayload(r.model, true, prompt, 256)
	payload["tools"] = []interface{}{map[string]interface{}{
		"type": "function", "name": name, "description": "Return the supplied value.",
		"parameters": map[string]interface{}{
			"type": "object", "properties": map[string]interface{}{"value": map[string]interface{}{"type": "string"}}, "required": []string{"value"},
		},
	}}
	payload["tool_choice"] = map[string]interface{}{"type": "function", "name": name}
	ctx, cancel := r.scenarioContext(parent)
	response := r.post(ctx, "/v1/responses", payload, true, true)
	cancel()
	result := streamScenarioResult("responses-function-call", "responses", r.model, response)
	call, found := response.stream.toolCall(name)
	arguments, hasArguments := response.stream.toolArguments(name)
	var input map[string]interface{}
	if result.Status == statusPass && (!found || call.id == "" || !hasArguments || !response.stream.hasSingleCompleteTool(name) || json.Unmarshal([]byte(arguments), &input) != nil || input["value"] != "RESPONSES_FUNCTION_OK") {
		result.Status = statusFail
		result.Detail += "; expected one complete forced Responses function call"
	}
	if result.Status == statusPass {
		continuation := responsesPayload(r.model, false, "", 64)
		continuation["input"] = []interface{}{
			map[string]interface{}{"type": "message", "role": "user", "content": prompt},
			map[string]interface{}{"type": "function_call", "call_id": call.id, "name": name, "arguments": arguments},
			map[string]interface{}{"type": "function_call_output", "call_id": call.id, "output": "RESPONSES_RESULT_OK"},
			map[string]interface{}{"type": "message", "role": "user", "content": "Reply with exactly RESPONSES_RESULT_OK."},
		}
		followCtx, followCancel := r.scenarioContext(parent)
		follow := r.post(followCtx, "/v1/responses", continuation, true, false)
		followCancel()
		applyRoundTripResult(&result, follow, "RESPONSES_RESULT_OK")
	}
	r.add(result)
}

func (r *runner) runResponsesCustomTool(parent context.Context) {
	const name = "dev_exec"
	prompt := "Call dev_exec once with the exact freeform input CUSTOM_TOOL_OK. Do not answer in text."
	payload := responsesPayload(r.model, true, "", 256)
	payload["input"] = []interface{}{
		map[string]interface{}{
			"type": "additional_tools", "tools": []interface{}{map[string]interface{}{
				"type": "custom", "name": name, "description": "Accept a short freeform development command.",
			}},
		},
		map[string]interface{}{"type": "message", "role": "user", "content": prompt},
	}
	payload["tool_choice"] = map[string]interface{}{"type": "custom", "name": name}
	ctx, cancel := r.scenarioContext(parent)
	response := r.post(ctx, "/v1/responses", payload, true, true)
	cancel()
	result := streamScenarioResult("responses-custom-tool", "responses", r.model, response)
	call, found := response.stream.toolCall(name)
	input, hasInput := response.stream.toolArguments(name)
	if result.Status == statusPass && (!found || call.id == "" || !hasInput || !response.stream.hasSingleCompleteTool(name) || !strings.Contains(input, "CUSTOM_TOOL_OK")) {
		result.Status = statusFail
		result.Detail += "; expected one complete custom tool call with freeform input"
	}
	if result.Status == statusPass {
		continuation := responsesPayload(r.model, false, "", 64)
		continuation["input"] = []interface{}{
			map[string]interface{}{"type": "message", "role": "user", "content": prompt},
			map[string]interface{}{"type": "custom_tool_call", "call_id": call.id, "name": name, "input": input},
			map[string]interface{}{"type": "custom_tool_call_output", "call_id": call.id, "output": "CUSTOM_RESULT_OK"},
			map[string]interface{}{"type": "message", "role": "user", "content": "Reply with exactly CUSTOM_RESULT_OK."},
		}
		followCtx, followCancel := r.scenarioContext(parent)
		follow := r.post(followCtx, "/v1/responses", continuation, true, false)
		followCancel()
		applyRoundTripResult(&result, follow, "CUSTOM_RESULT_OK")
	}
	r.add(result)
}

func (r *runner) runPromptCacheReuse(parent context.Context) {
	nonce := fmt.Sprintf("%x", r.startedAt.UnixNano())
	stablePrefix := "Cache probe " + nonce + ". " + strings.Repeat("Stable repository context must remain byte-identical across requests. ", 420)
	payload := claudePayload(r.model, false, "Reply with exactly CACHE_OK.", 32)
	payload["system"] = []interface{}{map[string]interface{}{
		"type": "text", "text": stablePrefix, "cache_control": map[string]interface{}{"type": "ephemeral"},
	}}
	responses := make([]apiResponse, 0, 3)
	for attempt := 0; attempt < 3; attempt++ {
		ctx, cancel := r.scenarioContext(parent)
		response := r.post(ctx, "/v1/messages", payload, true, false)
		cancel()
		responses = append(responses, response)
		if response.err != nil || !validJSONResponse(response) {
			break
		}
	}
	result := responseScenarioResult("prompt-cache-reuse", "anthropic", r.model, responses[len(responses)-1], false)
	result.TotalMillis = 0
	var cold, warm tokenUsage
	for index, response := range responses {
		result.TotalMillis += response.total.Milliseconds()
		result.RequestIDs = append(result.RequestIDs, response.requestID)
		if !validJSONResponse(response) || strings.TrimSpace(responseText(response.body)) == "" {
			result.Status = statusFail
			result.Detail = fmt.Sprintf("attempt %d failed: %s", index+1, responseErrorDetail(response))
			r.add(result)
			return
		}
		usage := extractTokenUsageFromBody(response.body)
		if index == 0 {
			cold = usage
		}
		if usage.cacheReadTokens > warm.cacheReadTokens {
			warm = usage
			result.RequestID = response.requestID
		}
	}
	result.InputTokens = warm.inputTokens
	result.OutputTokens = warm.outputTokens
	result.CacheReadTokens = warm.cacheReadTokens
	result.CacheCreateTokens = warm.cacheCreationTokens
	total := warm.anthropicInputTotal()
	ratio := 0.0
	if total > 0 {
		ratio = 100 * float64(warm.cacheReadTokens) / float64(total)
	}
	result.Detail = fmt.Sprintf("cold_create=%d cold_read=%d warm_read=%d warm_total_input=%d hit_ratio=%.1f%% attempts=%d", cold.cacheCreationTokens, cold.cacheReadTokens, warm.cacheReadTokens, total, ratio, len(responses))
	switch {
	case cold.anthropicInputTotal() == 0 && warm.anthropicInputTotal() == 0:
		result.Status = statusWarn
		result.Detail += "; upstream returned no input usage"
	case warm.cacheReadTokens == 0:
		result.Status = statusWarn
		result.Detail += "; no cache read observed (check accounting mode and account affinity)"
	default:
		result.Status = statusPass
	}
	r.add(result)
}

func (r *runner) runMultimodalAccounting(parent context.Context) {
	solid, err := pngBase64(false)
	if err != nil {
		r.add(scenarioResult{Name: "multimodal-accounting", Status: statusFail, Protocol: "anthropic", Model: r.model, Detail: err.Error()})
		return
	}
	noisy, err := pngBase64(true)
	if err != nil {
		r.add(scenarioResult{Name: "multimodal-accounting", Status: statusFail, Protocol: "anthropic", Model: r.model, Detail: err.Error()})
		return
	}
	tests := []struct {
		name, protocol, path string
	}{
		{name: "anthropic-multimodal-accounting", protocol: "anthropic", path: "/v1/messages"},
		{name: "chat-multimodal-accounting", protocol: "openai", path: "/v1/chat/completions"},
		{name: "responses-multimodal-accounting", protocol: "responses", path: "/v1/responses"},
	}
	for _, test := range tests {
		request := func(encoded string) apiResponse {
			ctx, cancel := r.scenarioContext(parent)
			defer cancel()
			return r.post(ctx, test.path, multimodalPayload(test.protocol, r.model, encoded), true, false)
		}
		solidResponse := request(solid)
		noisyResponse := request(noisy)
		result := responseScenarioResult(test.name, test.protocol, r.model, noisyResponse, false)
		if !validJSONResponse(solidResponse) || !validJSONResponse(noisyResponse) || responseText(solidResponse.body) == "" || responseText(noisyResponse.body) == "" {
			result.Status = statusFail
			result.Detail = "image request failed: solid=" + responseErrorDetail(solidResponse) + "; noisy=" + responseErrorDetail(noisyResponse)
			r.add(result)
			continue
		}
		solidUsage := extractTokenUsageFromBody(solidResponse.body)
		noisyUsage := extractTokenUsageFromBody(noisyResponse.body)
		solidInput := reportedMultimodalInput(test.protocol, solidUsage)
		noisyInput := reportedMultimodalInput(test.protocol, noisyUsage)
		difference := solidInput - noisyInput
		if difference < 0 {
			difference = -difference
		}
		result.Detail = fmt.Sprintf("dimensions=96x96 solid_base64=%d noisy_base64=%d solid_input=%d noisy_input=%d difference=%d", len(solid), len(noisy), solidInput, noisyInput, difference)
		switch {
		case solidInput == 0 || noisyInput == 0:
			result.Status = statusWarn
			result.Detail += "; usage unavailable, functional image transport only"
		case difference > 1024:
			result.Status = statusFail
			result.Detail += "; token usage appears dependent on encoded byte length"
		default:
			result.Status = statusPass
		}
		r.add(result)
	}
}

func multimodalPayload(protocol, model, encoded string) map[string]interface{} {
	dataURL := "data:image/png;base64," + encoded
	switch protocol {
	case "openai":
		payload := openAIChatPayload(model, false, "", 32)
		payload["messages"] = []interface{}{map[string]interface{}{
			"role": "user", "content": []interface{}{
				map[string]interface{}{"type": "image_url", "image_url": map[string]interface{}{"url": dataURL}},
				map[string]interface{}{"type": "text", "text": "Reply with exactly IMAGE_OK."},
			},
		}}
		return payload
	case "responses":
		payload := responsesPayload(model, false, "", 32)
		payload["input"] = []interface{}{map[string]interface{}{
			"type": "message", "role": "user", "content": []interface{}{
				map[string]interface{}{"type": "input_image", "image_url": dataURL},
				map[string]interface{}{"type": "input_text", "text": "Reply with exactly IMAGE_OK."},
			},
		}}
		return payload
	default:
		payload := claudePayload(model, false, "", 32)
		payload["messages"] = []interface{}{map[string]interface{}{
			"role": "user", "content": []interface{}{
				map[string]interface{}{"type": "image", "source": map[string]interface{}{"type": "base64", "media_type": "image/png", "data": encoded}},
				map[string]interface{}{"type": "text", "text": "Reply with exactly IMAGE_OK."},
			},
		}}
		return payload
	}
}

func reportedMultimodalInput(protocol string, usage tokenUsage) int {
	if protocol == "anthropic" {
		return usage.anthropicInputTotal()
	}
	return usage.inputTokens
}

func pngBase64(noisy bool) (string, error) {
	const size = 96
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	state := uint32(0x9e3779b9)
	nextByte := func() uint8 {
		state ^= state << 13
		state ^= state >> 17
		state ^= state << 5
		return uint8(state)
	}
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			pixel := color.NRGBA{R: 32, G: 128, B: 224, A: 255}
			if noisy {
				pixel = color.NRGBA{R: nextByte(), G: nextByte(), B: nextByte(), A: 255}
			}
			img.SetNRGBA(x, y, pixel)
		}
	}
	var output bytes.Buffer
	if err := png.Encode(&output, img); err != nil {
		return "", fmt.Errorf("encode test PNG: %w", err)
	}
	return base64.StdEncoding.EncodeToString(output.Bytes()), nil
}

func (r *runner) runOutputLimit(parent context.Context) {
	prompt := "Write 100 numbered lines, each containing eight different words. Do not stop early."
	tests := []struct {
		name, protocol, path string
		payload              map[string]interface{}
	}{
		{"anthropic-output-limit", "anthropic", "/v1/messages", claudePayload(r.model, true, prompt, 24)},
		{"chat-output-limit", "openai", "/v1/chat/completions", openAIChatPayload(r.model, true, prompt, 24)},
		{"responses-output-limit", "responses", "/v1/responses", responsesPayload(r.model, true, prompt, 24)},
	}
	for _, test := range tests {
		ctx, cancel := r.scenarioContext(parent)
		response := r.post(ctx, test.path, test.payload, true, true)
		cancel()
		result := streamScenarioResult(test.name, test.protocol, r.model, response)
		if response.stream.incomplete && result.Status != statusFail {
			result.Status = statusPass
			result.Detail += "; output limit reported cleanly"
		} else if result.Status == statusPass {
			result.Status = statusWarn
			result.Detail += "; model completed before reaching the requested output limit"
		}
		r.add(result)
	}
}

func (r *runner) runLongStream(parent context.Context) {
	prompt := "Write 50 numbered lines. Each line must contain six different short words. Begin immediately and do not use tools."
	ctx, cancel := r.scenarioContext(parent)
	response := r.post(ctx, "/v1/messages", claudePayload(r.model, true, prompt, 768), true, true)
	cancel()
	result := streamScenarioResult("anthropic-long-stream", "anthropic", r.model, response)
	if result.Status == statusPass && response.stream.contentDeltas <= 2 {
		result.Status = statusWarn
		result.Detail += "; long output arrived in too few content deltas"
	}
	if result.Status == statusPass && response.stream.firstSemantic > 0 && response.total-response.stream.firstSemantic < 50*time.Millisecond {
		result.Status = statusWarn
		result.Detail += "; long output appears burst-buffered"
	}
	if result.Status == statusPass && response.stream.maxActivityGap > 60*time.Second {
		result.Status = statusWarn
		result.Detail += "; stream contained a wire-activity gap above 60s"
	}
	r.add(result)
}
