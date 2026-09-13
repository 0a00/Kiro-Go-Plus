package proxy

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestOpenAIStreamUsageModeIsTriState(t *testing.T) {
	var omitted OpenAIRequest
	if err := json.Unmarshal([]byte(`{"model":"x","stream":true}`), &omitted); err != nil {
		t.Fatal(err)
	}
	if got := resolveOpenAIStreamUsageMode(omitted.StreamOptions); got != openAIStreamUsageLegacy {
		t.Fatalf("omitted stream_options mode = %d, want legacy", got)
	}

	var disabled OpenAIRequest
	if err := json.Unmarshal([]byte(`{"model":"x","stream":true,"stream_options":{"include_usage":false}}`), &disabled); err != nil {
		t.Fatal(err)
	}
	if got := resolveOpenAIStreamUsageMode(disabled.StreamOptions); got != openAIStreamUsageOmit {
		t.Fatalf("explicit false mode = %d, want omit", got)
	}

	var enabled OpenAIRequest
	if err := json.Unmarshal([]byte(`{"model":"x","stream":true,"stream_options":{"include_usage":true}}`), &enabled); err != nil {
		t.Fatal(err)
	}
	if got := resolveOpenAIStreamUsageMode(enabled.StreamOptions); got != openAIStreamUsageInclude {
		t.Fatalf("explicit true mode = %d, want include", got)
	}
}

func TestOpenAIStreamTerminalChunksFollowRequestedUsageContract(t *testing.T) {
	usage := map[string]interface{}{"total_tokens": 7}

	legacy := buildOpenAIStreamTerminalChunks("chat", "model", "stop", usage, openAIStreamUsageLegacy)
	if len(legacy) != 1 || legacy[0]["usage"] == nil {
		t.Fatalf("legacy terminal chunks = %#v", legacy)
	}

	withoutUsage := buildOpenAIStreamTerminalChunks("chat", "model", "stop", usage, openAIStreamUsageOmit)
	if len(withoutUsage) != 1 {
		t.Fatalf("explicit false terminal chunks = %#v", withoutUsage)
	}
	if _, ok := withoutUsage[0]["usage"]; ok {
		t.Fatalf("explicit false unexpectedly included usage: %#v", withoutUsage[0])
	}

	withUsage := buildOpenAIStreamTerminalChunks("chat", "model", "stop", usage, openAIStreamUsageInclude)
	if len(withUsage) != 2 {
		t.Fatalf("explicit true terminal chunks = %#v", withUsage)
	}
	if value, ok := withUsage[0]["usage"]; !ok || value != nil {
		t.Fatalf("finish chunk usage = %#v, want null", value)
	}
	choices, ok := withUsage[1]["choices"].([]interface{})
	if !ok || len(choices) != 0 {
		t.Fatalf("usage chunk choices = %#v, want empty array", withUsage[1]["choices"])
	}
	if !reflect.DeepEqual(withUsage[1]["usage"], usage) {
		t.Fatalf("usage chunk did not carry usage map")
	}
}
