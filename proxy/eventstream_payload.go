package proxy

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
)

// streamEventKind is the small set of semantic events the downstream
// translators understand. Upstream event names have changed casing and
// separators between Kiro data planes, so the parser classifies them before
// dispatching callbacks.
type streamEventKind uint8

const (
	streamEventUnknown streamEventKind = iota
	streamEventAssistant
	streamEventReasoning
	streamEventTool
	streamEventMetering
	streamEventContextUsage
	streamEventMetadata
	streamEventCompletion
	streamEventAuxiliary
)

var (
	streamAssistantWrapperNames = []string{
		"assistantResponseEvent", "assistant_response_event",
		"assistantResponse", "assistant_response",
		"assistantMessage", "assistant_message",
		"assistantResponseMessage", "assistant_response_message",
		"responseMessage", "response_message",
	}
	streamReasoningWrapperNames = []string{
		"reasoningContentEvent", "reasoning_content_event",
		"reasoningEvent", "reasoning_event",
		"thinkingEvent", "thinking_event",
		"reasoningContent", "reasoning_content",
		"thinkingContent", "thinking_content",
	}
	streamMetadataWrapperNames = []string{
		"metadataEvent", "metadata_event",
		"messageMetadataEvent", "message_metadata_event",
		"metadata", "messageMetadata", "message_metadata",
		"responseMetadata", "response_metadata",
	}
	streamToolWrapperNames = []string{
		"toolUseEvent", "tool_use_event",
		"toolUse", "tool_use",
		"toolUseStartEvent", "tool_use_start_event",
		"toolUseInputEvent", "tool_use_input_event",
		"toolUseStopEvent", "tool_use_stop_event",
		"toolUseCompleteEvent", "tool_use_complete_event",
	}
	streamTelemetryWrapperNames = []string{
		"meteringEvent", "metering",
		"usageEvent", "usage_event",
		"usage", "tokenUsage", "token_usage",
		"metricsEvent", "metrics_event", "metrics",
		"tokenMetrics", "token_metrics",
	}
	streamAuxiliaryWrapperNames = []string{
		"followupPromptEvent", "followup_prompt_event",
		"followupPrompt", "followup_prompt",
		"codeReferenceEvent", "code_reference_event",
		"supplementaryWebLinksEvent", "supplementary_web_links_event",
		"supplementaryWebLinks", "supplementary_web_links",
		"citationEvent", "citation_event", "citation",
	}
	streamResponseWrapperNames = []string{
		"assistantResponseEvent", "assistant_response_event",
		"assistantResponse", "assistant_response",
		"assistantMessage", "assistant_message",
		"assistantResponseMessage", "assistant_response_message",
		"responseMessage", "response_message",
		"message", "response",
		"metadataEvent", "metadata_event",
		"messageMetadataEvent", "message_metadata_event",
		"metadata", "messageMetadata", "message_metadata",
		"responseMetadata", "response_metadata",
	}
)

type decodedStreamEvent struct {
	kind         streamEventKind
	text         string
	reasoning    string
	toolPayloads []map[string]interface{}
	stopReason   string
	terminal     bool
	telemetry    bool
}

// eventStreamDiagnostics deliberately stores only counters. In particular, it
// never retains event payloads, prompts, tool arguments, image bytes, or token
// values. This makes it safe to include a compact summary in an error trace.
type eventStreamDiagnostics struct {
	mu            sync.Mutex
	frames        int
	payloadBytes  int
	recognized    int
	unknown       int
	text          int
	reasoning     int
	tools         int
	telemetry     int
	completions   int
	invalidFrames int
}

func (d *eventStreamDiagnostics) record(payloadBytes int, event decodedStreamEvent) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.frames++
	if payloadBytes > 0 {
		d.payloadBytes += payloadBytes
	}
	if event.kind == streamEventUnknown {
		d.unknown++
	} else {
		d.recognized++
	}
	if event.text != "" {
		d.text++
	}
	if event.reasoning != "" {
		d.reasoning++
	}
	if len(event.toolPayloads) > 0 {
		d.tools += len(event.toolPayloads)
	}
	if event.telemetry {
		d.telemetry++
	}
	if event.terminal || event.stopReason != "" {
		d.completions++
	}
}

