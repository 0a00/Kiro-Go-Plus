package proxy

import (
	"bytes"
	"reflect"
	"testing"
)

func TestStrictMeaningfulStreamRejectsThinkingAndStructuralTail(t *testing.T) {
	var received []string
	wrapper, gate := wrapMeaningfulStreamCallback(&KiroStreamCallback{
		OnText: func(text string, thinking bool) {
			received = append(received, text)
		},
		OnComplete: func(int, int) {
			received = append(received, "complete")
		},
	}, nil, true, false, false, false)

	wrapper.OnText("long hidden reasoning", true)
	wrapper.OnText("}", false)
	wrapper.OnComplete(10, 20)

	if !gate.hasActivity() || gate.hasActionableOutput() {
		t.Fatalf("unexpected gate state: activity=%v actionable=%v", gate.hasActivity(), gate.hasActionableOutput())
	}
	if len(received) != 0 {
		t.Fatalf("invalid response leaked to downstream: %#v", received)
	}
}

func TestMeaningfulStreamProgressDoesNotCountAsActivity(t *testing.T) {
	activityCalls := 0
	progressCalls := 0
	wrapper, gate := wrapMeaningfulStreamCallback(&KiroStreamCallback{
		OnProgress: func() { progressCalls++ },
	}, func() {
		activityCalls++
	}, true, false, false, false)

	wrapper.OnProgress()

	if progressCalls != 1 {
		t.Fatalf("progress callback count = %d, want 1", progressCalls)
	}
	if activityCalls != 0 || gate.hasActivity() {
		t.Fatalf("metadata-only progress marked meaningful activity: calls=%d activity=%v", activityCalls, gate.hasActivity())
	}
}

func TestMeaningfulStreamToolFramesCountAsActivity(t *testing.T) {
	tests := []struct {
		name string
		emit func(*KiroStreamCallback)
	}{
		{
			name: "tool start",
			emit: func(callback *KiroStreamCallback) {
				callback.OnToolUseStart("toolu_write", "Write")
			},
		},
		{
			name: "tool delta",
			emit: func(callback *KiroStreamCallback) {
				callback.OnToolUseDelta("toolu_write", `{"content":"partial`)
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			activityCalls := 0
			wrapper, gate := wrapMeaningfulStreamCallback(&KiroStreamCallback{}, func() {
				activityCalls++
			}, true, true, false, false)

			tc.emit(wrapper)

			if activityCalls != 1 || !gate.hasActivity() {
				t.Fatalf("tool frame did not mark activity: calls=%d activity=%v", activityCalls, gate.hasActivity())
			}
		})
	}
}

func TestStrictMeaningfulStreamFlushesAfterSubstantiveText(t *testing.T) {
	var received []string
	wrapper, gate := wrapMeaningfulStreamCallback(&KiroStreamCallback{
		OnText: func(text string, thinking bool) {
			prefix := "text:"
			if thinking {
				prefix = "thinking:"
			}
			received = append(received, prefix+text)
		},
		OnComplete: func(int, int) {
			received = append(received, "complete")
		},
	}, nil, true, false, false, false)

	wrapper.OnText("plan", true)
	wrapper.OnText("<", false)
	wrapper.OnText("html", false)
	wrapper.OnComplete(10, 20)

	want := []string{"thinking:plan", "text:<html", "complete"}
	if !reflect.DeepEqual(received, want) {
		t.Fatalf("unexpected callback order: got %#v want %#v", received, want)
	}
	if !gate.hasActionableOutput() {
		t.Fatal("expected substantive text to commit the stream")
	}
}

func TestDeferredMeaningfulStreamCommitsTextOnlyResponseAtCompletion(t *testing.T) {
	var received []string
	wrapper, gate := wrapMeaningfulStreamCallback(&KiroStreamCallback{
		OnText: func(text string, thinking bool) {
			received = append(received, text)
		},
		OnComplete: func(int, int) {
			received = append(received, "complete")
		},
	}, nil, true, false, true, false)

	wrapper.OnText("I will create the requested file now.", false)
	if gate.hasActionableOutput() || len(received) != 0 {
		t.Fatalf("inferred tool preamble committed before completion: %#v", received)
	}
	wrapper.OnComplete(10, 20)

	want := []string{"I will create the requested file now.", "complete"}
	if !reflect.DeepEqual(received, want) || !gate.hasActionableOutput() {
		t.Fatalf("completed text response was not committed: got %#v want %#v", received, want)
	}
}

