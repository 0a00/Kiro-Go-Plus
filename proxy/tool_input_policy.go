package proxy

import (
	"encoding/json"
	"strings"
)

// eventStreamParseOptions carries request-scoped facts that cannot be inferred
// from an upstream event stream alone.
type eventStreamParseOptions struct {
	emptyInputTools map[string]struct{}
}

func eventStreamParseOptionsForPayload(payload *KiroPayload) eventStreamParseOptions {
	options := eventStreamParseOptions{}
	if payload == nil {
		return options
	}
	context := payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext
	if context == nil {
		return options
	}
	const (
		toolSchemaZeroArguments uint8 = 1 << iota
		toolSchemaOther
	)
	shapes := make(map[string]uint8, len(context.Tools))
	for _, wrapper := range context.Tools {
		tool := wrapper.ToolSpecification
		name := strings.TrimSpace(tool.Name)
		if name == "" {
			continue
		}
		if schemaDefinesZeroArguments(tool.InputSchema.JSON) {
			shapes[name] |= toolSchemaZeroArguments
		} else {
			shapes[name] |= toolSchemaOther
		}
	}
	for name, shape := range shapes {
		if shape != toolSchemaZeroArguments {
			continue
		}
		if options.emptyInputTools == nil {
			options.emptyInputTools = make(map[string]struct{})
		}
		options.emptyInputTools[name] = struct{}{}
	}
	return options
}

func (o eventStreamParseOptions) allowsEmptyToolInput(name string) bool {
	_, ok := o.emptyInputTools[strings.TrimSpace(name)]
	return ok
}

// schemaDefinesZeroArguments deliberately recognizes only explicit object
// schemas with an empty properties map. Open, dynamic, or composed schemas may
// accept {}, but treating a missing upstream argument stream as intentional
// could execute a tool without parameters the model meant to provide.
func schemaDefinesZeroArguments(schema interface{}) bool {
	object, ok := schema.(map[string]interface{})
	if !ok || object == nil {
		return false
	}
	schemaType, exists := object["type"]
	value, ok := schemaType.(string)
	if !exists || !ok || !strings.EqualFold(strings.TrimSpace(value), "object") {
		return false
	}
	properties, exists := object["properties"]
	propertyMap, ok := properties.(map[string]interface{})
	if !exists || !ok || len(propertyMap) != 0 {
		return false
	}
	if required, exists := object["required"]; exists && !emptySchemaStringArray(required) {
		return false
	}
	if minimum, exists := object["minProperties"]; exists {
		value, ok := schemaInteger(minimum)
		if !ok || value != 0 {
			return false
		}
	}
	if maximum, exists := object["maxProperties"]; exists {
		value, ok := schemaInteger(maximum)
		if !ok || value != 0 {
			return false
		}
	}
	if additional, exists := object["additionalProperties"]; exists {
		allowed, ok := additional.(bool)
		if !ok || allowed {
			return false
		}
	}
	for _, keyword := range []string{
		"$ref", "allOf", "anyOf", "oneOf", "not", "if", "then", "else", "const", "enum",
		"patternProperties", "propertyNames", "dependentSchemas", "dependentRequired", "dependencies", "unevaluatedProperties",
	} {
		if _, exists := object[keyword]; exists {
			return false
		}
	}
	return true
}

func emptySchemaStringArray(value interface{}) bool {
	switch items := value.(type) {
	case nil:
		return true
	case []string:
		return len(items) == 0
	case []interface{}:
		return len(items) == 0
	default:
		return false
	}
}

func schemaInteger(value interface{}) (int64, bool) {
	switch number := value.(type) {
	case int:
		return int64(number), true
	case int64:
		return number, true
	case float64:
		converted := int64(number)
		return converted, float64(converted) == number
	case json.Number:
		converted, err := number.Int64()
		return converted, err == nil
	default:
		return 0, false
	}
}
