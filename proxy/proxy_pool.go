package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"kiro-go/config"
	"kiro-go/internal/outboundproxy"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	proxyPoolProbeTimeout = 8 * time.Second
	proxyPoolProbeURL     = "https://runtime.us-east-1.kiro.dev/"
)

type proxyPoolEntryResponse struct {
	ID                  string `json:"id"`
	ProxyURL            string `json:"proxyURL"`
	ProxyPasswordSet    bool   `json:"proxyPasswordSet"`
	Label               string `json:"label,omitempty"`
	Enabled             bool   `json:"enabled"`
	Health              string `json:"health,omitempty"`
	LatencyMs           int    `json:"latencyMs,omitempty"`
	LastCheckedAt       int64  `json:"lastCheckedAt,omitempty"`
	ConsecutiveFailures int    `json:"consecutiveFailures,omitempty"`
}

type proxyPoolProbeResult struct {
	ID        string
	Health    string
	LatencyMs int
	Error     string
}

var proxyPoolProbeHTTPClient = func(proxyURL string) (*http.Client, error) {
	return GetClientForProxy(proxyURL)
}

var proxyPoolProbeDo = func(ctx context.Context, client *http.Client, rawURL string) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodHead, rawURL, nil)
	if err != nil {
		return nil, err
	}
	return client.Do(request)
}

func normalizeProxyPoolLine(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.EqualFold(raw, "direct") {
		return "", fmt.Errorf("proxy URL cannot be empty or direct")
	}
	if strings.Contains(raw, "://") {
		if err := outboundproxy.Validate(raw); err != nil {
			return "", err
		}
		return raw, nil
	}

	parts := strings.Split(raw, ":")
	if len(parts) == 2 {
		candidate := "http://" + raw
		if err := outboundproxy.Validate(candidate); err != nil {
			return "", err
		}
		return candidate, nil
	}
	if len(parts) < 4 {
		return "", fmt.Errorf("proxy must be a URL or host:port[:user:password]")
	}
	host := strings.Trim(strings.TrimSpace(parts[0]), "[]")
	port := strings.TrimSpace(parts[1])
	user := strings.TrimSpace(parts[2])
	password := strings.Join(parts[3:], ":")
	if host == "" || port == "" || user == "" || password == "" {
		return "", fmt.Errorf("proxy host, port, username, and password are required")
	}
	candidate := (&url.URL{
		Scheme: "socks5h",
		Host:   net.JoinHostPort(host, port),
		User:   url.UserPassword(user, password),
	}).String()
	if err := outboundproxy.Validate(candidate); err != nil {
		return "", err
	}
	return candidate, nil
}

func proxyPoolResponse(entry config.ProxyPoolEntry) proxyPoolEntryResponse {
	safe, passwordSet := sanitizedProxyURL(entry.ProxyURL)
	return proxyPoolEntryResponse{
		ID:                  entry.ID,
		ProxyURL:            safe,
		ProxyPasswordSet:    passwordSet,
		Label:               entry.Label,
		Enabled:             entry.Enabled,
		Health:              entry.Health,
		LatencyMs:           entry.LatencyMs,
		LastCheckedAt:       entry.LastCheckedAt,
		ConsecutiveFailures: entry.ConsecutiveFailures,
	}
}

func probeProxyPoolEntry(ctx context.Context, entry config.ProxyPoolEntry) proxyPoolProbeResult {
	result := proxyPoolProbeResult{ID: entry.ID, Health: "unhealthy"}
	client, err := proxyPoolProbeHTTPClient(entry.ProxyURL)
	if err != nil {
		result.Error = redactDiagnosticText(err.Error())
		return result
	}
	probeCtx, cancel := context.WithTimeout(ctx, proxyPoolProbeTimeout)
	defer cancel()
	started := time.Now()
	response, err := proxyPoolProbeDo(probeCtx, client, proxyPoolProbeURL)
	if err != nil {
		result.Error = redactDiagnosticText(err.Error())
		return result
	}
	if response != nil && response.Body != nil {
		response.Body.Close()
	}
	if response != nil && response.StatusCode == http.StatusProxyAuthRequired {
		result.Error = "proxy authentication failed"
		return result
	}
	result.Health = "healthy"
	result.LatencyMs = int(time.Since(started).Milliseconds())
	return result
}

func probeProxyPoolEntries(ctx context.Context, entries []config.ProxyPoolEntry) []proxyPoolProbeResult {
	results := make([]proxyPoolProbeResult, len(entries))
	jobs := make(chan int)
	workers := 16
	if len(entries) < workers {
		workers = len(entries)
	}
	if workers == 0 {
		return results
	}
	var group sync.WaitGroup
	for i := 0; i < workers; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			for index := range jobs {
				results[index] = probeProxyPoolEntry(ctx, entries[index])
			}
		}()
	}
	for i := range entries {
		jobs <- i
	}
	close(jobs)
	group.Wait()
	return results
}

func (h *Handler) apiGetProxyPool(w http.ResponseWriter, r *http.Request) {
	entries := config.GetProxyPoolEntries()
	response := make([]proxyPoolEntryResponse, 0, len(entries))
	for _, entry := range entries {
		response = append(response, proxyPoolResponse(entry))
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"entries":              response,
		"credentialEncryption": config.CredentialEncryptionEnabled(),
	})
}

