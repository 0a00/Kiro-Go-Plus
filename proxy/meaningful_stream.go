package proxy

import (
	"strings"
	"sync"
	"sync/atomic"
	"unicode"
)

type pendingStreamEventKind uint8

const (
	pendingText pendingStreamEventKind = iota
	pendingToolUse
	pendingComplete
	pendingUsage
	pendingTruncated
	pendingError
	pendingCredits
	pendingContextUsage
)

type pendingStreamEvent struct {
	kind       pendingStreamEventKind
	text       string
	isThinking bool
	toolUse    KiroToolUse
	input      int
	output     int
	usage      KiroTokenUsage
	err        error
	credits    float64
	percentage float64
}

// meaningfulStreamCallback stops the first-token timer on any upstream
// activity, but only commits strict tool-agent streams after substantive visible
// text or a complete tool call. This keeps hidden reasoning and malformed tails
// such as a lone "}" from turning an unusable response into HTTP 200 success.
type meaningfulStreamCallback struct {
	target     *KiroStreamCallback
	onActivity func()
	strict     bool
	activity   atomic.Bool
	actionable atomic.Bool

	mu           sync.Mutex
	committed    bool
	visibleProbe strings.Builder
	pending      []pendingStreamEvent
}

func wrapMeaningfulStreamCallback(target *KiroStreamCallback, onActivity func(), strict bool) (*KiroStreamCallback, *meaningfulStreamCallback) {
	if target == nil {
		target = &KiroStreamCallback{}
	}
	gate := &meaningfulStreamCallback{target: target, onActivity: onActivity, strict: strict}
	wrapper := &KiroStreamCallback{
		OnText: func(text string, isThinking bool) {
			if strings.TrimSpace(text) == "" {
				return
			}
			gate.markActivity()
			gate.handleEvent(pendingStreamEvent{kind: pendingText, text: text, isThinking: isThinking})
		},
		OnToolUse: func(toolUse KiroToolUse) {
			gate.markActivity()
			gate.handleEvent(pendingStreamEvent{kind: pendingToolUse, toolUse: toolUse})
		},
		OnComplete: func(input, output int) {
			gate.handleEvent(pendingStreamEvent{kind: pendingComplete, input: input, output: output})
		},
		OnUsage: func(usage KiroTokenUsage) {
			gate.handleEvent(pendingStreamEvent{kind: pendingUsage, usage: usage})
		},
		OnTruncated: func(reason string) {
			gate.handleEvent(pendingStreamEvent{kind: pendingTruncated, text: reason})
		},
		OnError: func(err error) {
			gate.handleEvent(pendingStreamEvent{kind: pendingError, err: err})
		},
		OnCredits: func(credits float64) {
			gate.handleEvent(pendingStreamEvent{kind: pendingCredits, credits: credits})
		},
		OnContextUsage: func(percentage float64) {
			gate.handleEvent(pendingStreamEvent{kind: pendingContextUsage, percentage: percentage})
		},
	}
	return wrapper, gate
}

func (g *meaningfulStreamCallback) markActivity() {
	if g == nil || g.activity.Swap(true) {
		return
	}
	if g.onActivity != nil {
		g.onActivity()
	}
}

func (g *meaningfulStreamCallback) handleEvent(event pendingStreamEvent) {
	if g == nil {
		return
	}
	if !g.strict {
		if event.kind == pendingText || event.kind == pendingToolUse {
			g.actionable.Store(true)
		}
		g.dispatch(event)
		return
	}
	if event.kind == pendingToolUse && !isActionableToolUse(event.toolUse) {
		return
	}

	g.mu.Lock()
	if g.committed {
		g.mu.Unlock()
		g.dispatch(event)
		return
	}
	g.appendPendingLocked(event)

	commit := event.kind == pendingToolUse
	if event.kind == pendingText && !event.isThinking {
		if g.visibleProbe.Len() < 4096 {
			remaining := 4096 - g.visibleProbe.Len()
			if len(event.text) > remaining {
				g.visibleProbe.WriteString(event.text[:remaining])
			} else {
				g.visibleProbe.WriteString(event.text)
			}
		}
		commit = hasSubstantiveAgentText(g.visibleProbe.String())
	}
	if !commit {
		g.mu.Unlock()
		return
	}

	g.committed = true
	g.actionable.Store(true)
	pending := g.pending
	g.pending = nil
	g.mu.Unlock()
	for _, item := range pending {
		g.dispatch(item)
	}
}

func (g *meaningfulStreamCallback) appendPendingLocked(event pendingStreamEvent) {
	if event.kind == pendingText && len(g.pending) > 0 {
		last := &g.pending[len(g.pending)-1]
		if last.kind == pendingText && last.isThinking == event.isThinking {
			last.text += event.text
			return
		}
	}
	if event.kind == pendingUsage && len(g.pending) > 0 {
		last := &g.pending[len(g.pending)-1]
		if last.kind == pendingUsage {
			*last = event
			return
		}
	}
	g.pending = append(g.pending, event)
}

func (g *meaningfulStreamCallback) dispatch(event pendingStreamEvent) {
	if g == nil || g.target == nil {
		return
	}
	switch event.kind {
	case pendingText:
		if g.target.OnText != nil {
			g.target.OnText(event.text, event.isThinking)
		}
	case pendingToolUse:
		if g.target.OnToolUse != nil {
			g.target.OnToolUse(event.toolUse)
		}
	case pendingComplete:
		if g.target.OnComplete != nil {
			g.target.OnComplete(event.input, event.output)
		}
	case pendingUsage:
		if g.target.OnUsage != nil {
			g.target.OnUsage(event.usage)
		}
	case pendingTruncated:
		if g.target.OnTruncated != nil {
			g.target.OnTruncated(event.text)
		}
	case pendingError:
		if g.target.OnError != nil {
			g.target.OnError(event.err)
		}
	case pendingCredits:
		if g.target.OnCredits != nil {
			g.target.OnCredits(event.credits)
		}
	case pendingContextUsage:
		if g.target.OnContextUsage != nil {
			g.target.OnContextUsage(event.percentage)
		}
	}
}

func (g *meaningfulStreamCallback) hasActivity() bool {
	return g != nil && g.activity.Load()
}

func (g *meaningfulStreamCallback) hasActionableOutput() bool {
	return g != nil && g.actionable.Load()
}

func hasSubstantiveAgentText(text string) bool {
	text = strings.TrimSpace(text)
	if text == "" {
		return false
	}
	for _, r := range text {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			return true
		}
		if unicode.IsSymbol(r) && !strings.ContainsRune("<>+-=*/\\|&_#~`", r) {
			return true
		}
	}
	return false
}

func isActionableToolUse(toolUse KiroToolUse) bool {
	if strings.TrimSpace(toolUse.Name) == "" {
		return false
	}
	_, malformed := toolUse.Input["_raw_arguments"]
	return !malformed
}
