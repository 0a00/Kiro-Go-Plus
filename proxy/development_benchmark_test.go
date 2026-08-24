package proxy

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func BenchmarkClaudeRequestTranslation(b *testing.B) {
	request := &ClaudeRequest{
		Model: "claude-sonnet-5", MaxTokens: 4096,
		System: strings.Repeat("stable system instruction ", 200),
		Messages: []ClaudeMessage{
			{Role: "user", Content: "Inspect the repository and update the implementation."},
			{Role: "assistant", Content: "I will inspect the relevant files first."},
			{Role: "user", Content: "Continue and run focused tests."},
		},
		Tools: []ClaudeTool{{
			Name: "read_file", Description: "Read a UTF-8 file from the workspace.",
			InputSchema: map[string]interface{}{
				"type": "object", "properties": map[string]interface{}{"path": map[string]interface{}{"type": "string"}}, "required": []string{"path"},
			},
		}},
	}
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		if payload := ClaudeToKiro(request, false); payload == nil {
			b.Fatal("nil payload")
		}
	}
}

func BenchmarkToolSchemaConversion(b *testing.B) {
	tools := make([]ClaudeTool, 32)
	for index := range tools {
		tools[index] = ClaudeTool{
			Name: fmt.Sprintf("mcp__fixture__tool_%d", index), Description: strings.Repeat("tool description ", 10),
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"path":  map[string]interface{}{"type": "string"},
					"limit": map[string]interface{}{"type": "integer", "minimum": 1},
				},
				"required": []string{"path"},
			},
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		converted, _ := convertClaudeTools(tools)
		if len(converted) != len(tools) {
			b.Fatal("tool conversion lost entries")
		}
	}
}

func BenchmarkPromptCacheHit(b *testing.B) {
	tracker := newPromptCacheTrackerWithSettings(time.Hour, 0.9)
	request := &ClaudeRequest{
		Model: "claude-sonnet-5",
		System: []interface{}{map[string]interface{}{
			"type": "text", "text": strings.Repeat("cacheable repository context ", 500),
			"cache_control": map[string]interface{}{"type": "ephemeral", "ttl": "1h"},
		}},
		Messages: []ClaudeMessage{{Role: "user", Content: "continue"}},
	}
	profile := tracker.BuildClaudeProfile(request, 5000)
	if profile == nil {
		b.Fatal("cache profile was not built")
	}
	tracker.Update("benchmark-account", profile)
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		usage := tracker.Compute("benchmark-account", profile)
		if usage.CacheReadInputTokens == 0 {
			b.Fatal("cache hit was not recorded")
		}
	}
}

func BenchmarkRequestLogConcurrentAdd(b *testing.B) {
	log := newRequestLog(4096)
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			log.add(requestLogEntry{Protocol: "anthropic", Model: "claude-sonnet-5", Status: "success", StatusCode: 200})
		}
	})
}
