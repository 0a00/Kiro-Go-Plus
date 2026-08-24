package main

import (
	"strings"
	"testing"
	"time"
)

func FuzzConsumeSSE(f *testing.F) {
	for _, seed := range []string{
		"",
		"data: [DONE]\n\n",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"OK\"}}\n\n",
		"data: {not-json}\n\n",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, stream string) {
		if len(stream) > 1<<20 {
			t.Skip()
		}
		_, _ = consumeSSE(strings.NewReader(stream), time.Now())
	})
}
