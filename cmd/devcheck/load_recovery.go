package main

import "context"

func (r *runner) runPostLoadRecovery(parent context.Context) {
	if !r.opts.postLoadRecovery {
		return
	}
	if parent == nil {
		parent = context.Background()
	}
	if parent.Err() != nil {
		r.add(scenarioResult{Name: "post-load-recovery", Status: statusSkip, Protocol: "load", Model: r.model, Detail: "parent context ended before recovery verification"})
		return
	}
	healthCtx, healthCancel := r.scenarioContext(parent)
	health := r.get(healthCtx, "/health", false)
	healthCancel()
	probe := buildLoadProbe(r.model, 2_000_000, min(max(r.effectiveLoadMaxTokens(), 16), 32))
	requestCtx, requestCancel := r.scenarioContext(parent)
	response := r.post(requestCtx, probe.path, probe.payload, true, probe.stream)
	requestCancel()
	sample := classifyLoadSample(response, probe)
	result := responseScenarioResult("post-load-recovery", "load", r.model, response, false)
	result.TotalMillis += health.total.Milliseconds()
	switch {
	case !validJSONResponse(health):
		result.Status = statusFail
		result.Detail = "health check failed after load: " + responseErrorDetail(health)
	case !sample.success:
		result.Status = statusFail
		result.Detail = "deterministic request failed after load: " + sample.category
	default:
		result.Status = statusPass
		result.Detail = "health and deterministic request recovered after load"
	}
	r.add(result)
}
