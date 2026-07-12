package clientcache

import (
	"net/http"
	"testing"
	"time"
)

func TestCacheIsBoundedAndExpiresEntries(t *testing.T) {
	now := time.Unix(1000, 0)
	cache := New(2, time.Minute)
	cache.now = func() time.Time { return now }
	created := 0
	create := func() *http.Client {
		created++
		return &http.Client{}
	}

	first := cache.Get("a", create)
	if again := cache.Get("a", create); again != first || created != 1 {
		t.Fatalf("expected cache hit, created=%d", created)
	}
	cache.Get("b", create)
	cache.Get("c", create)
	if cache.Len() != 2 {
		t.Fatalf("expected bounded cache size 2, got %d", cache.Len())
	}

	now = now.Add(2 * time.Minute)
	cache.Get("d", create)
	if cache.Len() != 1 {
		t.Fatalf("expected expired entries to be pruned, got %d", cache.Len())
	}
}
