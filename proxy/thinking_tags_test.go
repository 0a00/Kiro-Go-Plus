package proxy

import (
	"strings"
	"testing"
)

func TestThinkingTagParserHandlesSplitTagsAndUTF8(t *testing.T) {
	var source thinkingStreamSource
	parser := newThinkingTagParser(&source)
	segments := parser.feed("前置<thin", false)
	segments = append(segments, parser.feed("king>\n内部", false)...)
	segments = append(segments, parser.feed("思考</thinking>答案", false)...)
	segments = append(segments, parser.feed("", true)...)

	var visible, reasoning strings.Builder
	starts, ends := 0, 0
	for _, segment := range segments {
		switch segment.kind {
		case thinkingTagVisible:
			visible.WriteString(segment.text)
		case thinkingTagStart, thinkingTagMiddle, thinkingTagEnd:
			reasoning.WriteString(segment.text)
			if segment.kind == thinkingTagStart {
				starts++
			}
			if segment.kind == thinkingTagEnd {
				ends++
			}
		}
	}
	if visible.String() != "前置答案" {
		t.Fatalf("visible text = %q, want %q", visible.String(), "前置答案")
	}
	if reasoning.String() != "\n内部思考" {
		t.Fatalf("reasoning = %q, want %q", reasoning.String(), "\n内部思考")
	}
	if starts != 1 || ends != 1 {
		t.Fatalf("thinking boundaries = start:%d end:%d, want one each", starts, ends)
	}
}

func TestThinkingTagParserFlushesUnclosedBlock(t *testing.T) {
	parser := newThinkingTagParser(nil)
	segments := parser.feed("<thinking>未完成", false)
	segments = append(segments, parser.feed("", true)...)
	var reasoning strings.Builder
	for _, segment := range segments {
		if segment.kind != thinkingTagVisible {
			reasoning.WriteString(segment.text)
		}
	}
	if reasoning.String() != "未完成" {
		t.Fatalf("flushed reasoning = %q, want %q", reasoning.String(), "未完成")
	}

	parser.reset()
	segments = parser.feed("普通文本<think", false)
	segments = append(segments, parser.feed("", true)...)
	var visible strings.Builder
	for _, segment := range segments {
		if segment.kind == thinkingTagVisible {
			visible.WriteString(segment.text)
		}
	}
	if visible.String() != "普通文本<think" {
		t.Fatalf("incomplete opening tag was lost: %q", visible.String())
	}
}

func TestSplitThinkingTagsSupportsBothSpellings(t *testing.T) {
	visible, reasoning := splitThinkingTags("<think>one</think> answer <thinking>two</thinking>")
	if visible != "answer" {
		t.Fatalf("visible = %q, want %q", visible, "answer")
	}
	if reasoning != "onetwo" {
		t.Fatalf("reasoning = %q, want %q", reasoning, "onetwo")
	}
}

func TestThinkingTagParserDropsDuplicateTagsAfterNativeReasoning(t *testing.T) {
	source := thinkingSourceReasoningEvent
	parser := newThinkingTagParser(&source)
	segments := parser.feed("<thinking>duplicate</thinking>answer", true)
	var visible, reasoning strings.Builder
	for _, segment := range segments {
		if segment.kind == thinkingTagVisible {
			visible.WriteString(segment.text)
		} else {
			reasoning.WriteString(segment.text)
		}
	}
	if visible.String() != "answer" || reasoning.Len() != 0 {
		t.Fatalf("duplicate tag handling = visible:%q reasoning:%q", visible.String(), reasoning.String())
	}
}

func FuzzThinkingTagParser(f *testing.F) {
	for _, seed := range []string{
		"plain response",
		"<thinking>plan</thinking>answer",
		"prefix<think>中文思考</think>结论",
		"<thinking>",
		"前置<thin",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input string) {
		var source thinkingStreamSource
		parser := newThinkingTagParser(&source)
		for offset := 0; offset < len(input); {
			step := 1 + (int(input[offset]) & 7)
			end := offset + step
			if end > len(input) {
				end = len(input)
			}
			parser.feed(input[offset:end], false)
			offset = end
		}
		parser.feed("", true)
		_, _ = splitThinkingTags(input)
	})
}