func (d *eventStreamDiagnostics) recordInvalid() {
	if d == nil {
		return
	}
	d.mu.Lock()
	d.invalidFrames++
	d.mu.Unlock()
}

func (d *eventStreamDiagnostics) summary() string {
	if d == nil {
		return ""
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.frames == 0 && d.invalidFrames == 0 {
		return ""
	}
	return fmt.Sprintf("frames=%d payload_bytes=%d recognized=%d unknown=%d text=%d reasoning=%d tools=%d telemetry=%d completion=%d invalid=%d",
		d.frames, d.payloadBytes, d.recognized, d.unknown, d.text, d.reasoning, d.tools, d.telemetry, d.completions, d.invalidFrames)
}

func normalizeStreamIdentifier(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return ""
	}
	var normalized strings.Builder
	normalized.Grow(len(value))
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			normalized.WriteRune(r)
		}
	}
	return normalized.String()
}

func streamField(m map[string]interface{}, names ...string) (interface{}, bool) {
	if m == nil {
		return nil, false
	}
	for _, name := range names {
		if value, ok := m[name]; ok {
			return value, true
		}
	}
	wanted := make(map[string]struct{}, len(names))
	for _, name := range names {
		if normalized := normalizeStreamIdentifier(name); normalized != "" {
			wanted[normalized] = struct{}{}
		}
	}
	for name, value := range m {
		if _, ok := wanted[normalizeStreamIdentifier(name)]; ok {
			return value, true
		}
	}
	return nil, false
}

func streamMapField(m map[string]interface{}, names ...string) (map[string]interface{}, bool) {
	value, ok := streamField(m, names...)
	if !ok {
		return nil, false
	}
	nested, ok := value.(map[string]interface{})
	return nested, ok && nested != nil
}

func streamArrayField(m map[string]interface{}, names ...string) ([]interface{}, bool) {
	value, ok := streamField(m, names...)
	if !ok {
		return nil, false
	}
	items, ok := value.([]interface{})
	return items, ok
}

func mergeStreamMaps(outer, nested map[string]interface{}) map[string]interface{} {
	if outer == nil && nested == nil {
		return nil
	}
	merged := make(map[string]interface{}, len(outer)+len(nested))
	for key, value := range outer {
		merged[key] = value
	}
	for key, value := range nested {
		merged[key] = value
	}
	return merged
}

func streamWrapperMap(event map[string]interface{}, names ...string) (map[string]interface{}, bool) {
	nested, ok := streamMapField(event, names...)
	if !ok {
		return nil, false
	}
	return mergeStreamMaps(event, nested), true
}

// streamCandidateMaps returns the outer event and the common one/two-level
// response wrappers. Kiro has emitted both direct fields and nested message
// objects over time, so extraction must not depend on one envelope shape.
func streamCandidateMaps(event map[string]interface{}, names ...string) []map[string]interface{} {
	candidates := []map[string]interface{}{event}
	if event == nil {
		return candidates
	}
	for _, name := range names {
		nested, ok := streamMapField(event, name)
		if !ok {
			continue
		}
		candidates = append(candidates, nested, mergeStreamMaps(event, nested))
		for _, innerName := range names {
			inner, ok := streamMapField(nested, innerName)
			if !ok {
				continue
			}
			candidates = append(candidates, inner, mergeStreamMaps(nested, inner))
		}
	}
	return candidates
}

// unwrapStreamEnvelope handles one or two harmless transport wrappers used by
// some proxy/data-plane combinations, while requiring the nested value to
// contain a known response-shaped field before unwrapping it.
func unwrapStreamEnvelope(event map[string]interface{}) map[string]interface{} {
	current := event
	for depth := 0; depth < 2; depth++ {
		var nested map[string]interface{}
		for _, name := range []string{"data", "payload", "event"} {
			candidate, ok := streamMapField(current, name)
			if ok && streamLooksLikePayload(candidate) {
				nested = candidate
				break
			}
		}
		if nested == nil {
			break
		}
		current = mergeStreamMaps(current, nested)
	}
	return current
}

