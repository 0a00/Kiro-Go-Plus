package proxy

import (
	"context"
	"encoding/json"
	"kiro-go/config"
	"sort"
	"strings"
	"sync"
)

// requestModelVisibility keeps the client-facing model stable while the
// gateway may use one or more internal models for a request. The state is
// stored behind a pointer in context so all protocol subflows share it.
type requestModelVisibility struct {
	mu             sync.RWMutex
	requestedModel string
	routeApplied   bool
	internalModels map[string]string
}

type requestModelVisibilityContextKey struct{}

func withRequestedModel(ctx context.Context, model string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if state := requestModelVisibilityFromContext(ctx); state != nil {
		state.setRequestedModel(model)
		return ctx
	}
	state := &requestModelVisibility{
		requestedModel: strings.TrimSpace(model),
		internalModels: make(map[string]string),
	}
	return context.WithValue(ctx, requestModelVisibilityContextKey{}, state)
}

func requestModelVisibilityFromContext(ctx context.Context) *requestModelVisibility {
	if ctx == nil {
		return nil
	}
	state, _ := ctx.Value(requestModelVisibilityContextKey{}).(*requestModelVisibility)
	return state
}

func (s *requestModelVisibility) setRequestedModel(model string) {
	if s == nil {
		return
	}
	model = strings.TrimSpace(model)
	s.mu.Lock()
	if s.requestedModel == "" {
		s.requestedModel = model
	}
	s.mu.Unlock()
}

// markModelRoute records an internal route target. It deliberately does not
// alter the payload or request model; it only controls client-facing metadata
// and error text.
func markModelRoute(ctx context.Context, model string) {
	state := requestModelVisibilityFromContext(ctx)
	if state == nil {
		return
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return
	}
	state.mu.Lock()
	state.routeApplied = true
	if state.internalModels == nil {
		state.internalModels = make(map[string]string)
	}
	state.addInternalModelLocked(model)
	state.mu.Unlock()
}

func (s *requestModelVisibility) addInternalModelLocked(model string) {
	for _, value := range modelVisibilityAliases(model) {
		value = strings.TrimSpace(value)
		if value != "" {
			s.internalModels[strings.ToLower(value)] = value
		}
	}
}

func modelVisibilityAliases(model string) []string {
	model = strings.TrimSpace(model)
	if model == "" {
		return nil
	}
	values := make([]string, 0, 10)
	seen := make(map[string]struct{})
	add := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		key := strings.ToLower(value)
		if _, exists := seen[key]; exists {
			return
		}
		seen[key] = struct{}{}
		values = append(values, value)
	}
	add(model)
	add(exposedModelID(model))
	// A client-facing model may use the official dash spelling while Kiro
	// errors use the gateway's dot spelling, or the reverse. Keep both forms
	// in the private replacement set.
	add(modelIDForAPI(model, true))
	add(modelIDForAPI(model, false))

	base, thinking := ParseModelAndThinking(model, config.GetThinkingConfig().Suffix)
	if base != "" && !strings.EqualFold(base, model) {
		add(base)
		add(modelIDForAPI(base, true))
		add(modelIDForAPI(base, false))
		if thinking {
			suffixes := []string{config.GetThinkingConfig().Suffix, "-thinking", ".thinking"}
			for _, suffix := range suffixes {
				if suffix == "" {
					continue
				}
				add(base + suffix)
				add(modelIDForAPI(base, true) + suffix)
				add(modelIDForAPI(base, false) + suffix)
			}
		}
	}
	return values
}

func (s *requestModelVisibility) publicModel(fallback string) string {
	if s == nil {
		return exposedModelID(fallback)
	}
	s.mu.RLock()
	requested := s.requestedModel
	routed := s.routeApplied
	s.mu.RUnlock()
	if routed && strings.TrimSpace(requested) != "" {
		return exposedModelID(requested)
	}
	return exposedModelID(fallback)
}

func exposedRequestModelForContext(ctx context.Context, fallback string) string {
	if state := requestModelVisibilityFromContext(ctx); state != nil {
		return state.publicModel(fallback)
	}
	return exposedModelID(fallback)
}

func publicErrorMessage(ctx context.Context, err error) string {
	if err == nil {
		return ""
	}
	return publicErrorText(ctx, diagnosticErrorMessage(err))
}

