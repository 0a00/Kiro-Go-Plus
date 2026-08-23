package proxy

import (
	"strings"
	"sync"
)

const (
	effortLow    = "low"
	effortMedium = "medium"
	effortHigh   = "high"
	effortXHigh  = "xhigh"
	effortMax    = "max"
)

var effortRanks = map[string]int{
	effortLow:    0,
	effortMedium: 1,
	effortHigh:   2,
	effortXHigh:  3,
	effortMax:    4,
}

var discoveredModelMetadata sync.Map

func normalizeModelInfos(models []ModelInfo) {
	for i := range models {
		normalizeModelInfo(&models[i])
	}
}

func normalizeModelInfo(model *ModelInfo) {
	if model == nil {
		return
	}
	model.ModelId = strings.TrimSpace(model.ModelId)
	model.EffortLevels = normalizeEffortLevels(model.EffortLevels)
	model.EffortSchemaPath = strings.ToLower(strings.TrimSpace(model.EffortSchemaPath))
	if len(model.EffortLevels) == 0 {
		model.EffortLevels, model.EffortSchemaPath = extractEffortMetadata(model.AdditionalModelRequestFieldsSchema)
	}
	if model.ContextWindow > 0 {
		if model.TokenLimits == nil {
			model.TokenLimits = &ModelTokenLimits{MaxInputTokens: model.ContextWindow}
		} else if model.TokenLimits.MaxInputTokens <= 0 {
			model.TokenLimits.MaxInputTokens = model.ContextWindow
		}
	} else if model.TokenLimits != nil && model.TokenLimits.MaxInputTokens > 0 {
		model.ContextWindow = model.TokenLimits.MaxInputTokens
	}
}

func extractEffortMetadata(schema map[string]interface{}) ([]string, string) {
	paths := []struct {
		name string
		keys []string
	}{
		{name: "output_config", keys: []string{"properties", "output_config", "properties", "effort", "enum"}},
		{name: "reasoning", keys: []string{"properties", "reasoning", "properties", "effort", "enum"}},
	}
	for _, candidate := range paths {
		if levels := schemaStringList(schema, candidate.keys...); len(levels) > 0 {
			return normalizeEffortLevels(levels), candidate.name
		}
	}
	return nil, ""
}

func schemaStringList(schema map[string]interface{}, keys ...string) []string {
	if len(schema) == 0 || len(keys) == 0 {
		return nil
	}
	var current interface{} = schema
	for _, key := range keys {
		object, ok := current.(map[string]interface{})
		if !ok {
			return nil
		}
		current, ok = object[key]
		if !ok {
			return nil
		}
	}
	values, ok := current.([]interface{})
	if !ok {
		return nil
	}
	result := make([]string, 0, len(values))
	for _, value := range values {
		if text, ok := value.(string); ok {
			result = append(result, text)
		}
	}
	return result
}

func normalizeEffortLevels(levels []string) []string {
	if len(levels) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(levels))
	result := make([]string, 0, len(levels))
	for _, level := range levels {
		level = strings.ToLower(strings.TrimSpace(level))
		if level == "" {
			continue
		}
		if _, exists := seen[level]; exists {
			continue
		}
		seen[level] = struct{}{}
		result = append(result, level)
	}
	return result
}

func rememberDiscoveredModelMetadata(models []ModelInfo) {
	normalizeModelInfos(models)
	rememberDiscoveredModelTokenLimits(models)
	for _, model := range models {
		key := strings.ToLower(strings.TrimSpace(model.ModelId))
		if key == "" {
			continue
		}
		incoming := cloneModelInfo(model)
		if existingValue, ok := discoveredModelMetadata.Load(key); ok {
			existing := cloneModelInfo(existingValue.(ModelInfo))
			incoming = mergeModelInfo(existing, incoming)
		}
		discoveredModelMetadata.Store(key, incoming)
	}
}

func getDiscoveredModelMetadata(model string) (ModelInfo, bool) {
	key := strings.ToLower(strings.TrimSpace(model))
	if value, ok := discoveredModelMetadata.Load(key); ok {
		return cloneModelInfo(value.(ModelInfo)), true
	}
	if strings.HasSuffix(key, "-thinking") {
		if value, ok := discoveredModelMetadata.Load(strings.TrimSuffix(key, "-thinking")); ok {
			return cloneModelInfo(value.(ModelInfo)), true
		}
	}
	return ModelInfo{}, false
}

func cloneModelInfo(model ModelInfo) ModelInfo {
	model.Capabilities = append([]string(nil), model.Capabilities...)
	model.InputTypes = append([]string(nil), model.InputTypes...)
	model.EffortLevels = append([]string(nil), model.EffortLevels...)
	if model.TokenLimits != nil {
		limits := *model.TokenLimits
		model.TokenLimits = &limits
	}
	if model.PromptCaching != nil {
		cache := *model.PromptCaching
		if cache.SupportsPromptCaching != nil {
			supported := *cache.SupportsPromptCaching
			cache.SupportsPromptCaching = &supported
		}
		model.PromptCaching = &cache
	}
	return model
}

func (h *Handler) effortMetadataForModel(model string) ([]string, string) {
	model = normalizeKnownModelID(model)
	if h != nil {
		var cached *ModelInfo
		h.modelsCacheMu.RLock()
		for _, item := range h.cachedModels {
			if normalizeKnownModelID(item.ModelId) == model {
				copy := cloneModelInfo(item)
				cached = &copy
				break
			}
		}
		h.modelsCacheMu.RUnlock()
		if cached != nil {
			normalizeModelInfo(cached)
			if len(cached.EffortLevels) > 0 {
				path := cached.EffortSchemaPath
				if path == "" {
					path = "output_config"
				}
				return cached.EffortLevels, path
			}
		}
	}
	if discovered, ok := getDiscoveredModelMetadata(model); ok && len(discovered.EffortLevels) > 0 {
		path := discovered.EffortSchemaPath
		if path == "" {
			path = "output_config"
		}
		return discovered.EffortLevels, path
	}
	return fallbackEffortMetadata(model)
}

