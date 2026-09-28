package changefeed

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/protocol"
)

func next(t *testing.T, s *Subscription) Event {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ev, err := s.Next(ctx)
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	return ev
}

func nextErr(t *testing.T, s *Subscription) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := s.Next(ctx)
	return err
}

// Concurrent publishers get one contiguous sequence, and a subscriber that
// started first sees every event in order.
func TestContiguousSequenceUnderConcurrentPublishers(t *testing.T) {
	hub := New("", 0)
	sub, err := hub.Subscribe("/", protocol.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	const publishers, each = 8, 200
	var wg sync.WaitGroup
	for p := range publishers {
		wg.Go(func() {
			for i := range each {
				hub.Publish(Event{Path: "/a.md", Version: p*each + i + 1, Op: protocol.OpPublish})
			}
		})
	}
	wg.Wait()
	for want := uint64(1); want <= publishers*each; want++ {
		if got := next(t, sub).Seq; got != want {
			t.Fatalf("seq = %d, want %d", got, want)
		}
	}
	if head := hub.Head(); head.Seq != publishers*each || head.Epoch != hub.Epoch() {
		t.Fatalf("head = %v", head)
	}
	if sub.Cursor() != hub.Head() {
		t.Fatalf("cursor after draining = %v, want head %v", sub.Cursor(), hub.Head())
	}
}

// A slow subscriber the ring laps gets resync; a fast one keeps receiving,
// and publishing never waited on either.
func TestSlowSubscriberResyncsWhileFastOneContinues(t *testing.T) {
	hub := New("", 8)
	fast, _ := hub.Subscribe("/", protocol.Cursor{})
	slow, _ := hub.Subscribe("/", protocol.Cursor{})
	for i := range 4 {
		hub.Publish(Event{Path: "/a.md", Version: i + 1, Op: protocol.OpPublish})
	}
	for i := range 4 {
		if got := next(t, fast).Version; got != i+1 {
			t.Fatalf("fast read version %d, want %d", got, i+1)
		}
	}
	start := time.Now()
	for i := 4; i < 20; i++ {
		hub.Publish(Event{Path: "/a.md", Version: i + 1, Op: protocol.OpPublish})
	}
	if time.Since(start) > 100*time.Millisecond {
		t.Fatal("publishing waited on a subscriber")
	}
	if err := nextErr(t, slow); !errors.Is(err, ErrResync) {
		t.Fatalf("slow subscriber: err = %v, want ErrResync", err)
	}
	// The fast subscriber was lapped too: it read 4, the ring holds 13..20.
	if err := nextErr(t, fast); !errors.Is(err, ErrResync) {
		t.Fatalf("fast subscriber after being lapped: err = %v, want ErrResync", err)
	}
	// Resumed from the head, it keeps receiving.
	fast, _ = hub.Subscribe("/", protocol.Cursor{})
	hub.Publish(Event{Path: "/a.md", Version: 21, Op: protocol.OpPublish})
	if got := next(t, fast).Version; got != 21 {
		t.Fatalf("resumed fast subscriber read version %d, want 21", got)
	}
}

func TestResumeFromCursor(t *testing.T) {
	// Ring of 4 after 10 publishes holds 7..10; 6 is the newest dropped:
	// resumable, everything after it is retained. 5 is not.
	fresh := func() *Hub {
		hub := New("w", 4)
		for i := 1; i <= 10; i++ {
			hub.Publish(Event{Path: "/a.md", Version: i, Op: protocol.OpPublish})
		}
		return hub
	}
	tests := []struct {
		name  string
		since protocol.Cursor
		want  uint64 // first Seq delivered; 0 means resync
	}{
		{"after the newest dropped", protocol.Cursor{Epoch: "w", Seq: 6}, 7},
		{"inside the ring", protocol.Cursor{Epoch: "w", Seq: 8}, 9},
		{"at the head", protocol.Cursor{Epoch: "w", Seq: 10}, 11},
		{"before the ring", protocol.Cursor{Epoch: "w", Seq: 5}, 0},
		{"past the head", protocol.Cursor{Epoch: "w", Seq: 11}, 0},
		{"other epoch", protocol.Cursor{Epoch: "x", Seq: 8}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hub := fresh()
			sub, err := hub.Subscribe("/", tt.since)
			if tt.want == 0 {
				if !errors.Is(err, ErrResync) {
					t.Fatalf("err = %v, want ErrResync", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Subscribe: %v", err)
			}
			if sub.Cursor() != tt.since {
				t.Fatalf("cursor = %v, want the since cursor %v", sub.Cursor(), tt.since)
			}
			if tt.want > 10 {
				hub.Publish(Event{Path: "/a.md", Version: 11, Op: protocol.OpPublish})
			}
			if got := next(t, sub).Seq; got != tt.want {
				t.Fatalf("first seq = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestScopeFilterAndCursorAdvance(t *testing.T) {
	hub := New("w", 0)
	doc, _ := hub.Subscribe("/a/b.md", protocol.Cursor{})
	tree, _ := hub.Subscribe("/a/", protocol.Cursor{})
	all, _ := hub.Subscribe("/", protocol.Cursor{})
	hub.Publish(Event{Path: "/a/b.md", Version: 1, Op: protocol.OpPublish})     // 1
	hub.Publish(Event{Path: "/a/c.md", Version: 1, Op: protocol.OpPublish})     // 2
	hub.Publish(Event{Path: "/ab.md", Version: 1, Op: protocol.OpPublish})      // 3
	hub.Publish(Event{Path: "/a/b.md/x.md", Version: 1, Op: protocol.OpAppend}) // 4
	hub.Publish(Event{Path: "/a/b.md", Version: 2, Op: protocol.OpArchive})     // 5

	if got := next(t, doc).Seq; got != 1 {
		t.Fatalf("document scope first seq = %d", got)
	}
	if got := next(t, doc).Seq; got != 5 {
		t.Fatalf("document scope second seq = %d, want 5 (a path with the document as prefix is not it)", got)
	}
	if doc.Cursor().Seq != 5 {
		t.Fatalf("document cursor = %v", doc.Cursor())
	}
	for _, want := range []uint64{1, 2, 4, 5} {
		if got := next(t, tree).Seq; got != want {
			t.Fatalf("tree scope seq = %d, want %d", got, want)
		}
	}
	for want := uint64(1); want <= 5; want++ {
		if got := next(t, all).Seq; got != want {
			t.Fatalf("root scope seq = %d, want %d", got, want)
		}
	}
	// Skipped events still advance the resume cursor.
	hub.Publish(Event{Path: "/zzz.md", Version: 1, Op: protocol.OpPublish}) // 6
	if err := nextErr(t, doc); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the wait deadline", err)
	}
	if doc.Cursor().Seq != 6 {
		t.Fatalf("cursor after skipping = %v, want seq 6", doc.Cursor())
	}
}

func TestCloseEndsSubscribers(t *testing.T) {
	hub := New("w", 0)
	sub, _ := hub.Subscribe("/", protocol.Cursor{})
	done := make(chan error, 1)
	go func() {
		_, err := sub.Next(context.Background())
		done <- err
	}()
	time.Sleep(10 * time.Millisecond)
	hub.Close()
	select {
	case err := <-done:
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("err = %v, want ErrClosed", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not wake the subscriber")
	}
	hub.Close() // idempotent
}

func TestNewEpochFitsTheCursorGrammar(t *testing.T) {
	hub := New("", 0)
	if _, err := protocol.ParseCursor(hub.Head().String()); err != nil {
		t.Fatalf("random epoch does not parse: %v", err)
	}
	if New("", 0).Epoch() == hub.Epoch() {
		t.Fatal("two hubs share an epoch")
	}
}

// A caller's own sequence may leave gaps, which readers step over; a Skip
// makes what lies before it unresumable.
func TestPublishAtAndSkip(t *testing.T) {
	hub := New("w", 0)
	if !hub.PublishAt(Event{Seq: 5, Path: "/a.md", Version: 1, Op: protocol.OpPublish}) {
		t.Fatal("PublishAt(5) on an empty hub was dropped")
	}
	if hub.PublishAt(Event{Seq: 3, Path: "/a.md", Version: 1, Op: protocol.OpPublish}) {
		t.Fatal("PublishAt(3) behind the head was accepted")
	}
	resumed, err := hub.Subscribe("/", protocol.Cursor{Epoch: "w", Seq: 5})
	if err != nil {
		t.Fatal(err)
	}
	hub.PublishAt(Event{Seq: 7, Path: "/b.md", Version: 1, Op: protocol.OpPublish})
	if got := next(t, resumed); got.Seq != 7 || got.Path != "/b.md" {
		t.Fatalf("after the gap got %+v, want seq 7", got)
	}
	if resumed.Cursor().Seq != 7 {
		t.Fatalf("cursor = %v", resumed.Cursor())
	}

	hub.Skip(20)
	if head := hub.Head(); head.Seq != 20 {
		t.Fatalf("head after Skip = %v", head)
	}
	if err := nextErr(t, resumed); !errors.Is(err, ErrResync) {
		t.Fatalf("subscriber behind a skip got %v, want ErrResync", err)
	}
	if _, err := hub.Subscribe("/", protocol.Cursor{Epoch: "w", Seq: 19}); !errors.Is(err, ErrResync) {
		t.Fatalf("resume before the skip: %v, want ErrResync", err)
	}
	at, err := hub.Subscribe("/", protocol.Cursor{Epoch: "w", Seq: 20})
	if err != nil {
		t.Fatalf("resume at the skip: %v", err)
	}
	hub.PublishAt(Event{Seq: 21, Path: "/c.md", Version: 1, Op: protocol.OpPublish})
	if got := next(t, at); got.Seq != 21 {
		t.Fatalf("after the skip got %+v, want seq 21", got)
	}

	// A gap wider than the ring evicts what came before it.
	small := New("w", 4)
	small.PublishAt(Event{Seq: 1, Path: "/a.md", Version: 1, Op: protocol.OpPublish})
	small.PublishAt(Event{Seq: 10, Path: "/a.md", Version: 2, Op: protocol.OpPublish})
	if _, err := small.Subscribe("/", protocol.Cursor{Epoch: "w", Seq: 1}); !errors.Is(err, ErrResync) {
		t.Fatalf("resume across a ring-wide gap: %v, want ErrResync", err)
	}
	late, err := small.Subscribe("/", protocol.Cursor{Epoch: "w", Seq: 8})
	if err != nil {
		t.Fatal(err)
	}
	if got := next(t, late).Seq; got != 10 {
		t.Fatalf("seq after a wide gap = %d, want 10", got)
	}
}
