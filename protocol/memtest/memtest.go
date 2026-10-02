// Package memtest measures retained heap in tests: long-lived state must
// track live data, not the history of writes that built it.
package memtest

import (
	"fmt"
	"runtime"
)

// Retained runs build between two post-GC heap readings and returns the
// growth in bytes. Keep whatever build produced alive past the call
// (runtime.KeepAlive), or its memory is not counted.
func Retained(build func()) int64 {
	before := liveHeap()
	build()
	return int64(liveHeap()) - int64(before) //nolint:gosec // heap sizes are far below 2^63
}

// Owned runs build and returns the heap only its result keeps alive: the
// live heap with it, less the live heap once it is dropped. Keep what it
// shares, such as fakes, alive past the call (runtime.KeepAlive) to cancel.
func Owned(build func() any) int64 {
	owner := []any{build()}
	with := liveHeap()
	owner[0] = nil
	without := liveHeap()
	runtime.KeepAlive(owner)
	return int64(with) - int64(without) //nolint:gosec // heap sizes are far below 2^63
}

func liveHeap() uint64 {
	// A second cycle frees what the first cycle's finalizers released.
	runtime.GC()
	runtime.GC()
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	return stats.HeapAlloc
}

// AgentGraphBody mimics the federation agent's /graph.md: a nodes table
// plus one spaceless JSON line whose nanosecond timestamps are new for
// every cycle, the shape that leaked through the section index.
func AgentGraphBody(cycle int) []byte {
	body := []byte("# Document Graph\n\n## Nodes\n\n| URL | Status |\n|---|---|\n| mark://w/a.md | ok |\n\n## Source observations\n\n```json\n[")
	for i := range 2000 {
		if i > 0 {
			body = append(body, ',')
		}
		body = fmt.Appendf(body, `{"url":"mark://w/doc-%d.md","observed_at":"2026-09-29T15:58:33.%09dZ"}`, i, cycle*2000+i)
	}
	return append(body, "]\n```\n"...)
}