func (h *Handler) apiUpdateProxyPool(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Action     string   `json:"action"`
		Proxies    []string `json:"proxies"`
		ProxyID    string   `json:"proxyId"`
		AccountIDs []string `json:"accountIds"`
		RoundRobin bool     `json:"roundRobin"`
		CheckAll   bool     `json:"checkAll"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	action := strings.ToLower(strings.TrimSpace(request.Action))
	switch action {
	case "add":
		h.addProxyPoolEntries(w, request.Proxies)
	case "check":
		h.checkProxyPoolEntries(w, request.CheckAll)
	case "assign":
		h.assignProxyPoolEntries(w, request.ProxyID, request.AccountIDs, request.RoundRobin)
	default:
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "action must be add, check, or assign"})
	}
}

func (h *Handler) addProxyPoolEntries(w http.ResponseWriter, values []string) {
	if len(values) == 0 || len(values) > 100000 {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "proxies must contain between 1 and 100000 entries"})
		return
	}
	entries := config.GetProxyPoolEntries()
	seen := make(map[string]struct{}, len(entries)+len(values))
	for _, entry := range entries {
		seen[entry.ProxyURL] = struct{}{}
	}
	newEntries := make([]config.ProxyPoolEntry, 0, len(values))
	errors := make([]string, 0)
	for index, raw := range values {
		normalized, err := normalizeProxyPoolLine(raw)
		if err != nil {
			errors = append(errors, fmt.Sprintf("line %d: %s", index+1, err.Error()))
			continue
		}
		if _, exists := seen[normalized]; exists {
			continue
		}
		seen[normalized] = struct{}{}
		newEntries = append(newEntries, config.ProxyPoolEntry{
			ID:       uuid.NewString(),
			ProxyURL: normalized,
			Enabled:  true,
			Health:   "unknown",
		})
	}
	added, addErr := config.AppendProxyPoolEntries(newEntries)
	if addErr != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]interface{}{"error": addErr.Error(), "added": 0, "errors": errors})
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": len(errors) == 0, "added": added, "errors": errors})
}

func (h *Handler) checkProxyPoolEntries(w http.ResponseWriter, checkAll bool) {
	entries := config.GetProxyPoolEntries()
	if !checkAll && len(entries) > 1000 {
		entries = entries[:1000]
	}
	results := probeProxyPoolEntries(context.Background(), entries)
	byID := make(map[string]proxyPoolProbeResult, len(results))
	for _, result := range results {
		byID[result.ID] = result
	}
	for i := range entries {
		result := byID[entries[i].ID]
		entries[i].Health = result.Health
		entries[i].LatencyMs = result.LatencyMs
		entries[i].LastCheckedAt = time.Now().Unix()
		if result.Health == "healthy" {
			entries[i].ConsecutiveFailures = 0
		} else {
			entries[i].ConsecutiveFailures++
		}
	}
	updates := make(map[string]config.ProxyPoolHealthUpdate, len(entries))
	for _, entry := range entries {
		result := byID[entry.ID]
		updates[entry.ID] = config.ProxyPoolHealthUpdate{
			ID:                  entry.ID,
			Health:              result.Health,
			LatencyMs:           result.LatencyMs,
			LastCheckedAt:       entry.LastCheckedAt,
			ConsecutiveFailures: entry.ConsecutiveFailures,
		}
	}
	if err := config.UpdateProxyPoolHealth(updates); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "results": results})
}

func (h *Handler) assignProxyPoolEntries(w http.ResponseWriter, proxyID string, accountIDs []string, roundRobin bool) {
	entries := config.GetProxyPoolEntries()
	assignable := make([]config.ProxyPoolEntry, 0, len(entries))
	for _, entry := range entries {
		if !entry.Enabled || strings.EqualFold(entry.Health, "unhealthy") {
			continue
		}
		assignable = append(assignable, entry)
	}
	if strings.TrimSpace(proxyID) != "" {
		assignable = assignable[:0]
		for _, entry := range entries {
			if entry.ID == proxyID {
				assignable = append(assignable, entry)
				break
			}
		}
	}
	if len(assignable) == 0 {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "no enabled healthy proxy is available"})
		return
	}
	accounts := config.GetAccounts()
	selected := make(map[string]bool, len(accountIDs))
	for _, id := range accountIDs {
		selected[strings.TrimSpace(id)] = true
	}
	updates := make(map[string]string)
	position := 0
	for _, account := range accounts {
		if len(selected) > 0 && !selected[account.ID] {
			continue
		}
		entry := assignable[0]
		if roundRobin || strings.TrimSpace(proxyID) == "" {
			entry = assignable[position%len(assignable)]
			position++
		}
		updates[account.ID] = entry.ProxyURL
	}
	if len(updates) == 0 {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "no matching accounts"})
		return
	}
	if err := config.UpdateAccountProxyURLs(updates); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	if h.pool != nil {
		h.pool.Reload()
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "assigned": len(updates), "proxyCount": len(assignable)})
}

func (h *Handler) apiDeleteProxyPoolEntry(w http.ResponseWriter, r *http.Request, id string) {
	found, err := config.DeleteProxyPoolEntry(id)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	if !found {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"error": "proxy pool entry not found"})
		return
	}
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}
