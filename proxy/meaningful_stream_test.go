package proxy

import (
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
	}, nil, true, false)

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
	}, nil, true, false)

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

func TestStrictMeaningfulStreamCommitsCompleteToolUse(t *testing.T) {
	var received []string
	wrapper, gate := wrapMeaningfulStreamCallback(&KiroStreamCallback{
		OnText: func(text string, thinking bool) {
			received = append(received, "thinking:"+text)
		},
		OnToolUse: func(tool KiroToolUse) {
			received = append(received, "tool:"+tool.Name)
		},
	}, nil, true, false)

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

func TestStrictMeaningfulStreamRejectsMalformedToolArguments(t *testing.T) {
	called := false
	wrapper, gate := wrapMeaningfulStreamCallback(&KiroStreamCallback{
		OnToolUse: func(KiroToolUse) { called = true },
	}, nil, true, false)

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
	}, nil, true, false)

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
	}, nil, true, true)

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
