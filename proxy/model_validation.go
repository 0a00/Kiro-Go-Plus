package proxy

import (
	"kiro-go/config"
	"strings"
)

var builtInKiroModels = map[string]struct{}{
	"claude-sonnet-5":   {},
	"claude-opus-4.8":   {},
	"claude-opus-4.7":   {},
	"claude-opus-4.6":   {},
	"claude-opus-4.5":   {},
	"claude-sonnet-4.6": {},
	"claude-sonnet-4.5": {},
	"claude-sonnet-4":   {},
	"claude-haiku-4.5":  {},
}

func normalizeKnownModelID(model string) string {
	return strings.ToLower(strings.TrimSpace(MapModel(model)))
}

func (h *Handler) requestedModelAvailable(requested, actual string) bool {
	requested = strings.TrimSpace(requested)
	actual = normalizeKnownModelID(actual)
	if requested == "" || actual == "" {
		return false
	}
	if strings.EqualFold(requested, "auto") || strings.EqualFold(actual, "auto") {
		return true
	}
	if _, ok := config.ResolveConfiguredModel(requested); ok {
		return true
	}
	if _, ok := config.GetConfiguredModelMetadata(actual); ok {
		return true
	}
	// ListAvailableModels is account- and endpoint-dependent and can omit hidden
	// models that are still accepted by Kiro. Treat the built-in registry as the
	// supported protocol surface, then use the live cache for newly discovered
	// model IDs.
	if _, ok := builtInKiroModels[actual]; ok {
		return true
	}

	if h != nil {
		h.modelsCacheMu.RLock()
		cached := append([]ModelInfo(nil), h.cachedModels...)
		h.modelsCacheMu.RUnlock()
		if len(cached) > 0 {
			for _, model := range cached {
				if normalizeKnownModelID(model.ModelId) == actual {
					return true
				}
			}
			return false
		}
	}

	return false
}

func dedupeModelResponse(models []map[string]interface{}) []map[string]interface{} {
	seen := make(map[string]struct{}, len(models))
	out := make([]map[string]interface{}, 0, len(models))
	for _, model := range models {
		id, _ := model["id"].(string)
		key := strings.ToLower(strings.TrimSpace(id))
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, model)
	}
	return out
}
