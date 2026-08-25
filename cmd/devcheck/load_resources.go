package main

import "runtime"

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
	if after >= before {
		return int64(after - before)
	}
	return -int64(before - after)
}
