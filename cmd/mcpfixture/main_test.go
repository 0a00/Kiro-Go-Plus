package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMCPFixtureInitializeListCallAndAudit(t *testing.T) {
	initialize := handleRequest(rpcRequest{JSONRPC: "2.0", ID: json.RawMessage("1"), Method: "initialize", Params: json.RawMessage(`{"protocolVersion":"2025-06-18"}`)})
	if initialize == nil || initialize.Error != nil {
		t.Fatalf("initialize response: %+v", initialize)
	}
	encoded, _ := json.Marshal(initialize.Result)
	if string(encoded) == "" || !json.Valid(encoded) {
		t.Fatalf("invalid initialize result: %s", encoded)
	}

	list := handleRequest(rpcRequest{JSONRPC: "2.0", ID: json.RawMessage("2"), Method: "tools/list"})
	encoded, _ = json.Marshal(list.Result)
	if list.Error != nil || !containsJSONText(encoded, fixtureToolName) {
		t.Fatalf("tool list response: %s error=%v", encoded, list.Error)
	}

	auditPath := filepath.Join(t.TempDir(), "audit.log")
	t.Setenv("KIRO_MCP_FIXTURE_AUDIT", auditPath)
	call := handleRequest(rpcRequest{
		JSONRPC: "2.0", ID: json.RawMessage("3"), Method: "tools/call",
		Params: json.RawMessage(`{"name":"devcheck_echo","arguments":{"value":"MCP_CLIENT_E2E_OK"}}`),
	})
	encoded, _ = json.Marshal(call.Result)
	if call.Error != nil || !containsJSONText(encoded, "MCP_CLIENT_E2E_OK") {
		t.Fatalf("tool call response: %s error=%v", encoded, call.Error)
	}
	audit, err := os.ReadFile(auditPath)
	if err != nil || string(audit) != fixtureToolName+"\n" {
		t.Fatalf("audit = %q err=%v", audit, err)
	}
	info, err := os.Stat(auditPath)
	if err != nil {
		t.Fatalf("stat audit: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("audit permissions = %v", info.Mode().Perm())
	}
}

func TestMCPFixtureSupportsZeroArgumentAndRepeatedCalls(t *testing.T) {
	auditPath := filepath.Join(t.TempDir(), "audit.log")
	t.Setenv("KIRO_MCP_FIXTURE_AUDIT", auditPath)

	zeroArgument := handleRequest(rpcRequest{
		JSONRPC: "2.0", ID: json.RawMessage("1"), Method: "tools/call",
		Params: json.RawMessage(`{"name":"devcheck_no_args","arguments":{}}`),
	})
	encoded, _ := json.Marshal(zeroArgument.Result)
	if zeroArgument.Error != nil || !containsJSONText(encoded, "MCP_NO_ARGS_OK") {
		t.Fatalf("zero-argument tool response: %s error=%v", encoded, zeroArgument.Error)
	}

	for id, value := range map[string]string{"2": "A", "3": "B"} {
		call := handleRequest(rpcRequest{
			JSONRPC: "2.0", ID: json.RawMessage(id), Method: "tools/call",
			Params: json.RawMessage(`{"name":"devcheck_repeat","arguments":{"value":"` + value + `"}}`),
		})
		encoded, _ = json.Marshal(call.Result)
		if call.Error != nil || !containsJSONText(encoded, value) {
			t.Fatalf("repeated tool response: %s error=%v", encoded, call.Error)
		}
	}

	audit, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatalf("read audit: %v", err)
	}
	if string(audit) != fixtureNoArgToolName+"\n"+fixtureRepeatToolName+"\n"+fixtureRepeatToolName+"\n" {
		t.Fatalf("audit = %q", audit)
	}
}

func TestMCPFixtureRejectsBadCallsAndUnknownMethods(t *testing.T) {
	badCall := handleRequest(rpcRequest{JSONRPC: "2.0", ID: json.RawMessage("1"), Method: "tools/call", Params: json.RawMessage(`{"name":"wrong"}`)})
	encoded, _ := json.Marshal(badCall.Result)
	if badCall.Error != nil || !containsJSONText(encoded, `"isError":true`) {
		t.Fatalf("bad call response: %s error=%v", encoded, badCall.Error)
	}
	unknown := handleRequest(rpcRequest{JSONRPC: "2.0", ID: json.RawMessage("2"), Method: "unknown"})
	if unknown.Error == nil || unknown.Error.Code != -32601 {
		t.Fatalf("unknown method response: %+v", unknown)
	}
	if notification := handleRequest(rpcRequest{JSONRPC: "2.0", Method: "notifications/initialized"}); notification != nil {
		t.Fatalf("notification returned a response: %+v", notification)
	}
}

func containsJSONText(data []byte, value string) bool {
	return len(data) > 0 && json.Valid(data) && strings.Contains(string(data), value)
}
