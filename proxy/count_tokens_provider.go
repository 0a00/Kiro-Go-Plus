package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"kiro-go/config"
	"kiro-go/internal/httpbody"
	"net/http"
	"strings"
)

func callExternalCountTokens(req *ClaudeRequest) (int, error) {
	return callExternalCountTokensContext(context.Background(), req)
}

func callExternalCountTokensContext(ctx context.Context, req *ClaudeRequest) (int, error) {
	provider := config.GetCountTokensProviderConfig()
	if !provider.Enabled || strings.TrimSpace(provider.ApiURL) == "" {
		return 0, nil
	}
	body, err := json.Marshal(req)
	if err != nil {
		return 0, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, provider.ApiURL, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if provider.ApiKey != "" {
		switch strings.ToLower(strings.TrimSpace(provider.AuthType)) {
		case "bearer":
			httpReq.Header.Set("Authorization", "Bearer "+provider.ApiKey)
		default:
			httpReq.Header.Set("x-api-key", provider.ApiKey)
		}
	}

	client, err := GetRestClientForProxy(config.GetProxyURL())
	if err != nil {
		return 0, fmt.Errorf("configure outbound proxy: %w", err)
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	respBody, readErr := httpbody.ReadAll(resp.Body, httpbody.DefaultLimit)
	if readErr != nil {
		return 0, fmt.Errorf("count_tokens response: %w", readErr)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, fmt.Errorf("remote count_tokens HTTP %d: %s", resp.StatusCode, string(respBody))
	}
	tokens, ok := extractTokenCountFromResponse(respBody)
	if !ok || tokens < 1 {
		return 0, fmt.Errorf("remote count_tokens response missing input token count")
	}
	return tokens, nil
}

func extractTokenCountFromResponse(body []byte) (int, bool) {
	var value interface{}
	if err := json.Unmarshal(body, &value); err != nil {
		return 0, false
	}
	return findTokenCount(value)
}

func findTokenCount(value interface{}) (int, bool) {
	switch v := value.(type) {
	case map[string]interface{}:
		for _, key := range []string{"input_tokens", "inputTokens", "tokens", "token_count", "tokenCount", "count"} {
			if n, ok := numberToInt(v[key]); ok {
				return n, true
			}
		}
		for _, key := range []string{"usage", "result", "data"} {
			if n, ok := findTokenCount(v[key]); ok {
				return n, true
			}
		}
	case []interface{}:
		for _, item := range v {
			if n, ok := findTokenCount(item); ok {
				return n, true
			}
		}
	}
	return 0, false
}

func numberToInt(value interface{}) (int, bool) {
	switch v := value.(type) {
	case float64:
		return int(v), true
	case int:
		return v, true
	case int64:
		return int(v), true
	case json.Number:
		n, err := v.Int64()
		return int(n), err == nil
	default:
		return 0, false
	}
}
