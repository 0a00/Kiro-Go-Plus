package main

import (
	"context"
	"runtime"
	"sync"
	"time"
)

type loadResourceSnapshot struct {
	goroutines int
	heapAlloc  uint64
}

func readLoadResourceSnapshot() loadResourceSnapshot {
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)
	return loadResourceSnapshot{goroutines: runtime.NumGoroutine(), heapAlloc: memory.HeapAlloc}
}

func signedUint64Delta(after, before uint64) int64 {
	maxInt64 := uint64(^uint64(0) >> 1)
	if after >= before {
		delta := after - before
		if delta > maxInt64 {
			return int64(maxInt64)
		}
		return int64(delta)
	}
	delta := before - after
	if delta > maxInt64 {
		return -int64(maxInt64)
	}
	return -int64(delta)
}

// loadResourceSampler records the load generator's own resource trend. It is
// deliberately separate from server metrics: a saturated generator must not be
// mistaken for a saturated Kiro-Go Plus instance.
type loadResourceSampler struct {
	mu             sync.Mutex
	samples        int
	peakGoroutines int
	peakHeapAlloc  uint64
	stopCh         chan struct{}
	doneCh         chan struct{}
	stopOnce       sync.Once
}

func newLoadResourceSampler(ctx context.Context, interval time.Duration) *loadResourceSampler {
	if ctx == nil {
		ctx = context.Background()
	}
	sampler := &loadResourceSampler{}
	sampler.observe(readLoadResourceSnapshot())
	if interval <= 0 {
		return sampler
	}
	sampler.stopCh = make(chan struct{})
	sampler.doneCh = make(chan struct{})
	go func() {
		defer close(sampler.doneCh)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				sampler.observe(readLoadResourceSnapshot())
			case <-sampler.stopCh:
				return
			case <-ctx.Done():
				return
			}
		}
	}()
	return sampler
}

func (s *loadResourceSampler) observe(snapshot loadResourceSnapshot) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.samples++
	if snapshot.goroutines > s.peakGoroutines {
		s.peakGoroutines = snapshot.goroutines
	}
	if snapshot.heapAlloc > s.peakHeapAlloc {
		s.peakHeapAlloc = snapshot.heapAlloc
	}
	s.mu.Unlock()
}

func (s *loadResourceSampler) stop() {
	if s == nil {
		return
	}
	s.stopOnce.Do(func() {
		if s.stopCh != nil {
			close(s.stopCh)
			<-s.doneCh
		}
		s.observe(readLoadResourceSnapshot())
	})
}

func (s *loadResourceSampler) summary() (samples, peakGoroutines int, peakHeapAlloc uint64) {
	if s == nil {
		return 0, 0, 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.samples, s.peakGoroutines, s.peakHeapAlloc
}
