package proxy

import (
	"encoding/json"
	"strings"
)

// eventStreamParseOptions carries request-scoped facts that cannot be inferred
// from an upstream event stream alone.
type eventStreamParseOptions struct {
	toolInputPolicies map[string]toolInputPolicy
}

type toolInputPolicy uint8

const (
	toolInputPolicyNone toolInputPolicy = iota
	toolInputPolicyDeclaredEmpty
	toolInputPolicyClosedEmpty
)

func eventStreamParseOptionsForPayload(payload *KiroPayload) eventStreamParseOptions {
	options := eventStreamParseOptions{}
	if payload == nil || len(payload.toolInputPolicies) == 0 {
		return options
	}
	options.toolInputPolicies = make(map[string]toolInputPolicy, len(payload.toolInputPolicies))
	for name, policy := range payload.toolInputPolicies {
		options.toolInputPolicies[name] = policy
	}
	return options
}

func (o eventStreamParseOptions) allowsEmptyToolInput(name string, boundary toolUseRecoveryBoundary) bool {
	policy := o.toolInputPolicies[strings.TrimSpace(name)]
	switch boundary {
	case toolUseRecoveryExplicitToolUse:
		return policy == toolInputPolicyDeclaredEmpty || policy == toolInputPolicyClosedEmpty
	case toolUseRecoveryCleanEOF:
		return policy == toolInputPolicyClosedEmpty
	default:
		return false
	}
}

func classifyToolInputPolicy(schema interface{}) toolInputPolicy {
	object, ok := schema.(map[string]interface{})
	if !ok || object == nil {
		return toolInputPolicyNone
	}
	schemaType, exists := object["type"]
	value, ok := schemaType.(string)
	if !exists || !ok || value != "object" {
		return toolInputPolicyNone
	}
	properties, exists := object["properties"]
	propertyMap, ok := properties.(map[string]interface{})
	if !exists || !ok || len(propertyMap) != 0 {
		return toolInputPolicyNone
	}
	if required, exists := object["required"]; exists && !emptySchemaStringArray(required) {
		return toolInputPolicyNone
	}
	if minimum, exists := object["minProperties"]; exists {
		value, ok := schemaInteger(minimum)
		if !ok || value != 0 {
			return toolInputPolicyNone
		}
	}
	if maximum, exists := object["maxProperties"]; exists {
		value, ok := schemaInteger(maximum)
		if !ok || value != 0 {
			return toolInputPolicyNone
		}
	}
	for _, keyword := range []string{
		"$ref", "allOf", "anyOf", "oneOf", "not", "if", "then", "else", "const", "enum",
		"patternProperties", "propertyNames", "dependentSchemas", "dependentRequired", "dependencies", "unevaluatedProperties",
	} {
		if _, exists := object[keyword]; exists {
			return toolInputPolicyNone
		}
	}
	additional, exists := object["additionalProperties"]
	if !exists {
		return toolInputPolicyDeclaredEmpty
	}
	allowed, ok := additional.(bool)
	if ok && !allowed {
		return toolInputPolicyClosedEmpty
	}
	return toolInputPolicyNone
}

func registerToolInputPolicy(policies map[string]toolInputPolicy, name string, policy toolInputPolicy) {
	name = strings.TrimSpace(name)
	if name == "" {
		return
	}
	existing, exists := policies[name]
	if !exists {
		policies[name] = policy
		return
	}
	if existing != policy {
		policies[name] = toolInputPolicyNone
	}
}

func emptySchemaStringArray(value interface{}) bool {
	switch items := value.(type) {
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
