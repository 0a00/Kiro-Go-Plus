package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strings"
)

const maxBaselineBytes = 16 << 20

func (r *runner) compareLoadBaseline(path string) error {
	if r == nil {
		return fmt.Errorf("runner is nil")
	}
	path = strings.TrimSpace(path)
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if len(data) > maxBaselineBytes {
		return fmt.Errorf("baseline exceeds %d bytes", maxBaselineBytes)
	}
	var baseline devReport
	if err := json.Unmarshal(data, &baseline); err != nil {
		return err
	}
	if err := validateLoadBaselineMetadata(r.opts, baseline); err != nil {
		return err
	}
	byKey := make(map[string]scenarioResult)
	for _, result := range baseline.Results {
		if comparableLoadResult(result) {
			byKey[loadResultKey(result)] = result
		}
	}
	r.baselineCompared = true
	r.baselineRegressions = 0
	r.baselineMissing = 0
	currentKeys := make(map[string]struct{})
	for index := range r.results {
		current := &r.results[index]
		if !comparableLoadResult(*current) {
			continue
		}
		key := loadResultKey(*current)
		currentKeys[key] = struct{}{}
		previous, ok := byKey[key]
		if !ok {
			current.Status = statusFail
			current.Detail = compactDetail(current.Detail + "; baseline missing for this load profile")
			r.baselineMissing++
			continue
		}
		regressions := compareLoadResult(*current, previous, r.opts.baselineTolerancePercent)
		if len(regressions) == 0 {
			current.Detail = compactDetail(current.Detail + "; baseline=within-tolerance")
			continue
		}
		current.Status = statusFail
		current.ThresholdFailures = append(current.ThresholdFailures, regressions...)
		current.Detail = compactDetail(current.Detail + "; baseline_regressions=" + strings.Join(regressions, " | "))
		r.baselineRegressions++
	}
	for key := range byKey {
		if _, exists := currentKeys[key]; !exists {
			r.baselineMissing++
		}
	}
	return nil
}

func validateLoadBaselineMetadata(opts options, baseline devReport) error {
	if baseline.Suite != "load" && baseline.Suite != "staircase" && baseline.Suite != "soak" {
		return fmt.Errorf("baseline suite %q is not a load suite", baseline.Suite)
	}
	if baseline.Suite != opts.suite {
		return fmt.Errorf("baseline suite %q does not match current suite %q", baseline.Suite, opts.suite)
	}
	if baseline.LoadProfile == "" || baseline.LoadPattern == "" || baseline.LoadMaxTokens <= 0 || baseline.Concurrency <= 0 || baseline.Requests <= 0 {
		return fmt.Errorf("baseline does not contain load metadata; generate it with the current devcheck")
	}
	mismatches := make([]string, 0, 8)
	if baseline.LoadProfile != opts.loadProfile {
		mismatches = append(mismatches, "load profile")
	}
	if baseline.LoadPattern != opts.loadPattern {
		mismatches = append(mismatches, "load pattern")
	}
	if baseline.LoadMaxTokens != opts.loadMaxTokens {
		mismatches = append(mismatches, "load max tokens")
	}
	if baseline.Concurrency != opts.concurrency {
		mismatches = append(mismatches, "concurrency")
	}
	if baseline.Requests != opts.requests {
		mismatches = append(mismatches, "request count")
	}
	if !baselineFloatEqual(baseline.TargetRPS, opts.targetRPS) {
		mismatches = append(mismatches, "target RPS")
	}
	if baseline.RampMillis != opts.rampDuration.Milliseconds() {
		mismatches = append(mismatches, "ramp duration")
	}
	if baseline.WarmupRequests != opts.warmupRequests {
		mismatches = append(mismatches, "warmup requests")
	}
	switch opts.suite {
	case "staircase":
		if !sameIntSlice(baseline.ConcurrencyLevels, opts.concurrencySteps) {
			mismatches = append(mismatches, "staircase concurrency levels")
		}
		if baseline.StaircaseHoldMillis != opts.staircaseHold.Milliseconds() {
			mismatches = append(mismatches, "staircase hold")
		}
		if baseline.StaircaseCooldownMillis != opts.staircaseCooldown.Milliseconds() {
			mismatches = append(mismatches, "staircase cooldown")
		}
		if baseline.StaircaseMaxRequests != opts.staircaseMaxRequests {
			mismatches = append(mismatches, "staircase request cap")
		}
	case "soak":
		if baseline.SoakMillis != opts.soakDuration.Milliseconds() {
			mismatches = append(mismatches, "soak duration")
		}
		if baseline.SoakMaxRequests != opts.soakMaxRequests {
			mismatches = append(mismatches, "soak request cap")
		}
		if baseline.SoakTokenBudget != opts.soakTokenBudget {
			mismatches = append(mismatches, "soak token budget")
		}
	}
	if len(mismatches) > 0 {
		return fmt.Errorf("baseline load settings do not match current settings: %s", strings.Join(mismatches, ", "))
	}
	return nil
}

