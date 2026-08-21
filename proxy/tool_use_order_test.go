package proxy

import (
	"bytes"
	"testing"
)

func TestParseEventStreamPreservesInterleavedToolInputsAndOrder(t *testing.T) {
	var stream bytes.Buffer
	for _, event := range []map[string]interface{}{
		{"toolUseId": "toolu_a", "name": "lookup", "input": `{"query":"a`},
		{"toolUseId": "toolu_b", "name": "lookup", "input": `{"query":"b`},
		{"toolUseId": "toolu_b", "input": `2"}`, "stop": true},
		{"toolUseId": "toolu_a", "input": `1"}`, "stop": true},
	} {
		stream.Write(awsEventStreamFrame(t, "toolUseEvent", event))
	}

	var tools []KiroToolUse
	err := parseEventStream(bytes.NewReader(stream.Bytes()), &KiroStreamCallback{
		OnToolUse: func(tool KiroToolUse) { tools = append(tools, tool) },
	})
	if err != nil {
		t.Fatalf("parse interleaved tools: %v", err)
	}
	if len(tools) != 2 || tools[0].ToolUseID != "toolu_a" || tools[1].ToolUseID != "toolu_b" {
		t.Fatalf("tool order changed: %#v", tools)
	}
	if tools[0].Input["query"] != "a1" || tools[1].Input["query"] != "b2" {
		t.Fatalf("tool arguments crossed calls: %#v", tools)
	}
}

func TestParseEventStreamIDLessContinuationUsesLatestTool(t *testing.T) {
	var stream bytes.Buffer
	for _, event := range []map[string]interface{}{
		{"toolUseId": "toolu_1", "name": "lookup", "input": `{"query":"a`},
		{"input": `b`},
		{"input": `c"}`, "stop": true},
	} {
		stream.Write(awsEventStreamFrame(t, "toolUseEvent", event))
	}

	var tools []KiroToolUse
	if err := parseEventStream(bytes.NewReader(stream.Bytes()), &KiroStreamCallback{
		OnToolUse: func(tool KiroToolUse) { tools = append(tools, tool) },
	}); err != nil {
		t.Fatalf("parse id-less continuation: %v", err)
	}
	if len(tools) != 1 || tools[0].Input["query"] != "abc" {
		t.Fatalf("continuation was not assembled: %#v", tools)
	}
}

func TestParseEventStreamRekeysGeneratedToolWithoutReordering(t *testing.T) {
	var stream bytes.Buffer
	for _, event := range []map[string]interface{}{
		{"name": "first", "input": `{"value":`},
		{"toolUseId": "toolu_first", "name": "first", "input": `1}`},
		{"toolUseId": "toolu_second", "name": "second", "input": `{"value":2}`},
	} {
		stream.Write(awsEventStreamFrame(t, "toolUseEvent", event))
	}

	var tools []KiroToolUse
	if err := parseEventStream(bytes.NewReader(stream.Bytes()), &KiroStreamCallback{
		OnToolUse: func(tool KiroToolUse) { tools = append(tools, tool) },
	}); err != nil {
		t.Fatalf("parse rekeyed tools: %v", err)
	}
	if len(tools) != 2 || tools[0].ToolUseID != "toolu_first" || tools[1].ToolUseID != "toolu_second" {
		t.Fatalf("rekey changed arrival order: %#v", tools)
	}
}
