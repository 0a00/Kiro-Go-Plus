package clientcache

import (
	"net/http"
	"sync"
	"time"
)

type entry struct {
	client   *http.Client
	lastUsed time.Time
}

// Cache is a bounded TTL cache for proxy-specific HTTP clients.
type Cache struct {
	mu      sync.Mutex
	entries map[string]entry
	max     int
	ttl     time.Duration
	now     func() time.Time
}

func New(max int, ttl time.Duration) *Cache {
	if max < 1 {
		max = 1
	}
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}
	return &Cache{entries: make(map[string]entry), max: max, ttl: ttl, now: time.Now}
}

func (c *Cache) Get(key string, create func() *http.Client) *http.Client {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	c.pruneExpiredLocked(now)
	if cached, ok := c.entries[key]; ok {
		cached.lastUsed = now
		c.entries[key] = cached
		return cached.client
	}
	if len(c.entries) >= c.max {
		c.evictOldestLocked()
	}
	client := create()
	c.entries[key] = entry{client: client, lastUsed: now}
	return client
}

func (c *Cache) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for key, cached := range c.entries {
		closeIdleConnections(cached.client)
		delete(c.entries, key)
	}
}

func (c *Cache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

func (c *Cache) pruneExpiredLocked(now time.Time) {
	for key, cached := range c.entries {
		if now.Sub(cached.lastUsed) < c.ttl {
			continue
		}
		closeIdleConnections(cached.client)
		delete(c.entries, key)
	}
}

func (c *Cache) evictOldestLocked() {
	oldestKey := ""
	var oldest time.Time
	for key, cached := range c.entries {
		if oldestKey == "" || cached.lastUsed.Before(oldest) {
			oldestKey = key
			oldest = cached.lastUsed
		}
	}
	if oldestKey == "" {
		return
	}
	closeIdleConnections(c.entries[oldestKey].client)
	delete(c.entries, oldestKey)
}

func closeIdleConnections(client *http.Client) {
	if client == nil || client.Transport == nil {
		return
	}
	if closer, ok := client.Transport.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}