func streamLooksLikePayload(value map[string]interface{}) bool {
	if value == nil {
		return false
	}
	for _, name := range []string{
		"content", "text", "thinking", "reasoning", "delta", "contentBlockDelta", "content_block_delta",
		"assistantResponseEvent", "assistant_response_event", "reasoningContentEvent", "reasoning_content_event",
		"assistantResponseMessage", "assistant_response_message", "responseMessage", "response_message",
		"messageMetadataEvent", "message_metadata_event", "usageEvent", "usage_event", "metricsEvent", "metrics_event",
		"followupPromptEvent", "followup_prompt_event", "codeReferenceEvent", "code_reference_event",
		"supplementaryWebLinksEvent", "supplementary_web_links_event",
		"toolUseEvent", "tool_use", "toolUses", "tool_uses", "usage", "tokenUsage", "metrics",
		"stopReason", "stop_reason", "finishReason", "finish_reason", "status", "messageStatus", "message_status",
	} {
		if _, ok := streamField(value, name); ok {
			return true
		}
	}
	return false
}

func classifyStreamEvent(eventType string, event map[string]interface{}) streamEventKind {
	normalizedType := normalizeStreamIdentifier(eventType)
	switch {
	case strings.Contains(normalizedType, "tooluse"):
		return streamEventTool
	case normalizedType == "contentblockstart" || normalizedType == "contentblockdelta" || normalizedType == "contentblockstop":
		if streamContentBlockIsTool(event) {
			return streamEventTool
		}
		if streamContentBlockIsReasoning(event) {
			return streamEventReasoning
		}
		return streamEventAssistant
	case strings.Contains(normalizedType, "reasoning") || strings.Contains(normalizedType, "thinking"):
		return streamEventReasoning
	case strings.Contains(normalizedType, "assistantresponse") || normalizedType == "assistantmessage" || normalizedType == "assistantresponse":
		return streamEventAssistant
	case strings.Contains(normalizedType, "contextusage"):
		return streamEventContextUsage
	case strings.Contains(normalizedType, "metering") || strings.Contains(normalizedType, "usage") || strings.Contains(normalizedType, "metrics"):
		return streamEventMetering
	case strings.Contains(normalizedType, "metadata"):
		return streamEventMetadata
	case normalizedType == "messagestop" || normalizedType == "messagecomplete" || normalizedType == "responsecomplete" || normalizedType == "responsecompleted" || normalizedType == "completionevent" || normalizedType == "endofstream" || normalizedType == "streamcomplete" || normalizedType == "streamcompleted":
		return streamEventCompletion
	case strings.Contains(normalizedType, "followupprompt") ||
		strings.Contains(normalizedType, "codereference") ||
		strings.Contains(normalizedType, "supplementaryweblink") ||
		strings.Contains(normalizedType, "citation"):
		return streamEventAuxiliary
	}

	if streamHasToolFields(event) || streamHasToolWrapper(event) {
		return streamEventTool
	}
	if streamMapHasAssistantWrapper(event) {
		return streamEventAssistant
	}
	if streamMapHasReasoningWrapper(event) {
		return streamEventReasoning
	}
	if streamMapHasMetadataWrapper(event) || streamHasStopReason(event) {
		return streamEventMetadata
	}
	if streamHasUsageFields(event) {
		return streamEventMetering
	}
	if streamMapHasAuxiliaryWrapper(event) {
		return streamEventAuxiliary
	}
	if _, ok := streamField(event, "thinking", "reasoning", "reasoningContent", "reasoning_content"); ok {
		return streamEventReasoning
	}
	if _, ok := streamField(event, "content", "outputText", "output_text", "completion"); ok {
		return streamEventAssistant
	}
	if normalizedType == "" || normalizedType == "delta" || normalizedType == "textdelta" || strings.Contains(normalizedType, "response") {
		if _, ok := streamField(event, "text"); ok {
			return streamEventAssistant
		}
	}
	return streamEventUnknown
}

