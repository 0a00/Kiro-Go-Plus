// Command mcpfixture is a minimal, isolated stdio MCP server for client E2E tests.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

const (
	fixtureToolName       = "devcheck_echo"
	fixtureNoArgToolName  = "devcheck_no_args"
	fixtureRepeatToolName = "devcheck_repeat"
)

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  interface{}     `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func main() {
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	encoder := json.NewEncoder(os.Stdout)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var request rpcRequest
		if err := json.Unmarshal([]byte(line), &request); err != nil {
			_ = encoder.Encode(rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{Code: -32700, Message: "parse error"}})
			continue
		}
		if response := handleRequest(request); response != nil {
			_ = encoder.Encode(response)
		}
	}
	if err := scanner.Err(); err != nil {
		fmt.Fprintln(os.Stderr, "mcp fixture input:", err)
		os.Exit(1)
	}
}

func handleRequest(request rpcRequest) *rpcResponse {
	if request.JSONRPC != "2.0" || strings.TrimSpace(request.Method) == "" {
		return errorResponse(request.ID, -32600, "invalid request")
	}
	if len(request.ID) == 0 {
		return nil
	}
	response := &rpcResponse{JSONRPC: "2.0", ID: request.ID}
	switch request.Method {
	case "initialize":
		var params struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(request.Params, &params)
		if params.ProtocolVersion == "" {
			params.ProtocolVersion = "2025-03-26"
		}
		response.Result = map[string]interface{}{
			"protocolVersion": params.ProtocolVersion,
			"capabilities":    map[string]interface{}{"tools": map[string]interface{}{"listChanged": false}},
			"serverInfo":      map[string]interface{}{"name": "kiro-go-plus-devcheck", "version": "1"},
		}
	case "ping":
		response.Result = map[string]interface{}{}
	case "tools/list":
		response.Result = map[string]interface{}{"tools": fixtureTools()}
	case "tools/call":
		var params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if json.Unmarshal(request.Params, &params) != nil {
			response.Result = map[string]interface{}{
				"content": []interface{}{map[string]interface{}{"type": "text", "text": "invalid fixture tool call"}}, "isError": true,
			}
			break
		}
		response.Result = handleToolCall(params.Name, params.Arguments)
	default:
		return errorResponse(request.ID, -32601, "method not found")
	}
	return response
}

func fixtureTools() []interface{} {
	valueSchema := map[string]interface{}{
		"type": "object", "properties": map[string]interface{}{"value": map[string]interface{}{"type": "string"}},
		"required": []string{"value"}, "additionalProperties": false,
	}
	return []interface{}{
		map[string]interface{}{"name": fixtureToolName, "description": "Return a deterministic value for Kiro-Go Plus client testing.", "inputSchema": valueSchema},
		map[string]interface{}{"name": fixtureNoArgToolName, "description": "Return a deterministic value without accepting arguments.", "inputSchema": map[string]interface{}{
			"type": "object", "properties": map[string]interface{}{}, "additionalProperties": false,
		}},
		map[string]interface{}{"name": fixtureRepeatToolName, "description": "Return a value and record repeated tool dispatches.", "inputSchema": valueSchema},
	}
}

func handleToolCall(name string, rawArguments json.RawMessage) map[string]interface{} {
	arguments := strings.TrimSpace(string(rawArguments))
	switch name {
	case fixtureToolName, fixtureRepeatToolName:
		var input struct {
			Value string `json:"value"`
		}
		if arguments == "" || json.Unmarshal(rawArguments, &input) != nil || strings.TrimSpace(input.Value) == "" {
			return fixtureToolError("invalid fixture tool call")
		}
		recordNamedToolCall(name)
		return fixtureToolResult(input.Value)
	case fixtureNoArgToolName:
		if arguments != "" && arguments != "{}" && arguments != "null" {
			return fixtureToolError("zero-argument tool received input")
		}
		recordNamedToolCall(name)
		return fixtureToolResult("MCP_NO_ARGS_OK")
	default:
		return fixtureToolError("unknown fixture tool")
	}
}

func fixtureToolResult(value string) map[string]interface{} {
	return map[string]interface{}{
		"content": []interface{}{map[string]interface{}{"type": "text", "text": value}}, "isError": false,
	}
}

func fixtureToolError(message string) map[string]interface{} {
	return map[string]interface{}{
		"content": []interface{}{map[string]interface{}{"type": "text", "text": message}}, "isError": true,
	}
}

func errorResponse(id json.RawMessage, code int, message string) *rpcResponse {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	return &rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: message}}
}

func recordToolCall() {
	recordNamedToolCall(fixtureToolName)
}

func recordNamedToolCall(name string) {
	path := strings.TrimSpace(os.Getenv("KIRO_MCP_FIXTURE_AUDIT"))
	if path == "" {
		return
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	_ = file.Chmod(0o600)
	_, _ = file.WriteString(name + "\n")
	_ = file.Close()
}
