package proxy

import (
	"encoding/json"
	"fmt"
	"strings"
)

type responsesInputResult struct {
	Messages               []OpenAIMessage
	AdditionalTools        []OpenAITool
	AdditionalToolsPresent bool
}

func parseResponsesInput(raw json.RawMessage) ([]OpenAIMessage, error) {
	result, err := parseResponsesInputWithTools(raw)
	return result.Messages, err
}

func parseResponsesInputWithTools(raw json.RawMessage) (responsesInputResult, error) {
	if len(raw) == 0 {
		return responsesInputResult{}, nil
	}

	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return responsesInputResult{}, nil
	}

	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return responsesInputResult{}, fmt.Errorf("invalid input string: %w", err)
		}
		if strings.TrimSpace(s) == "" {
			return responsesInputResult{}, nil
		}
		return responsesInputResult{Messages: []OpenAIMessage{{Role: "user", Content: s}}}, nil
	}

	if trimmed[0] == '[' {
		var items []json.RawMessage
		if err := json.Unmarshal(raw, &items); err != nil {
			return responsesInputResult{}, fmt.Errorf("invalid input array: %w", err)
		}
		return convertResponsesInputItemsWithTools(items)
	}

	if trimmed[0] == '{' {
		return convertResponsesInputItemsWithTools([]json.RawMessage{raw})
	}

	return responsesInputResult{}, fmt.Errorf("unsupported input shape")
}

func convertResponsesInputItems(items []json.RawMessage) ([]OpenAIMessage, error) {
	result, err := convertResponsesInputItemsWithTools(items)
	return result.Messages, err
}

func convertResponsesInputItemsWithTools(items []json.RawMessage) (responsesInputResult, error) {
	messages := make([]OpenAIMessage, 0, len(items))
	pendingUserParts := []interface{}{}
	result := responsesInputResult{}

	flushPendingUser := func() {
		if len(pendingUserParts) == 0 {
			return
		}
		messages = append(messages, OpenAIMessage{
			Role:    "user",
			Content: pendingUserParts,
		})
		pendingUserParts = nil
	}

	for index, item := range items {
		var obj map[string]interface{}
		if err := json.Unmarshal(item, &obj); err != nil {
			return responsesInputResult{}, fmt.Errorf("input item %d is invalid JSON: %w", index, err)
		}
		if obj == nil {
			return responsesInputResult{}, fmt.Errorf("input item %d must be an object", index)
		}

		typ, _ := obj["type"].(string)
		role, _ := obj["role"].(string)

		switch {
		case typ == "message" || (typ == "" && role != ""):
			flushPendingUser()
			msg, err := buildMessageFromInputItem(obj, role)
			if err != nil {
				return responsesInputResult{}, fmt.Errorf("input item %d: %w", index, err)
			}
			if msg == nil {
				return responsesInputResult{}, fmt.Errorf("input item %d contains an invalid message", index)
			}
			messages = append(messages, *msg)

		case typ == "function_call_output" || typ == "tool_result":
			flushPendingUser()
			callID, _ := obj["call_id"].(string)
			if callID == "" {
				callID, _ = obj["tool_call_id"].(string)
			}
			if strings.TrimSpace(callID) == "" {
				return responsesInputResult{}, fmt.Errorf("input item %d function_call_output requires call_id", index)
			}
			out := stringifyArbitrary(obj["output"])
			if out == "" {
				out = stringifyArbitrary(obj["content"])
			}
			messages = append(messages, OpenAIMessage{
				Role:       "tool",
				Content:    out,
				ToolCallID: callID,
			})

		case typ == "function_call" || typ == "custom_tool_call":
			flushPendingUser()
			tc := ToolCall{
				ID:   stringField(obj, "call_id", "id"),
				Type: "function",
			}
			tc.Function.Name, _ = obj["name"].(string)
			if typ == "custom_tool_call" {
				rawInput := stringifyArbitrary(obj["input"])
				encoded, _ := json.Marshal(map[string]interface{}{"input": rawInput})
				tc.Function.Arguments = string(encoded)
			} else {
				tc.Function.Arguments = stringifyArbitrary(obj["arguments"])
			}
			if strings.TrimSpace(tc.ID) == "" || strings.TrimSpace(tc.Function.Name) == "" {
				return responsesInputResult{}, fmt.Errorf("input item %d %s requires call_id and name", index, typ)
			}
			appendResponsesAssistantToolCall(&messages, tc)

		case typ == "custom_tool_call_output":
			flushPendingUser()
			callID := stringField(obj, "call_id", "tool_call_id")
			if strings.TrimSpace(callID) == "" {
				return responsesInputResult{}, fmt.Errorf("input item %d custom_tool_call_output requires call_id", index)
			}
			out := stringifyArbitrary(obj["output"])
			if out == "" {
				out = stringifyArbitrary(obj["content"])
			}
			messages = append(messages, OpenAIMessage{Role: "tool", Content: out, ToolCallID: callID})

		case typ == "additional_tools":
			result.AdditionalToolsPresent = true
			var envelope struct {
				Tools []OpenAITool `json:"tools"`
			}
			if err := json.Unmarshal(item, &envelope); err != nil {
				return responsesInputResult{}, fmt.Errorf("input item %d has invalid additional_tools: %w", index, err)
			}
			result.AdditionalTools = append(result.AdditionalTools, envelope.Tools...)

		case typ == "reasoning" || typ == "item_reference":
			// Reasoning signatures cannot be replayed safely through Kiro, and
			// item_reference is resolved by previous_response_id expansion.
			continue

		case typ == "input_text" || typ == "text":
			text, _ := obj["text"].(string)
			if text == "" {
				return responsesInputResult{}, fmt.Errorf("input item %d requires non-empty text", index)
			}
			pendingUserParts = append(pendingUserParts, map[string]interface{}{
				"type": "input_text",
				"text": text,
			})

		case typ == "input_image", typ == "image", typ == "image_url":
			pendingUserParts = append(pendingUserParts, map[string]interface{}(obj))

		case typ == "output_text":
			flushPendingUser()
			text, _ := obj["text"].(string)
			if text == "" {
				return responsesInputResult{}, fmt.Errorf("input item %d requires non-empty output text", index)
			}
			messages = append(messages, OpenAIMessage{Role: "assistant", Content: text})

		default:
			if role != "" {
				flushPendingUser()
				msg, err := buildMessageFromInputItem(obj, role)
				if err != nil {
					return responsesInputResult{}, fmt.Errorf("input item %d: %w", index, err)
				}
				if msg == nil {
					return responsesInputResult{}, fmt.Errorf("input item %d contains an invalid message", index)
				}
				messages = append(messages, *msg)
				continue
			}
			return responsesInputResult{}, fmt.Errorf("input item %d has unsupported type %q", index, typ)
		}
	}

	flushPendingUser()
	result.Messages = messages
	return result, nil
}

