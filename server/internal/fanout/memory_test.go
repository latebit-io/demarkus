package fanout

import (
	"bytes"
	"context"
	"fmt"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/memtest"
	"github.com/latebit-io/demarkus/server/internal/changefeed"
)

// countingWriter discards what a watcher writes and counts the blocks.
type countingWriter struct{ blocks atomic.Int64 }

func (c *countingWriter) Write(p []byte) (int, error) {
	c.blocks.Add(int64(bytes.Count(p, fence) / 2))
	return len(p), nil
}

var fence = []byte("---\n")

// agentEvent is a production-shaped hint: a deep path, a fresh hash and a
// long agent id on every write.
func agentEvent(hub *changefeed.Hub, n int) changefeed.Event {
	return changefeed.Event{
		Seq:     hub.Head().Seq + 1,
		Path:    agentPath(n),
		Version: n%40 + 1,
		Hash:    fmt.Sprintf("sha256-%064x", n),
		Op:      protocol.OpAppend,
		Agent:   fmt.Sprintf("federation-agent-%d-2026-09-30T15:58:33.%09dZ", n%3, n),
	}
}

func agentPath(n int) string { return fmt.Sprintf("/agents/session-%d/inbox/01HZX%08d.md", n%7, n) }

// served runs n watches on scope with their own writers and returns the
// writers, once every acknowledgement is written, and a stop.
func served(t *testing.T, f *Fanout, n int, scope string) (writers []*countingWriter, stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	writers = make([]*countingWriter, n)
	done := make(chan error, n)
	for i := range n {
		writers[i] = &countingWriter{}
		go func() { done <- f.Serve(ctx, Request{Scope: scope}, writers[i]) }()
	}
	waitDelivered(t, writers, 0)
	return writers, func() {
		cancel()
		for range n {
			if err := <-done; err != nil {
				t.Errorf("Serve: %v", err)
			}
		}
	}
}

// waitDelivered spins until every writer has seen the ack and want events.
func waitDelivered(t *testing.T, writers []*countingWriter, want int64) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for _, w := range writers {
		for w.blocks.Load() < want+1 {
			if time.Now().After(deadline) {
				t.Fatalf("watcher delivered %d of %d blocks", w.blocks.Load(), want)
			}
			time.Sleep(100 * time.Microsecond)
		}
	}
}

// A watched world holds one ring of encoded events and nothing per write:
// five rings' worth of agent traffic through 200 watchers retains no more
// than one ring, and nothing once the watchers are gone.
func TestFanoutRetainsOneRingWhileWatched(t *testing.T) {
	const ring, watchers = 512, 200
	hub := changefeed.New("w", ring)
	f := newFanout(t, hub, Config{Heartbeat: time.Hour})
	writers, stop := served(t, f, watchers, "/agents/")
	// Bursts stay under the ring, so no watcher is lapped into a resync.
	var published int64
	cycle := func(events int) {
		for events > 0 {
			for range min(events, ring/4) {
				hub.PublishAt(agentEvent(hub, int(published)))
				published++
			}
			events -= ring / 4
			waitDelivered(t, writers, published)
		}
	}
	cycle(ring)
	growth := memtest.Retained(func() { cycle(5 * ring) })
	runtime.KeepAlive(f)
	t.Logf("heap grew %d bytes over %d events to %d watchers", growth, 5*ring, watchers)
	// What may grow: the ring's blocks (about 300 bytes each) and each
	// watcher's kept scratch; nothing per event delivered.
	limit := int64(ring*400 + watchers*(keepBufBytes+keepPending*100))
	if growth > limit {
		t.Errorf("heap grew %d bytes over five rings of events, want under %d", growth, limit)
	}
	stop()
	after := memtest.Retained(func() {})
	runtime.KeepAlive(f)
	if running(f) || f.Watches() != 0 {
		t.Fatalf("watchers gone but fanout still running: %d watches", f.Watches())
	}
	t.Logf("idle heap delta %d bytes", after)
}

// Delivering one event costs the reader a fixed number of allocations;
// each further watcher adds none.
func TestDeliveryAllocatesNothingPerWatcher(t *testing.T) {
	hub := changefeed.New("w", 0)
	f := newFanout(t, hub, Config{Heartbeat: time.Hour})
	allocsWith := func(watchers int) float64 {
		writers, stop := served(t, f, watchers, "/")
		defer stop()
		var published int64
		publishAndWait := func() {
			hub.PublishAt(agentEvent(hub, int(published)))
			published++
			waitDelivered(t, writers, published)
		}
		for range 50 {
			publishAndWait()
		}
		return testing.AllocsPerRun(200, publishAndWait)
	}
	one, many := allocsWith(1), allocsWith(64)
	t.Logf("allocations per event: %.1f with 1 watcher, %.1f with 64", one, many)
	if perWatcher := (many - one) / 63; perWatcher > 0.1 {
		t.Errorf("each further watcher costs %.2f allocations per event, want 0", perWatcher)
	}
}

// largestWriter counts blocks and keeps the largest single write.
type largestWriter struct {
	countingWriter
	largest atomic.Int64
}

func (w *largestWriter) Write(p []byte) (int, error) {
	if n := int64(len(p)); n > w.largest.Load() {
		w.largest.Store(n)
	}
	return w.countingWriter.Write(p)
}

// A resume far behind is written a batch at a time, so the watcher holds one
// batch of the backlog, not all of it.
func TestCatchUpWritesABatchAtATime(t *testing.T) {
	const events = 1200
	hub, _ := pagedHub(events)
	f := newFanout(t, hub, Config{})
	writer := &largestWriter{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.Serve(ctx, Request{Scope: "/", Since: protocol.Cursor{Epoch: "w", Seq: 2}}, writer) }()
	waitDelivered(t, []*countingWriter{&writer.countingWriter}, events-2)
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Serve: %v", err)
	}
	// One block past the batch bound at most: the one that crossed it.
	if largest := writer.largest.Load(); largest > maxBatchBytes+1024 {
		t.Fatalf("largest write = %d bytes, want at most a batch (%d) and one block", largest, maxBatchBytes)
	}
}
