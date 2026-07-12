package proxy

import (
	"bytes"
	"encoding/json"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"math"
	"strings"
)

const (
	defaultImageInputTokens = 100
	maxImageInputTokens     = 1600
	imageTokenPixelDivisor  = 750.0
)

func estimateApproxTokens(text string) int {
	if text == "" {
		return 0
	}

	runes := []rune(text)
	length := len(runes)
	if length == 0 {
		return 0
	}
	if length < 5 {
		return max(1, int(math.Ceil(float64(length)/3.0)))
	}

	var regularAscii, digits, symbols, nonASCII int
	for _, r := range runes {
		switch {
		case r >= 0x80:
			nonASCII++
		case r >= '0' && r <= '9':
			digits++
		case (r >= '!' && r <= '/') || (r >= ':' && r <= '@') || (r >= '[' && r <= '`') || (r >= '{' && r <= '~'):
			symbols++
		default:
			regularAscii++
		}
	}

	estimated := int(math.Ceil(
		float64(regularAscii)/4.5 +
			float64(digits)/2.0 +
			float64(symbols)/1.5 +
			float64(nonASCII)/1.5,
	))

	if estimated < 1 {
		return 1
	}
	return estimated
}

func estimateClaudeRequestInputTokens(req *ClaudeRequest) int {
	if req == nil {
		return 0
	}

	total := estimateClaudeValueTokens(req.System)

	for _, msg := range req.Messages {
		total += estimateClaudeValueTokens(msg.Content)
	}

	for _, tool := range req.Tools {
		total += estimateApproxTokens(tool.Name)
		total += estimateApproxTokens(tool.Description)
		total += estimateJSONTokens(tool.InputSchema)
	}

	return total
}

func estimateClaudeOutputTokens(content, thinkingContent string, toolUses []KiroToolUse) int {
	total := estimateApproxTokens(content)
	total += estimateApproxTokens(thinkingContent)

	for _, tu := range toolUses {
		total += estimateApproxTokens(tu.Name)
		total += estimateJSONTokens(tu.Input)
	}

	return total
}

func estimateClaudeValueTokens(v interface{}) int {
	switch value := v.(type) {
	case nil:
		return 0
	case string:
		return estimateApproxTokens(value)
	case []interface{}:
		total := 0
		for _, part := range value {
			total += estimateClaudeValueTokens(part)
		}
		return total
	case map[string]interface{}:
		if isImageContentBlock(value) {
			return estimateImageContentTokens(value)
		}
		typeName, _ := value["type"].(string)
		switch strings.ToLower(strings.TrimSpace(typeName)) {
		case "text":
			if text, ok := value["text"].(string); ok {
				return estimateApproxTokens(text)
			}
		case "thinking":
			if thinking, ok := value["thinking"].(string); ok {
				return estimateApproxTokens(thinking)
			}
		case "tool_use":
			total := 0
			if name, ok := value["name"].(string); ok {
				total += estimateApproxTokens(name)
			}
			if input, ok := value["input"]; ok {
				total += estimateJSONTokens(input)
			}
			if total > 0 {
				return total
			}
		case "tool_result":
			if content, ok := value["content"]; ok {
				return estimateClaudeValueTokens(content)
			}
		}

		total := 0
		if text, ok := value["text"].(string); ok {
			total += estimateApproxTokens(text)
		}
		if thinking, ok := value["thinking"].(string); ok {
			total += estimateApproxTokens(thinking)
		}
		if content, ok := value["content"]; ok {
			total += estimateClaudeValueTokens(content)
		}
		if total > 0 {
			return total
		}

		return estimateJSONTokens(value)
	default:
		return estimateJSONTokens(value)
	}
}

func estimateJSONTokens(v interface{}) int {
	if v == nil {
		return 0
	}

	b, err := json.Marshal(v)
	if err != nil {
		return 0
	}

	return estimateApproxTokens(string(b))
}