func fallbackEffortMetadata(model string) ([]string, string) {
	if isClaudeOpus5Model(model) {
		return []string{effortLow, effortMedium, effortHigh, effortXHigh, effortMax}, "output_config"
	}
	if isClaudeSonnet5Model(model) {
		return []string{effortHigh}, "output_config"
	}
	return nil, ""
}

func isClaudeOpus5Model(model string) bool {
	model = strings.ToLower(strings.TrimSpace(model))
	model = strings.TrimSuffix(model, "-thinking")
	return model == "claude-opus-5" || strings.HasPrefix(model, "claude-opus-5.")
}

func isClaudeSonnet5Model(model string) bool {
	model = strings.ToLower(strings.TrimSpace(model))
	model = strings.TrimSuffix(model, "-thinking")
	return model == "claude-sonnet-5" || strings.HasPrefix(model, "claude-sonnet-5.")
}

func supportsAdaptiveThinking(model string) bool {
	model = normalizeKnownModelID(model)
	model = strings.ToLower(strings.TrimSpace(strings.TrimSuffix(model, "-thinking")))
	if isClaudeOpus5Model(model) || isClaudeSonnet5Model(model) {
		return true
	}
	switch model {
	case "claude-opus-4.6", "claude-opus-4.7", "claude-opus-4.8":
		return true
	default:
		return false
	}
}

func normalizeRequestedEffort(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case effortLow:
		return effortLow
	case effortMedium:
		return effortMedium
	case effortHigh:
		return effortHigh
	case effortXHigh, "extra-high", "extra_high":
		return effortXHigh
	case effortMax, "maximum":
		return effortMax
	case "minimal", "min":
		return effortLow
	default:
		return ""
	}
}

func resolveSupportedEffort(requested string, supported []string) (string, bool) {
	requested = normalizeRequestedEffort(requested)
	if requested == "" || len(supported) == 0 {
		return "", false
	}
	supported = normalizeEffortLevels(supported)
	for _, level := range supported {
		if level == requested {
			return requested, true
		}
	}
	targetRank, valid := effortRanks[requested]
	if !valid {
		return "", false
	}
	best := ""
	bestDistance := int(^uint(0) >> 1)
	bestRank := -1
	for _, level := range supported {
		rank, ok := effortRanks[level]
		if !ok {
			continue
		}
		distance := rank - targetRank
		if distance < 0 {
			distance = -distance
		}
		if distance < bestDistance || (distance == bestDistance && rank > bestRank) {
			best = level
			bestDistance = distance
			bestRank = rank
		}
	}
	return best, best != ""
}

func defaultNativeEffort(model string) string {
	if isClaudeOpus5Model(model) || isClaudeSonnet5Model(model) {
		return effortHigh
	}
	return effortMedium
}

func requestedClaudeNativeEffort(req *ClaudeRequest, thinking bool) string {
	if req == nil {
		return ""
	}
	if req.OutputConfig != nil {
		if effort := normalizeRequestedEffort(req.OutputConfig.Effort); effort != "" {
			return effort
		}
	}
	if req.Thinking != nil {
		if effort := normalizeRequestedEffort(req.Thinking.Effort); effort != "" {
			return effort
		}
		switch strings.ToLower(strings.TrimSpace(req.Thinking.Type)) {
		case "adaptive":
			return defaultNativeEffort(req.Model)
		case "enabled", "disabled":
			return ""
		}
	}
	if thinking {
		return defaultNativeEffort(req.Model)
	}
	return ""
}

func (h *Handler) prepareClaudeNativeEffort(req *ClaudeRequest, thinking bool) {
	if req == nil {
		return
	}
	requested := requestedClaudeNativeEffort(req, thinking)
	levels, path := h.effortMetadataForModel(req.Model)
	resolved, ok := resolveSupportedEffort(requested, levels)
	if !ok {
		return
	}
	req.NativeEffort = resolved
	req.NativeEffortPath = path
}

func requestedOpenAINativeEffort(req *OpenAIRequest, thinking bool) string {
	if req == nil {
		return ""
	}
	if effort := normalizeRequestedEffort(req.ReasoningEffort); effort != "" {
		return effort
	}
	if thinking {
		return defaultNativeEffort(req.Model)
	}
	return ""
}

func (h *Handler) prepareOpenAINativeEffort(req *OpenAIRequest, thinking bool) {
	if req == nil {
		return
	}
	requested := requestedOpenAINativeEffort(req, thinking)
	levels, path := h.effortMetadataForModel(req.Model)
	resolved, ok := resolveSupportedEffort(requested, levels)
	if !ok {
		return
	}
	req.NativeEffort = resolved
	req.NativeEffortPath = path
}

func applyNativeEffort(payload *KiroPayload, path, effort string) {
	if payload == nil || effort == "" {
		return
	}
	path = strings.ToLower(strings.TrimSpace(path))
	if path != "reasoning" {
		path = "output_config"
	}
	payload.AdditionalModelRequestFields = map[string]interface{}{
		path: map[string]interface{}{"effort": effort},
	}
}

func discoveredPromptCacheMinimum(model string) int {
	metadata, ok := getDiscoveredModelMetadata(model)
	if !ok || metadata.PromptCaching == nil {
		return 0
	}
	return metadata.PromptCaching.MinimumTokensPerCacheCheckpoint
}
