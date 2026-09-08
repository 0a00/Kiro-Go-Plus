package main

import "testing"

func TestLoadOutputLimitRequiresOptInAndCompleteStream(t *testing.T) {
	response := apiResponse{statusCode: 200, stream: sseStats{
		terminal: true, semanticOutput: true, incomplete: true, stopReason: "max_tokens", output: []byte("EXPECTED"),
	}}
	probe := loadProbe{stream: true, expectedMarker: "EXPECTED", validation: loadValidationExact}
	if sample := classifyLoadSample(response, probe); sample.success || sample.category != "output_limit" {
		t.Fatalf("unrequested output limit accepted: %+v", sample)
	}
	probe.allowOutputLimit = true
	if sample := classifyLoadSample(response, probe); !sample.success {
		t.Fatalf("expected output limit rejected: %+v", sample)
	}
	response.stream.terminal = false
	if sample := classifyLoadSample(response, probe); sample.success {
		t.Fatal("truncated stream accepted")
	}
	response.stream.terminal = true
	response.stream.stopReason = "content_filter"
	if sample := classifyLoadSample(response, probe); sample.success {
		t.Fatal("content filter accepted as output limit")
	}
	response.stream.stopReason = "max_tokens"
	response.stream.output = []byte("WRONG")
	if sample := classifyLoadSample(response, probe); sample.success {
		t.Fatal("wrong content accepted")
	}
}