func TestDeferredMeaningfulStreamKeepsPreambleRetryableUntilToolUse(t *testing.T) {
	var received []string
	wrapper, gate := wrapMeaningfulStreamCallback(&KiroStreamCallback{
		OnText: func(text string, _ bool) {
			received = append(received, "text:"+text)
		},
		OnToolUse: func(tool KiroToolUse) {
			received = append(received, "tool:"+tool.Name)
		},
	}, nil, true, false, true, false)

	wrapper.OnText("I will write the file.", false)
	if gate.hasActionableOutput() || len(received) != 0 {
		t.Fatalf("preamble should remain retryable before a complete tool call: %#v", received)
	}
	wrapper.OnToolUse(KiroToolUse{ToolUseID: "tool-1", Name: "Write", Input: map[string]interface{}{"file_path": "index.html"}})

	want := []string{"text:I will write the file.", "tool:Write"}
	if !reflect.DeepEqual(received, want) || !gate.hasActionableOutput() {
		t.Fatalf("tool call did not commit buffered preamble: got %#v want %#v", received, want)
	}
}

func TestDeferredMeaningfulStreamStreamsThinkingBeforeActionableOutput(t *testing.T) {
	var received []string
	wrapper, gate := wrapMeaningfulStreamCallback(&KiroStreamCallback{
		OnText: func(text string, thinking bool) {
			prefix := "text:"
			if thinking {
				prefix = "thinking:"
			}
			received = append(received, prefix+text)
		},
		OnToolUse: func(tool KiroToolUse) {
			received = append(received, "tool:"+tool.Name)
		},
	}, nil, true, false, true, true)

	wrapper.OnText("planning the file", true)
	if gate.hasActionableOutput() || !reflect.DeepEqual(received, []string{"thinking:planning the file"}) {
		t.Fatalf("thinking should stream without committing actionable output: %#v", received)
	}
	wrapper.OnText("I will write it now.", false)
	if gate.hasActionableOutput() || len(received) != 1 {
		t.Fatalf("visible preamble should remain buffered: %#v", received)
	}
	wrapper.OnToolUse(KiroToolUse{ToolUseID: "tool-1", Name: "Write", Input: map[string]interface{}{"file_path": "index.html"}})

	want := []string{"thinking:planning the file", "text:I will write it now.", "tool:Write"}
	if !reflect.DeepEqual(received, want) || !gate.hasActionableOutput() {
		t.Fatalf("tool call did not commit buffered output: got %#v want %#v", received, want)
	}
}

func TestStrictMeaningfulStreamCommitsCompleteToolUse(t *testing.T) {
	var received []string
	wrapper, gate := wrapMeaningfulStreamCallback(&KiroStreamCallback{
		OnText: func(text string, thinking bool) {
			received = append(received, "thinking:"+text)
		},
		OnToolUse: func(tool KiroToolUse) {
			received = append(received, "tool:"+tool.Name)
		},
	}, nil, true, false, false, false)

	wrapper.OnText("plan", true)
	wrapper.OnToolUse(KiroToolUse{ToolUseID: "tool-1", Name: "write"})

	want := []string{"thinking:plan", "tool:write"}
	if !reflect.DeepEqual(received, want) {
		t.Fatalf("unexpected callbacks: got %#v want %#v", received, want)
	}
	if !gate.hasActionableOutput() {
		t.Fatal("expected complete tool use to commit the stream")
	}
}

