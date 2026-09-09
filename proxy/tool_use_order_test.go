package proxy

import (
	"bytes"
	"strings"
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

func TestHandleToolUseEventUsesFinalIDForStreamedCallbacks(t *testing.T) {
	var events []string
	callback := &KiroStreamCallback{
		OnToolUseStart: func(id, name string) { events = append(events, "start:"+id+":"+name) },
		OnToolUseDelta: func(id, input string) { events = append(events, "delta:"+id+":"+input) },
		OnToolUseStop:  func(id string) { events = append(events, "stop:"+id) },
		OnToolUse:      func(tool KiroToolUse) { events = append(events, "tool:"+tool.ToolUseID) },
	}
	pending := &pendingToolUseSet{}

	if err := handleToolUseEvent(map[string]interface{}{
		"name":  "Write",
		"input": `{"path":`,
	}, pending, callback); err != nil {
		t.Fatalf("unexpected initial tool-use error: %v", err)
	}
	if err := handleToolUseEvent(map[string]interface{}{
		"toolUseId": "toolu_final",
		"name":      "Write",
		"input":     `"README.md"}`,
		"stop":      true,
	}, pending, callback); err != nil {
		t.Fatalf("unexpected completed tool-use error: %v", err)
	}

	for _, event := range events {
		if strings.Contains(event, "toolu_") && !strings.Contains(event, "toolu_final") {
			t.Fatalf("generated ID leaked into streamed callback: %q; events=%#v", event, events)
		}
	}
	if len(events) != 5 || events[0] != "start:toolu_final:Write" ||
		events[1] != `delta:toolu_final:{"path":` ||
		events[2] != `delta:toolu_final:"README.md"}` ||
		events[3] != "stop:toolu_final" || events[4] != "tool:toolu_final" {
		t.Fatalf("unexpected streamed callback sequence: %#v", events)
	}
}