func appendResponsesAssistantToolCall(messages *[]OpenAIMessage, toolCall ToolCall) {
	if messages == nil {
		return
	}
	if n := len(*messages); n > 0 &&
		(*messages)[n-1].Role == "assistant" &&
		len((*messages)[n-1].ToolCalls) > 0 &&
		strings.TrimSpace(extractOpenAIMessageText((*messages)[n-1].Content)) == "" {
		(*messages)[n-1].ToolCalls = append((*messages)[n-1].ToolCalls, toolCall)
		return
	}
	*messages = append(*messages, OpenAIMessage{Role: "assistant", Content: "", ToolCalls: []ToolCall{toolCall}})
}

func buildMessageFromInputItem(obj map[string]interface{}, role string) (*OpenAIMessage, error) {
	if role == "" {
		role = "user"
	}
	newMessage := func(content interface{}) (*OpenAIMessage, error) {
		message := &OpenAIMessage{Role: role, Content: content}
		message.Name, _ = obj["name"].(string)
		message.ToolCallID = stringField(obj, "tool_call_id", "call_id")
		if err := normalizeOpenAIMessageRole(message); err != nil {
			return nil, err
		}
		return message, nil
	}

	if content, ok := obj["content"]; ok {
		switch v := content.(type) {
		case string:
			return newMessage(v)
		case []interface{}:
			parts := make([]interface{}, 0, len(v))
			textOnly := strings.Builder{}
			anyNonText := false
			for _, p := range v {
				part, ok := p.(map[string]interface{})
				if !ok {
					continue
				}
				ptype, _ := part["type"].(string)
				switch ptype {
				case "input_text", "text":
					if t, ok := part["text"].(string); ok {
						textOnly.WriteString(t)
						parts = append(parts, map[string]interface{}{"type": "input_text", "text": t})
					}
				case "output_text":
					if t, ok := part["text"].(string); ok {
						textOnly.WriteString(t)
						parts = append(parts, map[string]interface{}{"type": "input_text", "text": t})
					}
				case "input_image", "image", "image_url":
					anyNonText = true
					parts = append(parts, part)
				default:
					if t, ok := part["text"].(string); ok && t != "" {
						textOnly.WriteString(t)
						parts = append(parts, map[string]interface{}{"type": "input_text", "text": t})
					}
				}
			}
			if !anyNonText {
				return newMessage(textOnly.String())
			}
			return newMessage(parts)
		case map[string]interface{}:
			return buildMessageFromInputItem(v, role)
		}
	}

	if text, ok := obj["text"].(string); ok && text != "" {
		return newMessage(text)
	}

	return nil, nil
}

func stringifyArbitrary(v interface{}) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return ""
		}
		return string(b)
	}
}

func stringField(obj map[string]interface{}, keys ...string) string {
	for _, k := range keys {
		if v, ok := obj[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}