func publicErrorText(ctx context.Context, message string) string {
	state := requestModelVisibilityFromContext(ctx)
	if state == nil || strings.TrimSpace(message) == "" {
		return message
	}
	state.mu.RLock()
	requested := strings.TrimSpace(state.requestedModel)
	models := make([]string, 0, len(state.internalModels))
	for _, model := range state.internalModels {
		models = append(models, model)
	}
	state.mu.RUnlock()
	if requested == "" || len(models) == 0 {
		return message
	}

	publicModel := exposedModelID(requested)
	// Replace longer IDs first so a shorter model ID cannot partially consume a
	// dated/thinking variant. Matching is case-insensitive because upstream
	// errors are not consistent about model-name casing.
	sort.SliceStable(models, func(i, j int) bool {
		return len(models[i]) > len(models[j])
	})
	for _, model := range models {
		if strings.EqualFold(model, requested) || strings.EqualFold(model, publicModel) {
			continue
		}
		message = replaceFold(message, model, publicModel)
	}
	return message
}

func replaceFold(value, old, replacement string) string {
	old = strings.TrimSpace(old)
	if old == "" {
		return value
	}
	lowerValue := strings.ToLower(value)
	lowerOld := strings.ToLower(old)
	if !strings.Contains(lowerValue, lowerOld) {
		return value
	}
	var builder strings.Builder
	builder.Grow(len(value))
	from := 0
	for from < len(value) {
		relative := strings.Index(lowerValue[from:], lowerOld)
		if relative < 0 {
			builder.WriteString(value[from:])
			break
		}
		index := from + relative
		builder.WriteString(value[from:index])
		builder.WriteString(replacement)
		from = index + len(old)
	}
	return builder.String()
}

func publicVisibilityContext(ctx context.Context, requested, internal string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if strings.TrimSpace(requested) == "" || strings.TrimSpace(internal) == "" {
		return ctx
	}
	ctx = withRequestedModel(ctx, requested)
	markModelRoute(ctx, internal)
	return ctx
}

func publicRequestLogEntry(ctx context.Context, entry requestLogEntry) requestLogEntry {
	if strings.TrimSpace(entry.ModelFallbackFrom) != "" {
		ctx = publicVisibilityContext(ctx, entry.ModelFallbackFrom, entry.ModelFallbackTo)
	}
	entry.Model = exposedRequestModelForContext(ctx, entry.Model)
	entry.Error = publicErrorText(ctx, entry.Error)
	if strings.TrimSpace(entry.ModelFallbackFrom) != "" {
		entry.Model = exposedModelID(entry.ModelFallbackFrom)
	}
	return entry
}

func publicRequestDetail(ctx context.Context, detail requestDetail) requestDetail {
	requested := requestDetailRequestedModel(detail.Request.BodyJSON)
	if state := requestModelVisibilityFromContext(ctx); state == nil || !state.isRouted() {
		if requested != "" && detail.Model != "" && !publicModelsEqual(requested, detail.Model) {
			ctx = publicVisibilityContext(ctx, requested, detail.Model)
		}
	}
	detail.Model = exposedRequestModelForContext(ctx, detail.Model)
	detail.Response.Error = publicErrorText(ctx, detail.Response.Error)
	detail.Response.TruncationReason = publicErrorText(ctx, detail.Response.TruncationReason)
	for i := range detail.Attempts {
		detail.Attempts[i].Error = publicErrorText(ctx, detail.Attempts[i].Error)
	}
	return detail
}

func (s *requestModelVisibility) isRouted() bool {
	if s == nil {
		return false
	}
	s.mu.RLock()
	routed := s.routeApplied
	s.mu.RUnlock()
	return routed
}

func publicModelsEqual(left, right string) bool {
	left = strings.TrimSpace(left)
	right = strings.TrimSpace(right)
	if left == "" || right == "" {
		return left == right
	}
	return strings.EqualFold(exposedModelID(left), exposedModelID(right))
}

func requestDetailRequestedModel(body string) string {
	if strings.TrimSpace(body) == "" {
		return ""
	}
	var envelope struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal([]byte(body), &envelope); err != nil {
		return ""
	}
	return strings.TrimSpace(envelope.Model)
}
