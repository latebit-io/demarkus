package changefeed

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/protocol"
)

// stamper numbers events the way a store does: under its commit lock, so
// sequences reach the hub in order. The hub takes only committed sequences.
type stamper struct {
	mu   sync.Mutex
	last uint64
}

func (s *stamper) publish(hub *Hub, ev Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.last++
	ev.Seq = s.last
	hub.PublishAt(ev)
}

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
	var st stamper
	sub, err := hub.Subscribe(t.Context(), "/", protocol.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	const publishers, each = 8, 200
	var wg sync.WaitGroup
	for p := range publishers {
		wg.Go(func() {
			for i := range each {
				st.publish(hub, Event{Path: "/a.md", Version: p*each + i + 1, Op: protocol.OpPublish})
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
	var st stamper
	fast, _ := hub.Subscribe(t.Context(), "/", protocol.Cursor{})
	slow, _ := hub.Subscribe(t.Context(), "/", protocol.Cursor{})
	for i := range 4 {
		st.publish(hub, Event{Path: "/a.md", Version: i + 1, Op: protocol.OpPublish})
	}
	for i := range 4 {
		if got := next(t, fast).Version; got != i+1 {
			t.Fatalf("fast read version %d, want %d", got, i+1)
		}
	}
	start := time.Now()
	for i := 4; i < 20; i++ {
		st.publish(hub, Event{Path: "/a.md", Version: i + 1, Op: protocol.OpPublish})
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
	fast, _ = hub.Subscribe(t.Context(), "/", protocol.Cursor{})
	st.publish(hub, Event{Path: "/a.md", Version: 21, Op: protocol.OpPublish})
	if got := next(t, fast).Version; got != 21 {
		t.Fatalf("resumed fast subscriber read version %d, want 21", got)
	}
}

func TestResumeFromCursor(t *testing.T) {
	// Ring of 4 after 10 publishes holds 7..10; 6 is the newest dropped:
	// resumable, everything after it is retained. 5 is not.
	fresh := func() *Hub {
		hub := New("w", 4)
		var st stamper
		for i := 1; i <= 10; i++ {
			st.publish(hub, Event{Path: "/a.md", Version: i, Op: protocol.OpPublish})
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
			sub, err := hub.Subscribe(t.Context(), "/", tt.since)
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
				hub.PublishAt(Event{Seq: 11, Path: "/a.md", Version: 11, Op: protocol.OpPublish})
			}
			if got := next(t, sub).Seq; got != tt.want {
				t.Fatalf("first seq = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestScopeFilterAndCursorAdvance(t *testing.T) {
	hub := New("w", 0)
	var st stamper
	doc, _ := hub.Subscribe(t.Context(), "/a/b.md", protocol.Cursor{})
	tree, _ := hub.Subscribe(t.Context(), "/a/", protocol.Cursor{})
	all, _ := hub.Subscribe(t.Context(), "/", protocol.Cursor{})
	st.publish(hub, Event{Path: "/a/b.md", Version: 1, Op: protocol.OpPublish})     // 1
	st.publish(hub, Event{Path: "/a/c.md", Version: 1, Op: protocol.OpPublish})     // 2
	st.publish(hub, Event{Path: "/ab.md", Version: 1, Op: protocol.OpPublish})      // 3
	st.publish(hub, Event{Path: "/a/b.md/x.md", Version: 1, Op: protocol.OpAppend}) // 4
	st.publish(hub, Event{Path: "/a/b.md", Version: 2, Op: protocol.OpArchive})     // 5

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
	st.publish(hub, Event{Path: "/zzz.md", Version: 1, Op: protocol.OpPublish}) // 6
	if err := nextErr(t, doc); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the wait deadline", err)
	}
	if doc.Cursor().Seq != 6 {
		t.Fatalf("cursor after skipping = %v, want seq 6", doc.Cursor())
	}
}

func TestCloseEndsSubscribers(t *testing.T) {
	hub := New("w", 0)
	sub, _ := hub.Subscribe(t.Context(), "/", protocol.Cursor{})
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

// A gap in the caller's sequence is unresumable, like a Skip: nothing before
// the gap can be named, so a cursor or a reader inside it gets resync.
func TestPublishAtAndSkip(t *testing.T) {
	hub := New("w", 0)
	if !hub.PublishAt(Event{Seq: 5, Path: "/a.md", Version: 1, Op: protocol.OpPublish}) {
		t.Fatal("PublishAt(5) on an empty hub was dropped")
	}
	if hub.PublishAt(Event{Seq: 3, Path: "/a.md", Version: 1, Op: protocol.OpPublish}) {
		t.Fatal("PublishAt(3) behind the head was accepted")
	}
	if _, err := hub.Subscribe(t.Context(), "/", protocol.Cursor{Epoch: "w", Seq: 3}); !errors.Is(err, ErrResync) {
		t.Fatalf("resume before the first gap: err = %v, want ErrResync", err)
	}
	resumed, err := hub.Subscribe(t.Context(), "/", protocol.Cursor{Epoch: "w", Seq: 5})
	if err != nil {
		t.Fatal(err)
	}
	hub.PublishAt(Event{Seq: 7, Path: "/b.md", Version: 1, Op: protocol.OpPublish})
	if err := nextErr(t, resumed); !errors.Is(err, ErrResync) {
		t.Fatalf("reader inside the gap: err = %v, want ErrResync", err)
	}
	after, err := hub.Subscribe(t.Context(), "/", protocol.Cursor{Epoch: "w", Seq: 6})
	if err != nil {
		t.Fatal(err)
	}
	if got := next(t, after).Seq; got != 7 {
		t.Fatalf("after the gap got seq %d, want 7", got)
	}

	hub.Skip(20)
	if head := hub.Head(); head.Seq != 20 {
		t.Fatalf("head after Skip = %v", head)
	}
	if err := nextErr(t, resumed); !errors.Is(err, ErrResync) {
		t.Fatalf("subscriber behind a skip got %v, want ErrResync", err)
	}
	if _, err := hub.Subscribe(t.Context(), "/", protocol.Cursor{Epoch: "w", Seq: 19}); !errors.Is(err, ErrResync) {
		t.Fatalf("resume before the skip: %v, want ErrResync", err)
	}
	at, err := hub.Subscribe(t.Context(), "/", protocol.Cursor{Epoch: "w", Seq: 20})
	if err != nil {
		t.Fatalf("resume at the skip: %v", err)
	}
	hub.PublishAt(Event{Seq: 21, Path: "/c.md", Version: 1, Op: protocol.OpPublish})
	if got := next(t, at); got.Seq != 21 {
		t.Fatalf("after the skip got %+v, want seq 21", got)
	}

	// Only the gap's far edge resumes, whatever the ring holds.
	small := New("w", 4)
	small.PublishAt(Event{Seq: 1, Path: "/a.md", Version: 1, Op: protocol.OpPublish})
	small.PublishAt(Event{Seq: 10, Path: "/a.md", Version: 2, Op: protocol.OpPublish})
	for _, seq := range []uint64{1, 8} {
		if _, err := small.Subscribe(t.Context(), "/", protocol.Cursor{Epoch: "w", Seq: seq}); !errors.Is(err, ErrResync) {
			t.Fatalf("resume at %d inside a gap: %v, want ErrResync", seq, err)
		}
	}
	late, err := small.Subscribe(t.Context(), "/", protocol.Cursor{Epoch: "w", Seq: 9})
	if err != nil {
		t.Fatal(err)
	}
	if got := next(t, late).Seq; got != 10 {
		t.Fatalf("seq after the gap = %d, want 10", got)
	}
}

// recordBacklog answers from a store's full history, counting reads; during
// runs inside the read, as a publish racing the load would.
type recordBacklog struct {
	history []Event // history[i] has Seq i+1
	err     error
	during  func()
	reads   int
	// peer runs on CatchUp, as a poll that finds a peer's commits would.
	peer       func()
	catchUpErr error
	catchUps   int
}

func (b *recordBacklog) CatchUp(ctx context.Context) error {
	b.catchUps++
	if b.peer != nil {
		b.peer()
	}
	if err := ctx.Err(); err != nil {
		return err // as a poll under an ended context fails
	}
	return b.catchUpErr
}

func (b *recordBacklog) Events(_ context.Context, after, through uint64) ([]Event, error) {
	b.reads++
	if b.during != nil {
		b.during()
	}
	if b.err != nil {
		return nil, b.err
	}
	return b.history[after:through], nil
}

// backlogHub is a reopened store's hub: 10 committed events alternating
// between /a/ and /b/, all in the backlog, the ring of 8 replayed from 7.
func backlogHub() (*Hub, *recordBacklog) {
	backlog := &recordBacklog{}
	hub := NewWithBacklog("w", 8, backlog)
	hub.Skip(6)
	for seq := uint64(1); seq <= 10; seq++ {
		path := "/a/x.md"
		if seq%2 == 0 {
			path = "/b/x.md"
		}
		ev := Event{Seq: seq, Path: path, Version: int(seq), Op: protocol.OpPublish}
		backlog.history = append(backlog.history, ev)
		if seq > 6 {
			hub.PublishAt(ev)
		}
	}
	return hub, backlog
}

// A resume older than the ring reads the backlog once, serves it in scope
// before the ring, and its cursor tracks every event consumed.
func TestResumeReadsBacklogBeforeRing(t *testing.T) {
	hub, backlog := backlogHub()
	sub, err := hub.Subscribe(t.Context(), "/a/", protocol.Cursor{Epoch: "w", Seq: 2})
	if err != nil {
		t.Fatal(err)
	}
	if sub.Cursor().Seq != 2 {
		t.Fatalf("cursor before reading = %d, want 2", sub.Cursor().Seq)
	}
	// The cursor sits just before the next event in scope: past 4 once 3
	// is read, at the backlog's end (6) once 5 is.
	for _, step := range []struct{ event, cursor uint64 }{{3, 4}, {5, 6}, {7, 7}, {9, 9}} {
		if got := next(t, sub); got.Seq != step.event {
			t.Fatalf("event = %+v, want seq %d", got, step.event)
		}
		if sub.Cursor().Seq != step.cursor {
			t.Fatalf("cursor after %d = %d, want %d", step.event, sub.Cursor().Seq, step.cursor)
		}
	}
	if sub.backlog != nil {
		t.Fatalf("backlog array still held after it was read: %d left", len(sub.backlog))
	}
	if err := nextErr(t, sub); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("after the head: %v", err)
	}
	if sub.Cursor().Seq != 10 {
		t.Fatalf("cursor at the head = %d, want 10", sub.Cursor().Seq)
	}
	if backlog.reads != 1 {
		t.Fatalf("backlog reads = %d, want 1", backlog.reads)
	}

	// Inside the ring the backlog is not read.
	if _, err := hub.Subscribe(t.Context(), "/", protocol.Cursor{Epoch: "w", Seq: 6}); err != nil {
		t.Fatal(err)
	}
	if backlog.reads != 1 {
		t.Fatalf("backlog read for a resume inside the ring")
	}
}

// Every resume the backlog cannot serve exactly is a resync.
func TestResumeBacklogFailuresResync(t *testing.T) {
	tests := []struct {
		name  string
		since uint64
		setup func(hub *Hub, backlog *recordBacklog)
		reads int
	}{
		{name: "lag past the ring", since: 1, reads: 0, setup: func(*Hub, *recordBacklog) {}},
		{name: "backlog error", since: 2, reads: 1, setup: func(_ *Hub, backlog *recordBacklog) {
			backlog.err = errors.New("bucket unavailable")
		}},
		{name: "short backlog", since: 2, reads: 1, setup: func(_ *Hub, backlog *recordBacklog) {
			backlog.history = append(backlog.history[:3:3], backlog.history[4:]...)
		}},
		{name: "ring passes the backlog during the read", since: 2, reads: 1, setup: func(hub *Hub, backlog *recordBacklog) {
			backlog.during = func() { publishThrough(hub, 16) }
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hub, backlog := backlogHub()
			tt.setup(hub, backlog)
			if _, err := hub.Subscribe(t.Context(), "/", protocol.Cursor{Epoch: "w", Seq: tt.since}); !errors.Is(err, ErrResync) {
				t.Fatalf("Subscribe: %v, want ErrResync", err)
			}
			if backlog.reads != tt.reads {
				t.Fatalf("backlog reads = %d, want %d", backlog.reads, tt.reads)
			}
		})
	}
}

// publishThrough publishes /a/x.md events after the head through seq, as
// commits that reach the ring while something else is under way.
func publishThrough(hub *Hub, seq uint64) {
	for next := hub.Head().Seq + 1; next <= seq; next++ {
		hub.PublishAt(Event{Seq: next, Path: "/a/x.md", Version: int(next), Op: protocol.OpPublish})
	}
}

// A resume past this replica's head asks the store once for what peers
// committed before calling it a gap; anything else never asks.
func TestResumePastTheHeadCatchesUpOnce(t *testing.T) {
	tests := []struct {
		name       string
		since      protocol.Cursor
		peerHead   uint64 // what a catch-up finds the peer committed through
		catchUpErr error
		resync     bool
		catchUps   int
		next       uint64
	}{
		{name: "peer committed past the head", since: protocol.Cursor{Epoch: "w", Seq: 11}, peerHead: 12, catchUps: 1, next: 12},
		{name: "still past the head after catching up", since: protocol.Cursor{Epoch: "w", Seq: 13}, peerHead: 12, resync: true, catchUps: 1},
		{name: "catching up fails", since: protocol.Cursor{Epoch: "w", Seq: 11}, catchUpErr: errors.New("bucket unavailable"), resync: true, catchUps: 1},
		{name: "at the head", since: protocol.Cursor{Epoch: "w", Seq: 10}},
		{name: "another epoch", since: protocol.Cursor{Epoch: "other", Seq: 20}, resync: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hub, backlog := backlogHub()
			backlog.peer = func() { publishThrough(hub, tt.peerHead) }
			backlog.catchUpErr = tt.catchUpErr
			sub, err := hub.Subscribe(t.Context(), "/", tt.since)
			if tt.resync != errors.Is(err, ErrResync) || !tt.resync && err != nil {
				t.Fatalf("Subscribe: %v, want resync %v", err, tt.resync)
			}
			if backlog.catchUps != tt.catchUps {
				t.Fatalf("catch-ups = %d, want %d", backlog.catchUps, tt.catchUps)
			}
			if tt.next != 0 {
				if got := next(t, sub); got.Seq != tt.next {
					t.Fatalf("first event = %+v, want seq %d", got, tt.next)
				}
			}
		})
	}
}

// Resumes that arrive while a catch-up is in flight share it: a failover
// that moves every watcher at once polls the store once, not per watcher.
func TestConcurrentResumesShareOneCatchUp(t *testing.T) {
	hub, backlog := backlogHub()
	release := make(chan struct{})
	backlog.peer = func() {
		<-release
		publishThrough(hub, 12)
	}
	const resumes = 8
	errs := make(chan error, resumes)
	for range resumes {
		go func() {
			_, err := hub.Subscribe(t.Context(), "/", protocol.Cursor{Epoch: "w", Seq: 11})
			errs <- err
		}()
	}
	time.Sleep(20 * time.Millisecond) // let the resumes pile up behind the first
	close(release)
	for range resumes {
		if err := <-errs; err != nil {
			t.Errorf("Subscribe: %v", err)
		}
	}
	if backlog.catchUps != 1 {
		t.Fatalf("catch-ups = %d, want 1 shared by every resume", backlog.catchUps)
	}
}

// joinedContext closes joined on its first Done call: catchUp's wait on the
// flight is that call, so the waiter has joined the flight by then.
type joinedContext struct {
	context.Context
	joined chan struct{}
	once   sync.Once
}

func (c *joinedContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.joined) })
	return c.Context.Done()
}

// The shared catch-up outlives the resume that started it: that caller
// leaving fails only itself, never the resumes waiting on the same flight.
func TestCatchUpOutlivesTheResumeThatStartedIt(t *testing.T) {
	hub, backlog := backlogHub()
	started, release := make(chan struct{}), make(chan struct{})
	backlog.peer = func() {
		close(started)
		<-release
		publishThrough(hub, 12)
	}
	since := protocol.Cursor{Epoch: "w", Seq: 11}
	leaving, leave := context.WithCancel(t.Context())
	first := make(chan error, 1)
	go func() {
		_, err := hub.Subscribe(leaving, "/", since)
		first <- err
	}()
	<-started
	second := make(chan error, 1)
	waiting := &joinedContext{Context: t.Context(), joined: make(chan struct{})}
	go func() {
		_, err := hub.Subscribe(waiting, "/", since)
		second <- err
	}()
	<-waiting.joined
	leave()
	if err := <-first; !errors.Is(err, context.Canceled) {
		t.Fatalf("leaving resume: %v, want its own cancellation", err)
	}
	close(release)
	if err := <-second; err != nil {
		t.Fatalf("waiting resume failed with the leaver: %v", err)
	}
}