func decodeStreamEvent(eventType string, event map[string]interface{}, current *toolUseState) decodedStreamEvent {
	return decodeStreamEventWithPending(eventType, event, nil, current)
}

func decodeStreamEventWithPending(eventType string, event map[string]interface{}, pending *pendingToolUseSet, current *toolUseState) decodedStreamEvent {
	event = unwrapStreamEnvelope(event)
	kind := classifyStreamEvent(eventType, event)
	decoded := decodedStreamEvent{kind: kind}
	decoded.text = extractStreamText(event, kind)
	decoded.reasoning = extractStreamReasoning(event, kind)
	decoded.toolPayloads = extractStreamToolPayloadsWithPending(eventType, event, current, pending)
	if len(decoded.toolPayloads) > 0 && kind == streamEventUnknown {
		decoded.kind = streamEventTool
	}
	decoded.stopReason = extractStreamStopReason(event, kind)
	decoded.terminal = streamEventHasTerminalSignal(eventType, event, kind, decoded.stopReason)
	decoded.telemetry = kind == streamEventMetering || kind == streamEventContextUsage || kind == streamEventMetadata || kind == streamEventAuxiliary
	return decoded
}

func streamMapHasAssistantWrapper(event map[string]interface{}) bool {
	_, ok := streamMapField(event, streamAssistantWrapperNames...)
	return ok
}

func streamMapHasReasoningWrapper(event map[string]interface{}) bool {
	_, ok := streamMapField(event, streamReasoningWrapperNames...)
	return ok
}

func streamMapHasMetadataWrapper(event map[string]interface{}) bool {
	_, ok := streamMapField(event, streamMetadataWrapperNames...)
	return ok
}

func streamMapHasAuxiliaryWrapper(event map[string]interface{}) bool {
	_, ok := streamMapField(event, streamAuxiliaryWrapperNames...)
	return ok
}

func streamHasUsageFields(event map[string]interface{}) bool {
	if _, ok := streamMapField(event, streamTelemetryWrapperNames...); ok {
		return true
	}
	_, hasInput := streamField(event, "inputTokens", "input_tokens", "outputTokens", "output_tokens", "totalTokens", "total_tokens")
	return hasInput
}

func streamHasStopReason(event map[string]interface{}) bool {
	_, ok := streamField(event, "stopReason", "stop_reason", "finishReason", "finish_reason", "reason")
	return ok
}

func streamHasToolWrapper(event map[string]interface{}) bool {
	_, ok := streamMapField(event, streamToolWrapperNames...)
	return ok
}

func streamHasToolFields(event map[string]interface{}) bool {
	_, hasID := streamField(event, "toolUseId", "tool_use_id", "toolUseID", "id", "callId", "call_id")
	_, hasName := streamField(event, "name", "toolName", "tool_name")
	_, hasInput := streamField(event, "input", "arguments", "partialJson", "partial_json", "inputDelta", "input_delta")
	_, hasStop := streamField(event, "stop", "isStop", "done", "completed", "complete", "isFinal", "final")
	_, hasIndex := streamField(event, "contentBlockIndex", "content_block_index", "index", "blockIndex", "block_index")
	return hasName && (hasID || hasInput || hasStop || hasIndex) ||
		hasID && (hasInput || hasStop || hasIndex) ||
		hasIndex && (hasInput || hasStop)
}

