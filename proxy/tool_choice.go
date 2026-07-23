package proxy

import (
	"encoding/json"
	"fmt"
	"strings"
)

type normalizedToolChoice struct {
	mode string
	name string
}

func applyOpenAIToolChoice(req *OpenAIRequest) error {
	if req == nil || len(req.ToolChoice) == 0 {
		return nil
	}
	choice, err := parseOpenAIToolChoice(req.ToolChoice)
	if err != nil {
		return err
	}
	switch choice.mode {
	case "", "auto":
		return nil
	case "none":
		req.Tools = nil
		return nil
	case "required":
		if len(req.Tools) == 0 {
			return fmt.Errorf("tool_choice=required requires at least one tool")
		}
		insertToolChoiceConstraint(req, "You must call at least one available tool before answering.")
		return nil
	case "function":
		selected := make([]OpenAITool, 0, 1)
		for _, tool := range req.Tools {
			if tool.Function.Name == choice.name {
				selected = append(selected, tool)
				break
			}
		}
		if len(selected) == 0 {
			return fmt.Errorf("tool_choice references unknown function %q", choice.name)
		}
		req.Tools = selected
		insertToolChoiceConstraint(req, fmt.Sprintf("You must call the %q tool before answering.", choice.name))
		return nil
	default:
		return fmt.Errorf("unsupported tool_choice %q", choice.mode)
	}
}

func parseOpenAIToolChoice(raw json.RawMessage) (normalizedToolChoice, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return normalizedToolChoice{mode: "auto"}, nil
	}
	if strings.HasPrefix(trimmed, "\"") {
		var mode string
		if err := json.Unmarshal(raw, &mode); err != nil {
			return normalizedToolChoice{}, fmt.Errorf("invalid tool_choice: %w", err)
		}
		mode = strings.ToLower(strings.TrimSpace(mode))
		switch mode {
		case "auto", "none", "required":
			return normalizedToolChoice{mode: mode}, nil
		default:
			return normalizedToolChoice{}, fmt.Errorf("unsupported tool_choice %q", mode)
		}
	}

	var object struct {
		Type     string `json:"type"`
		Name     string `json:"name"`
		Function *struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &object); err != nil {
		return normalizedToolChoice{}, fmt.Errorf("invalid tool_choice: %w", err)
	}
	name := strings.TrimSpace(object.Name)
	if object.Function != nil && strings.TrimSpace(object.Function.Name) != "" {
		name = strings.TrimSpace(object.Function.Name)
	}
	choiceType := strings.ToLower(strings.TrimSpace(object.Type))
	if (choiceType != "function" && choiceType != "custom" && choiceType != "tool") || name == "" {
		return normalizedToolChoice{}, fmt.Errorf("tool_choice function name is required")
	}
	return normalizedToolChoice{mode: "function", name: name}, nil
}

func insertToolChoiceConstraint(req *OpenAIRequest, constraint string) {
	insertAt := 0
	for insertAt < len(req.Messages) && req.Messages[insertAt].Role == "system" {
		insertAt++
	}
	message := OpenAIMessage{Role: "system", Content: constraint}
	req.Messages = append(req.Messages, OpenAIMessage{})
	copy(req.Messages[insertAt+1:], req.Messages[insertAt:])
	req.Messages[insertAt] = message
}