func TestStrictMeaningfulStreamCommitsRecoveredToolUseWithoutStop(t *testing.T) {
	var received []KiroToolUse
	wrapper, gate := wrapMeaningfulStreamCallback(&KiroStreamCallback{
		OnToolUse: func(tool KiroToolUse) {
			received = append(received, tool)
		},
	}, nil, true, true, false, false)
	stream := bytes.NewReader(awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
		"toolUseId": "toolu_write",
		"name":      "Write",
		"input":     `{"file_path":"index.html","content":"complete"}`,
	}))

	if err := parseEventStream(stream, wrapper); err != nil {
		t.Fatalf("expected recovered tool use to succeed, got %v", err)
	}
	if !gate.hasActionableOutput() || len(received) != 1 {
		t.Fatalf("recovered tool use was not committed: actionable=%v received=%#v", gate.hasActionableOutput(), received)
	}
	if received[0].Name != "Write" || received[0].Input["content"] != "complete" {
		t.Fatalf("unexpected recovered tool use: %#v", received[0])
	}
}

func TestStrictMeaningfulStreamRejectsMalformedToolArguments(t *testing.T) {
	called := false
	wrapper, gate := wrapMeaningfulStreamCallback(&KiroStreamCallback{
		OnToolUse: func(KiroToolUse) { called = true },
	}, nil, true, false, false, false)

	wrapper.OnToolUse(KiroToolUse{
		ToolUseID: "tool-1",
		Name:      "write",
		Input:     map[string]interface{}{"_raw_arguments": `{"path":`},
	})

	if called || gate.hasActionableOutput() {
		t.Fatalf("malformed tool call was committed: called=%v actionable=%v", called, gate.hasActionableOutput())
	}
}

func TestStrictMeaningfulStreamRejectsUnclosedThinkingTag(t *testing.T) {
	called := false
	wrapper, gate := wrapMeaningfulStreamCallback(&KiroStreamCallback{
		OnText: func(string, bool) { called = true },
	}, nil, true, false, false, false)

	wrapper.OnText("<thinking>plan the page\n<html><body>unfinished", false)

	if called || gate.hasActionableOutput() {
		t.Fatalf("unclosed thinking leaked: called=%v actionable=%v", called, gate.hasActionableOutput())
	}
}

func TestStrictMeaningfulStreamRequiresToolWhenConfigured(t *testing.T) {
	var received []string
	wrapper, gate := wrapMeaningfulStreamCallback(&KiroStreamCallback{
		OnText:    func(text string, _ bool) { received = append(received, "text:"+text) },
		OnToolUse: func(tool KiroToolUse) { received = append(received, "tool:"+tool.Name) },
	}, nil, true, true, false, false)

	wrapper.OnText("Here is the complete file contents", false)
	if gate.hasActionableOutput() || len(received) != 0 {
		t.Fatalf("text-only response committed before tool use: %#v", received)
	}
	wrapper.OnToolUse(KiroToolUse{ToolUseID: "tool-1", Name: "Write", Input: map[string]interface{}{"file_path": "index.html"}})

	want := []string{"text:Here is the complete file contents", "tool:Write"}
	if !reflect.DeepEqual(received, want) || !gate.hasActionableOutput() {
		t.Fatalf("unexpected required-tool callbacks: got %#v want %#v", received, want)
	}
}

func TestVisibleTextOutsideThinking(t *testing.T) {
	visible, incomplete := visibleTextOutsideThinking("<thinking>plan</thinking>done")
	if incomplete || visible != "done" {
		t.Fatalf("unexpected closed thinking parse: visible=%q incomplete=%v", visible, incomplete)
	}
	visible, incomplete = visibleTextOutsideThinking("<thinking>plan")
	if !incomplete || visible != "" {
		t.Fatalf("unexpected unclosed thinking parse: visible=%q incomplete=%v", visible, incomplete)
	}
}

func TestHasSubstantiveAgentText(t *testing.T) {
	for _, text := range []string{"}", "]]", "```", "<=>"} {
		if hasSubstantiveAgentText(text) {
			t.Fatalf("expected structural tail %q to be rejected", text)
		}
	}
	for _, text := range []string{"done", "完成", "<html", "✓"} {
		if !hasSubstantiveAgentText(text) {
			t.Fatalf("expected %q to be substantive", text)
		}
	}
}