func extractStreamText(event map[string]interface{}, kind streamEventKind) string {
	if kind != streamEventAssistant && kind != streamEventUnknown {
		return ""
	}
	candidates := streamCandidateMaps(event, streamAssistantWrapperNames...)
	for _, name := range []string{"message", "delta", "contentBlockDelta", "content_block_delta", "contentBlock", "content_block"} {
		if nested, ok := streamMapField(event, name); ok {
			candidates = append(candidates, nested)
			if delta, ok := streamMapField(nested, "delta"); ok {
				candidates = append(candidates, delta)
			}
		}
	}
	for _, candidate := range candidates {
		if text, ok := streamTextFields(candidate, "content", "text", "outputText", "output_text", "completion"); ok && text != "" {
			return text
		}
	}
	return ""
}

func extractStreamReasoning(event map[string]interface{}, kind streamEventKind) string {
	if kind != streamEventReasoning && kind != streamEventAssistant && kind != streamEventUnknown {
		return ""
	}
	candidates := streamCandidateMaps(event, streamReasoningWrapperNames...)
	candidates = append(candidates, streamCandidateMaps(event, streamAssistantWrapperNames...)...)
	for _, name := range []string{"delta", "contentBlockDelta", "content_block_delta", "message"} {
		if nested, ok := streamMapField(event, name); ok {
			candidates = append(candidates, nested)
			if delta, ok := streamMapField(nested, "delta"); ok {
				candidates = append(candidates, delta)
			}
		}
	}
	reasoningFields := []string{"thinking", "reasoning", "reasoningContent", "reasoning_content"}
	if kind == streamEventReasoning {
		reasoningFields = append([]string{"text", "content"}, reasoningFields...)
	}
	for _, candidate := range candidates {
		if text, ok := streamTextFields(candidate, reasoningFields...); ok && text != "" {
			return text
		}
	}
	return ""
}

func streamTextFields(m map[string]interface{}, names ...string) (string, bool) {
	for _, name := range names {
		value, ok := streamField(m, name)
		if !ok {
			continue
		}
		if text, ok := streamTextValue(value); ok {
			return text, true
		}
	}
	return "", false
}

func streamTextValue(value interface{}) (string, bool) {
	switch typed := value.(type) {
	case string:
		return typed, true
	case []interface{}:
		var result strings.Builder
		found := false
		for _, item := range typed {
			object, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			blockType, _ := streamStringField(object, "type", "blockType", "block_type")
			normalizedType := normalizeStreamIdentifier(blockType)
			if strings.Contains(normalizedType, "tooluse") || strings.Contains(normalizedType, "tool_use") {
				continue
			}
			text, ok := streamTextFields(object, "text", "content", "value")
			if ok {
				result.WriteString(text)
				found = true
			}
		}
		return result.String(), found
	case map[string]interface{}:
		return streamTextFields(typed, "text", "content", "value")
	default:
		return "", false
	}
}

func streamContentBlockIsTool(event map[string]interface{}) bool {
	block, ok := streamMapField(event, "contentBlock", "content_block")
	if !ok {
		block = event
	}
	blockType, _ := streamStringField(block, "type", "blockType", "block_type")
	normalizedType := normalizeStreamIdentifier(blockType)
	if strings.Contains(normalizedType, "tooluse") || strings.Contains(normalizedType, "tool") {
		return true
	}
	if delta, ok := streamMapField(event, "delta"); ok {
		deltaType, _ := streamStringField(delta, "type")
		normalizedDeltaType := normalizeStreamIdentifier(deltaType)
		if strings.Contains(normalizedDeltaType, "inputjson") || strings.Contains(normalizedDeltaType, "tooluse") || strings.Contains(normalizedDeltaType, "partialjson") {
			return true
		}
		_, hasPartialJSON := streamField(delta, "partialJson", "partial_json")
		return hasPartialJSON
	}
	return false
}

func streamContentBlockIsReasoning(event map[string]interface{}) bool {
	block, ok := streamMapField(event, "contentBlock", "content_block")
	if ok {
		blockType, _ := streamStringField(block, "type", "blockType", "block_type")
		normalizedType := normalizeStreamIdentifier(blockType)
		if strings.Contains(normalizedType, "thinking") || strings.Contains(normalizedType, "reasoning") {
			return true
		}
	}
	delta, ok := streamMapField(event, "delta")
	if !ok {
		return false
	}
	deltaType, _ := streamStringField(delta, "type")
	normalizedType := normalizeStreamIdentifier(deltaType)
	return strings.Contains(normalizedType, "thinking") || strings.Contains(normalizedType, "reasoning")
}

