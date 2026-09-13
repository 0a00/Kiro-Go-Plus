package proxy

import "time"

// openAIStreamUsageMode controls the compatibility boundary for Chat
// Completions streaming. A missing stream_options field keeps the historical
// terminal usage frame; an explicit option follows the official contract.
type openAIStreamUsageMode uint8

const (
	openAIStreamUsageLegacy openAIStreamUsageMode = iota
	openAIStreamUsageOmit
	openAIStreamUsageInclude
)

func resolveOpenAIStreamUsageMode(options *OpenAIStreamOptions) openAIStreamUsageMode {
	if options == nil || options.IncludeUsage == nil {
		return openAIStreamUsageLegacy
	}
	if *options.IncludeUsage {
		return openAIStreamUsageInclude
	}
	return openAIStreamUsageOmit
}

func applyOpenAIStreamUsageNull(chunk map[string]interface{}, mode openAIStreamUsageMode) {
	if mode == openAIStreamUsageInclude && chunk != nil {
		chunk["usage"] = nil
	}
}

func buildOpenAIStreamTerminalChunks(id, model string, finishReason string, usage map[string]interface{}, mode openAIStreamUsageMode) []map[string]interface{} {
	created := time.Now().Unix()
	finish := map[string]interface{}{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   model,
		"choices": []map[string]interface{}{{
			"index":         0,
			"delta":         map[string]interface{}{},
			"finish_reason": finishReason,
		}},
	}
	switch mode {
	case openAIStreamUsageLegacy:
		finish["usage"] = usage
		return []map[string]interface{}{finish}
	case openAIStreamUsageInclude:
		finish["usage"] = nil
		usageChunk := map[string]interface{}{
			"id":      id,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   model,
			"choices": []interface{}{},
			"usage":   usage,
		}
		return []map[string]interface{}{finish, usageChunk}
	default:
		return []map[string]interface{}{finish}
	}
}