func sameIntSlice(left, right []int) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func baselineFloatEqual(left, right float64) bool {
	if left == right {
		return true
	}
	if math.IsNaN(left) || math.IsNaN(right) || math.IsInf(left, 0) || math.IsInf(right, 0) {
		return false
	}
	scale := math.Max(1, math.Max(math.Abs(left), math.Abs(right)))
	return math.Abs(left-right) <= 1e-9*scale
}

func comparableLoadResult(result scenarioResult) bool {
	return result.Protocol == "load" && result.Requests > 0
}

func loadResultKey(result scenarioResult) string {
	return strings.Join([]string{result.Name, result.Model, result.Protocol}, "\x00")
}

func compareLoadResult(current, baseline scenarioResult, tolerancePercent float64) []string {
	violations := make([]string, 0, 4)
	if baseline.Requests > 0 && current.Requests != baseline.Requests {
		violations = append(violations, fmt.Sprintf("baseline requests %d -> %d", baseline.Requests, current.Requests))
	}
	if baseline.P95Millis > 0 && current.P95Millis > relativeLimit(baseline.P95Millis, tolerancePercent) {
		violations = append(violations, fmt.Sprintf("baseline p95 %dms -> %dms", baseline.P95Millis, current.P95Millis))
	}
	if baseline.P95Millis > 0 && current.P95Millis == 0 {
		violations = append(violations, "baseline p95 became unavailable")
	}
	if baseline.TTFTP95Millis > 0 && current.TTFTP95Millis > relativeLimit(baseline.TTFTP95Millis, tolerancePercent) {
		violations = append(violations, fmt.Sprintf("baseline ttft_p95 %dms -> %dms", baseline.TTFTP95Millis, current.TTFTP95Millis))
	}
	if baseline.TTFTP95Millis > 0 && current.TTFTP95Millis == 0 {
		violations = append(violations, "baseline ttft_p95 became unavailable")
	}
	if baseline.SuccessRate > 0 && current.SuccessRate < relativeSuccessFloor(baseline.SuccessRate, tolerancePercent) {
		violations = append(violations, fmt.Sprintf("baseline success_rate %.1f%% -> %.1f%%", baseline.SuccessRate, current.SuccessRate))
	}
	return violations
}

func relativeSuccessFloor(value, tolerancePercent float64) float64 {
	if value <= 0 {
		return 0
	}
	if tolerancePercent < 0 {
		tolerancePercent = 0
	}
	if tolerancePercent > 100 {
		tolerancePercent = 100
	}
	return value * (1 - tolerancePercent/100)
}

func relativeLimit(value int64, tolerancePercent float64) int64 {
	if value <= 0 {
		return 0
	}
	if tolerancePercent < 0 {
		tolerancePercent = 0
	}
	return value + int64(float64(value)*tolerancePercent/100)
}
