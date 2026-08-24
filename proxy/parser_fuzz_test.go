package proxy

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"testing"
)

func FuzzParseEventStream(f *testing.F) {
	valid := benchmarkEventStreamFrame("assistantResponseEvent", map[string]interface{}{"content": "hello"})
	valid = append(valid, benchmarkEventStreamFrame("metadataEvent", map[string]interface{}{"stopReason": "end_turn"})...)
	f.Add(valid)
	f.Add(valid[:len(valid)-3])
	f.Add([]byte{})
	f.Add([]byte("not-an-event-stream"))
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 1<<20 {
			t.Skip()
		}
		if len(data) >= 4 {
			claimed := binary.BigEndian.Uint32(data[:4])
			if claimed > 1<<20 && claimed <= eventStreamMaxFrame {
				t.Skip()
			}
		}
		_ = parseEventStream(bytes.NewReader(data), &KiroStreamCallback{})
	})
}

func FuzzClaudeToolSchemaConversion(f *testing.F) {
	for _, seed := range []string{
		`{"type":"object","properties":{}}`,
		`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`,
		`{"oneOf":[{"type":"string"},{"type":"number"}]}`,
		`null`,
		`[]`,
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		if len(raw) > 256<<10 {
			t.Skip()
		}
		var schema interface{}
		if json.Unmarshal([]byte(raw), &schema) != nil {
			return
		}
		converted, _ := convertClaudeTools([]ClaudeTool{{Name: "fuzz_tool", Description: "fuzz", InputSchema: schema}})
		_, _ = json.Marshal(converted)
	})
}

func FuzzClaudeRequestTranslation(f *testing.F) {
	for _, seed := range []string{
		`{"model":"claude-sonnet-5","max_tokens":64,"messages":[{"role":"user","content":"hello"}]}`,
		`{"model":"claude-sonnet-5-thinking","messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]}`,
		`{"messages":[]}`,
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		if len(raw) > 256<<10 {
			t.Skip()
		}
		var request ClaudeRequest
		if json.Unmarshal([]byte(raw), &request) != nil {
			return
		}
		payload := ClaudeToKiro(&request, false)
		_, _ = json.Marshal(payload)
	})
}

func FuzzOpenAIRequestTranslation(f *testing.F) {
	for _, seed := range []string{
		`{"model":"claude-sonnet-5","max_tokens":64,"messages":[{"role":"user","content":"hello"}]}`,
		`{"model":"claude-sonnet-5","messages":[{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"echo","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call_1","content":"OK"}]}`,
		`{"messages":[],"tools":[]}`,
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		if len(raw) > 256<<10 {
			t.Skip()
		}
		var request OpenAIRequest
		if json.Unmarshal([]byte(raw), &request) != nil {
			return
		}
		payload := OpenAIToKiro(&request, false)
		_, _ = json.Marshal(payload)
	})
}

func FuzzResponsesInputParsing(f *testing.F) {
	for _, seed := range []string{
		`"hello"`,
		`[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}]`,
		`[{"type":"function_call_output","call_id":"call_1","output":"OK"}]`,
		`null`,
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		if len(raw) > 256<<10 {
			t.Skip()
		}
		_, _ = parseResponsesInputWithTools(json.RawMessage(raw))
	})
}