func extractStreamToolPayloads(eventType string, event map[string]interface{}, current *toolUseState) []map[string]interface{} {
	return extractStreamToolPayloadsWithPending(eventType, event, current, nil)
}

func extractStreamToolPayloadsWithPending(eventType string, event map[string]interface{}, current *toolUseState, pending *pendingToolUseSet) []map[string]interface{} {
	var payloads []map[string]interface{}
	hasDedicatedPayload := false

	if assistant, ok := streamWrapperMap(event, streamAssistantWrapperNames...); ok {
		if items, ok := streamArrayField(assistant, "toolUses", "tool_uses"); ok {
			for _, item := range items {
				object, ok := item.(map[string]interface{})
				if !ok {
					continue
				}
				tool := canonicalToolPayload(object)
				tool["stop"] = true
				payloads = append(payloads, tool)
			}
			if len(payloads) > 0 {
				hasDedicatedPayload = true
			}
		}
	}

	if nested, ok := streamMapField(event, streamToolWrapperNames...); ok {
		tool := canonicalToolPayload(mergeStreamMaps(event, nested))
		if toolEventHasData(eventType, tool, nested) {
			payloads = append(payloads, tool)
			hasDedicatedPayload = true
		}
	}

	if block, ok := streamMapField(event, "contentBlock", "content_block"); ok && streamContentBlockIsTool(event) {
		tool := canonicalToolPayload(mergeStreamMaps(event, block))
		tool["stop"] = tool["stop"] == true || normalizeStreamIdentifier(eventType) == "contentblockstop"
		payloads = append(payloads, tool)
		hasDedicatedPayload = true
	} else if streamContentBlockIsTool(event) && normalizeStreamIdentifier(eventType) == "contentblockstop" {
		tool := canonicalToolPayload(event)
		tool["stop"] = true
		payloads = append(payloads, tool)
		hasDedicatedPayload = true
	}

	normalizedType := normalizeStreamIdentifier(eventType)
	if !hasDedicatedPayload && normalizedType == "contentblockstop" && pending != nil {
		if index := streamContentBlockIndexFrom(event); index != "" && pending.getByContentBlockIndex(index) != nil {
			tool := canonicalToolPayload(event)
			tool["stop"] = true
			payloads = append(payloads, tool)
			hasDedicatedPayload = true
		}
	}

	// Anthropic-style content_block events carry both text/thinking blocks and
	// tool blocks. The event name alone is therefore not enough to route one to
	// the tool assembler; streamContentBlockIsTool and the field checks below
	// provide the required payload evidence.
	directToolType := strings.Contains(normalizedType, "tooluse") || strings.Contains(normalizedType, "inputjson") || strings.Contains(normalizedType, "partialjson")
	if !hasDedicatedPayload && (directToolType || streamHasToolFields(event) || (current != nil && streamHasToolContinuation(event))) {
		tool := canonicalToolPayload(event)
		if directToolType && (strings.Contains(normalizedType, "stop") || strings.Contains(normalizedType, "end") || strings.Contains(normalizedType, "complete") || normalizedType == "contentblockstop") {
			tool["stop"] = true
		}
		payloads = append(payloads, tool)
	}
	return payloads
}

func streamHasToolContinuation(event map[string]interface{}) bool {
	_, hasInput := streamField(event, "input", "arguments", "partialJson", "partial_json", "inputDelta", "input_delta")
	_, hasStop := streamField(event, "stop", "isStop", "done", "completed", "complete", "isFinal", "final")
	if hasInput || hasStop {
		return true
	}
	if delta, ok := streamMapField(event, "delta"); ok {
		_, hasDeltaInput := streamField(delta, "partialJson", "partial_json", "input", "inputDelta", "input_delta", "arguments")
		return hasDeltaInput
	}
	return false
}

