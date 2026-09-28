package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"kiro-go/internal/httpbody"
	"strings"
	"time"
)

// Stop after the matching JSON-RPC response: an SSE MCP connection may remain
// open for notifications after the search has completed.
func decodeMCPResponse(body io.Reader, contentType, requestID string) (*mcpResponse, error) {
	limited := &io.LimitedReader{R: body, N: httpbody.DefaultLimit + 1}
	reader := bufio.NewReader(limited)
	accept := func(raw []byte) (*mcpResponse, error) {
		if limited.N <= 0 {
			return nil, fmt.Errorf("MCP response exceeds %d bytes", httpbody.DefaultLimit)
		}
		var response mcpResponse
		if err := json.Unmarshal(raw, &response); err != nil {
			return nil, fmt.Errorf("invalid MCP JSON response")
		}
		if response.Result == nil && response.Error == nil {
			return nil, nil // JSON-RPC notification, not the requested result.
		}
		if requestID != "" && len(response.ID) > 0 {
			var id string
			if json.Unmarshal(response.ID, &id) != nil || id != requestID {
				return nil, nil
			}
		}
		return &response, nil
	}
	// Some MCP routes omit the SSE content type. Detect ordinary JSON without
	// treating SSE comments or event names as JSON payloads.
	for {
		b, err := reader.Peek(1)
		if err != nil {
			return nil, err
		}
		if !bytes.ContainsAny(b, " \r\n\t") {
			break
		}
		_, _ = reader.ReadByte()
	}
	first, _ := reader.Peek(1)
	if !strings.HasPrefix(strings.ToLower(contentType), "text/event-stream") && first[0] == '{' {
		raw, err := httpbody.ReadAll(reader, httpbody.DefaultLimit)
		if err != nil {
			return nil, err
		}
		response, err := accept(raw)
		if err == nil && response == nil {
			err = fmt.Errorf("MCP response is missing matching result")
		}
		return response, err
	}
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), int(httpbody.DefaultLimit)+1)
	var data strings.Builder
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if line == "" {
			if data.Len() == 0 {
				continue
			}
			response, err := accept([]byte(data.String()))
			data.Reset()
			if err != nil || response != nil {
				return response, err
			}
		} else if strings.HasPrefix(line, "data:") {
			data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
			data.WriteByte('\n')
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if data.Len() > 0 {
		if response, err := accept([]byte(data.String())); err != nil || response != nil {
			return response, err
		}
	}
	return nil, fmt.Errorf("MCP stream ended without a matching result")
}

// Kiro has returned both epoch milliseconds and ISO dates in search results.
// Invalid optional dates must not discard otherwise usable titles and URLs.
func (r *webSearchResult) UnmarshalJSON(data []byte) error {
	type plain webSearchResult
	var value struct {
		*plain
		PublishedDate json.RawMessage `json:"publishedDate"`
	}
	*r = webSearchResult{}
	value.plain = (*plain)(r)
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	if len(value.PublishedDate) == 0 || string(value.PublishedDate) == "null" {
		return nil
	}
	if json.Unmarshal(value.PublishedDate, &r.PublishedAt) == nil {
		return nil
	}
	var date string
	if json.Unmarshal(value.PublishedDate, &date) == nil {
		for _, layout := range []string{time.RFC3339Nano, "2006-01-02"} {
			if parsed, err := time.Parse(layout, date); err == nil {
				r.PublishedAt = parsed.UnixMilli()
				break
			}
		}
	}
	return nil
}
