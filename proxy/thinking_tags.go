package proxy

import "strings"

// thinkingTagSegmentKind describes the boundary of a synthetic thinking block
// detected in ordinary assistant text. Native reasoning events do not use this
// parser; the source guard prevents the two representations being duplicated.
type thinkingTagSegmentKind uint8

const (
	thinkingTagVisible thinkingTagSegmentKind = iota
	thinkingTagStart
	thinkingTagMiddle
	thinkingTagEnd
)

type thinkingTagSegment struct {
	kind thinkingTagSegmentKind
	text string
}

// thinkingTagParser incrementally separates <thinking>/<think> blocks from
// ordinary assistant text. It retains only a short suffix while looking for a
// tag, so a long response is never buffered in its entirety.
type thinkingTagParser struct {
	buffer          string
	inThinking      bool
	closeTag        string
	dropThinking    bool
	thinkingStarted bool
	source          *thinkingStreamSource
}

func newThinkingTagParser(source *thinkingStreamSource) *thinkingTagParser {
	return &thinkingTagParser{source: source}
}

func (p *thinkingTagParser) reset() {
	if p == nil {
		return
	}
	p.buffer = ""
	p.inThinking = false
	p.closeTag = ""
	p.dropThinking = false
	p.thinkingStarted = false
}

func (p *thinkingTagParser) tagSourceAllowed() bool {
	if p == nil || p.source == nil {
		return true
	}
	return allowTagSource(p.source)
}

func (p *thinkingTagParser) appendThinking(segments *[]thinkingTagSegment, text string, closing bool) {
	if p == nil || p.dropThinking {
		return
	}
	if !p.thinkingStarted {
		if text != "" {
			*segments = append(*segments, thinkingTagSegment{kind: thinkingTagStart, text: text})
			p.thinkingStarted = true
		}
		if closing && p.thinkingStarted {
			*segments = append(*segments, thinkingTagSegment{kind: thinkingTagEnd})
		}
		return
	}
	if closing {
		*segments = append(*segments, thinkingTagSegment{kind: thinkingTagEnd, text: text})
		return
	}
	if text != "" {
		*segments = append(*segments, thinkingTagSegment{kind: thinkingTagMiddle, text: text})
	}
}

// feed consumes one assistant text fragment. With forceFlush set, any
// incomplete opening/closing tag is resolved at the end of the response: an
// incomplete opening tag remains visible, while an opened thinking block is
// emitted as a closed synthetic block.
func (p *thinkingTagParser) feed(text string, forceFlush bool) []thinkingTagSegment {
	if p == nil {
		return nil
	}
	if text != "" {
		p.buffer += text
	}
	segments := make([]thinkingTagSegment, 0, 2)
	for {
		if !p.inThinking {
			start, openTag, closeTag := nextThinkingTag(p.buffer, 0)
			if start >= 0 {
				if start > 0 {
					segments = append(segments, thinkingTagSegment{kind: thinkingTagVisible, text: p.buffer[:start]})
				}
				p.buffer = p.buffer[start+len(openTag):]
				p.inThinking = true
				p.closeTag = closeTag
				p.dropThinking = !p.tagSourceAllowed()
				p.thinkingStarted = false
				continue
			}

			safeLen := len(p.buffer)
			if !forceFlush {
				safeLen -= trailingThinkingTagPrefix(p.buffer)
			}
			safeLen = safeUTF8PrefixBytes(p.buffer, safeLen)
			if safeLen > 0 {
				segments = append(segments, thinkingTagSegment{kind: thinkingTagVisible, text: p.buffer[:safeLen]})
				p.buffer = p.buffer[safeLen:]
			}
			break
		}

		if p.closeTag == "" {
			p.closeTag = "</thinking>"
		}
		end := strings.Index(p.buffer, p.closeTag)
		if end >= 0 {
			p.appendThinking(&segments, p.buffer[:end], true)
			p.buffer = p.buffer[end+len(p.closeTag):]
			p.inThinking = false
			p.closeTag = ""
			p.dropThinking = false
			p.thinkingStarted = false
			continue
		}

		if forceFlush {
			p.appendThinking(&segments, p.buffer, true)
			p.buffer = ""
			p.inThinking = false
			p.closeTag = ""
			p.dropThinking = false
			p.thinkingStarted = false
			break
		}

		safeLen := len(p.buffer) - trailingTagPrefix(p.buffer, p.closeTag)
		safeLen = safeUTF8PrefixBytes(p.buffer, safeLen)
		if safeLen > 0 {
			p.appendThinking(&segments, p.buffer[:safeLen], false)
			p.buffer = p.buffer[safeLen:]
		}
		break
	}
	return segments
}

// splitThinkingTags is the buffered counterpart used by non-streaming
// Responses. It accepts both tag spellings and keeps the visible text separate
// from the extracted reasoning without exposing the tags to the caller.
func splitThinkingTags(content string) (visible, reasoning string) {
	source := thinkingSourceUnknown
	parser := newThinkingTagParser(&source)
	segments := parser.feed(content, false)
	segments = append(segments, parser.feed("", true)...)
	var visibleBuilder, reasoningBuilder strings.Builder
	for _, segment := range segments {
		switch segment.kind {
		case thinkingTagVisible:
			visibleBuilder.WriteString(segment.text)
		case thinkingTagStart, thinkingTagMiddle, thinkingTagEnd:
			reasoningBuilder.WriteString(segment.text)
		}
	}
	return strings.TrimSpace(visibleBuilder.String()), reasoningBuilder.String()
}
