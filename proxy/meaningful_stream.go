package proxy

import (
	"strings"
	"sync/atomic"
)

type meaningfulStreamCallback struct {
	target  *KiroStreamCallback
	onFirst func()
	seen    atomic.Bool
}

func wrapMeaningfulStreamCallback(target *KiroStreamCallback, onFirst func()) (*KiroStreamCallback, *meaningfulStreamCallback) {
	if target == nil {
		target = &KiroStreamCallback{}
	}
	gate := &meaningfulStreamCallback{target: target, onFirst: onFirst}
	wrapper := &KiroStreamCallback{
		OnText: func(text string, isThinking bool) {
			if strings.TrimSpace(text) == "" {
				return
			}
			gate.markMeaningful()
			if target.OnText != nil {
				target.OnText(text, isThinking)
			}
		},
		OnToolUse: func(toolUse KiroToolUse) {
			gate.markMeaningful()
			if target.OnToolUse != nil {
				target.OnToolUse(toolUse)
			}
		},
		OnComplete:     target.OnComplete,
		OnUsage:        target.OnUsage,
		OnTruncated:    target.OnTruncated,
		OnError:        target.OnError,
		OnCredits:      target.OnCredits,
		OnContextUsage: target.OnContextUsage,
	}
	return wrapper, gate
}

func (g *meaningfulStreamCallback) markMeaningful() {
	if g == nil || g.seen.Swap(true) {
		return
	}
	if g.onFirst != nil {
		g.onFirst()
	}
}

func (g *meaningfulStreamCallback) hasMeaningfulOutput() bool {
	return g != nil && g.seen.Load()
}
