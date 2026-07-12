package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"kiro-go/config"
	"kiro-go/logger"
	"net/http"
	"sync"
	"time"
)

type healthAlertManager struct {
	mu       sync.Mutex
	lastSent map[string]time.Time
	client   *http.Client
	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	closed   bool
}

func newHealthAlertManager() *healthAlertManager {
	ctx, cancel := context.WithCancel(context.Background())
	return &healthAlertManager{
		lastSent: make(map[string]time.Time),
		client:   &http.Client{Timeout: 5 * time.Second},
		ctx:      ctx,
		cancel:   cancel,
	}
}

func (m *healthAlertManager) Notify(kind string, fields map[string]interface{}) {
	if m == nil {
		return
	}
	health := config.GetHealthConfig()
	if !health.WebhookEnabled || health.WebhookURL == "" {
		return
	}
	cooldown := time.Duration(health.WebhookCooldownSeconds) * time.Second
	if cooldown < 10*time.Second {
		cooldown = 5 * time.Minute
	}
	now := time.Now()
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	if last := m.lastSent[kind]; !last.IsZero() && now.Sub(last) < cooldown {
		m.mu.Unlock()
		return
	}
	m.lastSent[kind] = now
	m.wg.Add(1)
	m.mu.Unlock()

	payload := map[string]interface{}{
		"event":     kind,
		"timestamp": now.UTC().Format(time.RFC3339),
		"service":   "kiro-go",
		"version":   config.Version,
	}
	for key, value := range fields {
		payload[key] = value
	}
	go func() {
		defer m.wg.Done()
		m.send(health.WebhookURL, payload)
	}()
}

func (m *healthAlertManager) send(url string, payload map[string]interface{}) {
	body, err := json.Marshal(payload)
	if err != nil {
		return
	}
	req, err := http.NewRequestWithContext(m.ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.client.Do(req)
	if err != nil {
		logger.Warnf("[HealthAlert] Webhook delivery failed: %v", err)
		return
	}
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		logger.Warnf("[HealthAlert] Webhook returned HTTP %d", resp.StatusCode)
	}
}

func (m *healthAlertManager) Close() {
	if m == nil {
		return
	}
	m.mu.Lock()
	if !m.closed {
		m.closed = true
		m.cancel()
	}
	m.mu.Unlock()
	m.wg.Wait()
}