func toolEventHasData(eventType string, canonical, nested map[string]interface{}) bool {
	if strings.Contains(normalizeStreamIdentifier(eventType), "tooluse") {
		return true
	}
	return streamHasToolFields(canonical) || streamHasToolFields(nested) || streamHasToolContinuation(nested)
}

func canonicalToolPayload(event map[string]interface{}) map[string]interface{} {
	canonical := make(map[string]interface{}, 4)
	if id := streamStringFrom(event, "toolUseId", "tool_use_id", "toolUseID", "id", "callId", "call_id"); id != "" {
		canonical["toolUseId"] = id
	}
	if name := streamToolName(event); name != "" {
		canonical["name"] = name
	}
	if index := streamContentBlockIndexFrom(event); index != "" {
		canonical["contentBlockIndex"] = index
	}
	if input, ok := streamToolInput(event); ok {
		canonical["input"] = input
	}
	if stop := streamBoolFrom(event, "stop", "isStop", "done", "completed", "complete", "isFinal", "final"); stop {
		canonical["stop"] = true
	}
	return canonical
}

func streamToolName(event map[string]interface{}) string {
	if name := streamStringFrom(event, "name", "toolName", "tool_name"); name != "" {
		return name
	}
	if function, ok := streamMapField(event, "function"); ok {
		return streamStringFrom(function, "name", "toolName", "tool_name")
	}
	if block, ok := streamMapField(event, "contentBlock", "content_block"); ok {
		return streamStringFrom(block, "name", "toolName", "tool_name")
	}
	return ""
}

func streamToolInput(event map[string]interface{}) (interface{}, bool) {
	if value, ok := streamField(event, "input", "arguments", "partialJson", "partial_json", "inputDelta", "input_delta"); ok {
		return value, true
	}
	if function, ok := streamMapField(event, "function"); ok {
		if value, found := streamField(function, "arguments", "input"); found {
			return value, true
		}
	}
	if delta, ok := streamMapField(event, "delta"); ok {
		if value, found := streamField(delta, "partialJson", "partial_json", "input", "inputDelta", "input_delta", "arguments"); found {
			return value, true
		}
	}
	if block, ok := streamMapField(event, "contentBlock", "content_block"); ok {
		if value, found := streamField(block, "input", "arguments"); found {
			return value, true
		}
	}
	return nil, false
}

func streamContentBlockIndexFrom(event map[string]interface{}) string {
	value, ok := streamField(event, "contentBlockIndex", "content_block_index", "index", "blockIndex", "block_index")
	if !ok {
		return ""
	}
	return streamIndexValue(value)
}

func streamIndexValue(value interface{}) string {
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	case json.Number:
		if integer, err := typed.Int64(); err == nil {
			return strconv.FormatInt(integer, 10)
		}
		return strings.TrimSpace(typed.String())
	case float64:
		return strconv.FormatInt(int64(typed), 10)
	case float32:
		return strconv.FormatInt(int64(typed), 10)
	case int:
		return strconv.Itoa(typed)
	case int8:
		return strconv.FormatInt(int64(typed), 10)
	case int16:
		return strconv.FormatInt(int64(typed), 10)
	case int32:
		return strconv.FormatInt(int64(typed), 10)
	case int64:
		return strconv.FormatInt(typed, 10)
	case uint:
		return strconv.FormatUint(uint64(typed), 10)
	case uint8:
		return strconv.FormatUint(uint64(typed), 10)
	case uint16:
		return strconv.FormatUint(uint64(typed), 10)
	case uint32:
		return strconv.FormatUint(uint64(typed), 10)
	case uint64:
		return strconv.FormatUint(typed, 10)
	default:
		return ""
	}
}