func estimateOpenAIRequestInputTokens(req *OpenAIRequest) int {
	if req == nil {
		return 0
	}

	total := 0

	for _, msg := range req.Messages {
		total += estimateOpenAIContentTokens(msg.Content)
		total += estimateApproxTokens(msg.ToolCallID)
		for _, tc := range msg.ToolCalls {
			total += estimateApproxTokens(tc.Function.Name)
			total += estimateApproxTokens(tc.Function.Arguments)
		}
	}

	for _, tool := range req.Tools {
		total += estimateApproxTokens(tool.Function.Name)
		total += estimateApproxTokens(tool.Function.Description)
		total += estimateJSONTokens(tool.Function.Parameters)
	}

	return total
}

func estimateOpenAIContentTokens(content interface{}) int {
	switch value := content.(type) {
	case nil:
		return 0
	case string:
		return estimateApproxTokens(value)
	case []interface{}:
		total := 0
		for _, part := range value {
			total += estimateOpenAIContentTokens(part)
		}
		return total
	case map[string]interface{}:
		if isImageContentBlock(value) {
			return estimateImageContentTokens(value)
		}
		typeName, _ := value["type"].(string)
		switch strings.ToLower(strings.TrimSpace(typeName)) {
		case "text", "input_text", "output_text":
			if text, ok := value["text"].(string); ok {
				return estimateApproxTokens(text)
			}
		case "tool_result":
			if nested, ok := value["content"]; ok {
				return estimateOpenAIContentTokens(nested)
			}
		}

		total := 0
		if text, ok := value["text"].(string); ok {
			total += estimateApproxTokens(text)
		}
		if nested, ok := value["content"]; ok {
			total += estimateOpenAIContentTokens(nested)
		}
		if total > 0 {
			return total
		}
		return estimateJSONTokens(value)
	default:
		return estimateJSONTokens(value)
	}
}

func isImageContentBlock(value map[string]interface{}) bool {
	if value == nil {
		return false
	}
	typeName, _ := value["type"].(string)
	switch strings.ToLower(strings.TrimSpace(typeName)) {
	case "image", "image_url", "input_image":
		return true
	case "file", "input_file":
		return extractImageFromClaudeBlock(value) != nil
	}
	for _, key := range []string{"image_url", "image_base64", "b64_json"} {
		if _, ok := value[key]; ok {
			return true
		}
	}
	if rawURL, ok := value["url"].(string); ok && strings.HasPrefix(strings.ToLower(strings.TrimSpace(rawURL)), "data:image/") {
		return true
	}
	if source, ok := value["source"].(map[string]interface{}); ok {
		for _, key := range []string{"media_type", "mediaType", "mime_type", "mime"} {
			if mediaType, ok := source[key].(string); ok && strings.HasPrefix(strings.ToLower(strings.TrimSpace(mediaType)), "image/") {
				return true
			}
		}
	}
	return false
}

func estimateImageContentTokens(block map[string]interface{}) int {
	kiroImage := extractImageFromClaudeBlock(block)
	if kiroImage == nil || strings.TrimSpace(kiroImage.Source.Bytes) == "" {
		return defaultImageInputTokens
	}
	decoded, err := decodeBase64Payload(kiroImage.Source.Bytes)
	if err != nil {
		return defaultImageInputTokens
	}
	config, _, err := image.DecodeConfig(bytes.NewReader(decoded))
	if err != nil || config.Width <= 0 || config.Height <= 0 {
		return defaultImageInputTokens
	}
	tokens := int(math.Ceil(float64(config.Width) * float64(config.Height) / imageTokenPixelDivisor))
	if tokens < defaultImageInputTokens {
		return defaultImageInputTokens
	}
	if tokens > maxImageInputTokens {
		return maxImageInputTokens
	}
	return tokens
}

func estimateOpenAIOutputTokens(content, reasoningContent string, toolUses []KiroToolUse) int {
	return estimateClaudeOutputTokens(content, reasoningContent, toolUses)
}