func extractStreamStopReason(event map[string]interface{}, kind streamEventKind) string {
	if kind == streamEventReasoning || kind == streamEventTool || kind == streamEventMetering || kind == streamEventContextUsage {
		return ""
	}
	candidates := streamCandidateMaps(event, streamResponseWrapperNames...)
	for _, name := range []string{"message", "response", "delta", "contentBlockDelta", "content_block_delta"} {
		if nested, ok := streamMapField(event, name); ok {
			candidates = append(candidates, nested)
			if inner, ok := streamMapField(nested, "message", "response", "metadata", "metadataEvent", "messageMetadataEvent"); ok {
				candidates = append(candidates, inner)
			}
		}
	}
	for _, candidate := range candidates {
		if reason := streamStringFrom(candidate, "stopReason", "stop_reason", "finishReason", "finish_reason", "reason"); reason != "" {
			return reason
		}
	}
	return ""
}

func streamEventHasTerminalSignal(eventType string, event map[string]interface{}, kind streamEventKind, stopReason string) bool {
	if stopReason != "" || kind == streamEventCompletion {
		return true
	}
	normalizedType := normalizeStreamIdentifier(eventType)
	if strings.HasSuffix(normalizedType, "completeevent") || strings.HasSuffix(normalizedType, "completedevent") || normalizedType == "endofstream" || normalizedType == "messagestop" || normalizedType == "responsestop" {
		return true
	}
	if strings.Contains(normalizedType, "assistantresponsemessage") || normalizedType == "assistantmessage" {
		return true
	}
	if kind == streamEventMetadata || kind == streamEventAssistant {
		candidates := streamCandidateMaps(event, streamResponseWrapperNames...)
		for _, candidate := range candidates {
			if complete, ok := streamField(candidate, "isFinal", "is_final", "completed", "complete", "finished"); ok && streamBoolValue(complete) {
				return true
			}
			status := strings.ToLower(strings.TrimSpace(streamStringFrom(candidate, "messageStatus", "message_status", "status")))
			switch status {
			case "completed", "complete", "finished", "done", "success", "succeeded":
				return true
			}
		}
	}
	if kind == streamEventAssistant {
		if message, ok := streamMapField(event, "assistantResponseMessage", "assistant_response_message"); ok {
			if _, hasContent := streamTextFields(message, "content", "text", "outputText", "output_text", "completion"); hasContent {
				// A standalone assistantResponseMessage is a complete message, while
				// assistantResponseEvent remains incremental and needs a terminal frame.
				if normalizedType == "" || strings.Contains(normalizedType, "assistantresponsemessage") || strings.Contains(normalizedType, "assistantmessage") {
					return true
				}
			}
		}
	}
	return false
}

func streamStringField(m map[string]interface{}, names ...string) (string, bool) {
	value, ok := streamField(m, names...)
	if !ok {
		return "", false
	}
	text, ok := value.(string)
	return text, ok
}

func streamStringFrom(m map[string]interface{}, names ...string) string {
	value, ok := streamStringField(m, names...)
	if !ok {
		return ""
	}
	return strings.TrimSpace(value)
}

func streamBoolFrom(m map[string]interface{}, names ...string) bool {
	value, ok := streamField(m, names...)
	return ok && streamBoolValue(value)
}

func streamBoolValue(value interface{}) bool {
	switch typed := value.(type) {
	case bool:
		return typed
	case string:
		return strings.EqualFold(strings.TrimSpace(typed), "true") || strings.TrimSpace(typed) == "1"
	case float64:
		return typed != 0
	case int:
		return typed != 0
	case json.Number:
		return typed != "0"
	default:
		return false
	}
}

func streamNumberField(m map[string]interface{}, names ...string) (float64, bool) {
	value, ok := streamField(m, names...)
	if !ok {
		return 0, false
	}
	switch typed := value.(type) {
	case float64:
		return typed, true
	case float32:
		return float64(typed), true
	case int:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case json.Number:
		parsed, err := typed.Float64()
		return parsed, err == nil
	case string:
		var parsed float64
		if _, err := fmt.Sscanf(strings.TrimSpace(typed), "%f", &parsed); err == nil {
			return parsed, true
		}
	}
	return 0, false
}
