package storetest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/storefmt"
	"github.com/latebit-io/demarkus/server/internal/backend"
	"github.com/latebit-io/demarkus/server/internal/changefeed"
	"github.com/latebit-io/demarkus/server/internal/handler"
)

// ChangeBackend is one open store with WATCH on. Close releases what Open
// took; the site's durable state survives it.
type ChangeBackend struct {
	Store handler.DocumentStore
	Close func() error
}

// hub is the store's hub through backend.ChangeSource, the contract under test.
func (b ChangeBackend) hub(t *testing.T) *changefeed.Hub {
	t.Helper()
	source, ok := b.Store.(backend.ChangeSource)
	if !ok {
		t.Fatalf("store %T does not implement backend.ChangeSource", b.Store)
	}
	hub := source.Changes()
	if hub == nil {
		t.Fatal("store has WATCH off")
	}
	return hub
}

func (b ChangeBackend) close(t *testing.T) {
	t.Helper()
	if b.Close != nil {
		if err := b.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
	}
}

// ChangeSite is one durable location a WATCH-capable store opens over, so
// the suite can reopen it the way a restart does and change it behind the
// store's back the way an out-of-band writer or a peer replica does.
type ChangeSite interface {
	// Open opens the store over the site; a second Open after Close is a
	// restart. Window applies across it.
	Open(t *testing.T) ChangeBackend
	// Tamper commits one publish of path while no backend from Open is open,
	// through whatever bypasses the store's own sequence at this site.
	Tamper(t *testing.T, path string)
	// Window is how many commits a cursor may lag and still resume after
	// Open; one more must resync.
	Window() int
}

// RunChangeConformance proves backend.ChangeSource for the store a site
// opens: sequences follow commits, survive a restart, resync past the window,
// and never miss a change made behind the store silently.
func RunChangeConformance(t *testing.T, newSite func(t *testing.T) ChangeSite) {
	subtests := []struct {
		name string
		fn   func(t *testing.T, site ChangeSite)
	}{
		{"EventsFollowCommits", testEventsFollowCommits},
		{"ContiguousUnderConcurrentWriters", testContiguousUnderConcurrentWriters},
		{"CursorSurvivesReopen", testCursorSurvivesReopen},
		{"IdleReopenKeepsEpoch", testIdleReopenKeepsEpoch},
		{"ResumeInsideWindowAfterReopen", testResumeInsideWindowAfterReopen},
		{"ResumeBeyondWindowResyncs", testResumeBeyondWindowResyncs},
		{"ChangeBehindClosedStoreIsNeverSilent", testChangeBehindClosedStoreIsNeverSilent},
		{"OtherEpochResyncs", testOtherEpochResyncs},
	}
	for _, st := range subtests {
		t.Run(st.name, func(t *testing.T) { st.fn(t, newSite(t)) })
	}
}

// changeSession is one open backend with a subscription over everything.
type changeSession struct {
	ChangeBackend
	hub    *changefeed.Hub
	direct Direct
	sub    *changefeed.Subscription
}

// subscribe opens a session over b from since; the error is Subscribe's.
func subscribe(t *testing.T, b ChangeBackend, since protocol.Cursor) (changeSession, error) {
	t.Helper()
	hub := b.hub(t)
	sub, err := hub.Subscribe(t.Context(), "/", since)
	if err != nil {
		return changeSession{}, err
	}
	return changeSession{ChangeBackend: b, hub: hub, direct: Direct{Store: b.Store}, sub: sub}, nil
}

func openChanges(t *testing.T, site ChangeSite, since protocol.Cursor) changeSession {
	t.Helper()
	s, err := subscribe(t, site.Open(t), since)
	if err != nil {
		t.Fatalf("Subscribe(%v): %v", since, err)
	}
	return s
}

func (s changeSession) publish(t *testing.T, path, body string, meta map[string]string) *storefmt.Document {
	t.Helper()
	doc, err := s.direct.WriteVersion(path, 0, []byte(body), meta)
	if err != nil {
		t.Fatalf("publish %s: %v", path, err)
	}
	return doc
}

// NextEvent reads one event or fails after a generous wait.
func NextEvent(t *testing.T, sub *changefeed.Subscription) changefeed.Event {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ev, err := sub.Next(ctx)
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	return ev
}

// Quiet fails if an event arrives within a short wait.
func Quiet(t *testing.T, sub *changefeed.Subscription) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if ev, err := sub.Next(ctx); err == nil {
		t.Fatalf("unexpected event %+v", ev)
	}
}

func (s changeSession) next(t *testing.T) changefeed.Event { return NextEvent(t, s.sub) }

// nextIn drains n events and returns them in order.
func (s changeSession) nextIn(t *testing.T, n int) []changefeed.Event {
	t.Helper()
	events := make([]changefeed.Event, 0, n)
	for range n {
		events = append(events, s.next(t))
	}
	return events
}

func (s changeSession) quiet(t *testing.T) { Quiet(t, s.sub) }

// publishN commits n versions of distinct paths under dir, draining each
// event as it lands so the ring is never lapped, and returns the cursor of
// the last one.
func (s changeSession) publishN(t *testing.T, dir string, n int) protocol.Cursor {
	t.Helper()
	var last changefeed.Event
	for i := range n {
		s.publish(t, fmt.Sprintf("%s/%03d.md", dir, i), "# n\n", nil)
		last = s.next(t)
	}
	return at(s.hub, last.Seq)
}

// at is the cursor of seq in hub's epoch.
func at(hub *changefeed.Hub, seq uint64) protocol.Cursor {
	return protocol.Cursor{Epoch: hub.Epoch(), Seq: seq}
}

func assertContiguous(t *testing.T, events []changefeed.Event) {
	t.Helper()
	for i := 1; i < len(events); i++ {
		if events[i].Seq != events[i-1].Seq+1 {
			t.Fatalf("sequence gap: %d then %d", events[i-1].Seq, events[i].Seq)
		}
	}
}

func testEventsFollowCommits(t *testing.T, site ChangeSite) {
	s := openChanges(t, site, protocol.Cursor{})
	defer s.close(t)
	first := s.publish(t, "/w/a.md", "# A\n", map[string]string{"agent": "suite", "user": "alice@example.com"})
	appended, err := s.direct.AppendVersion("/w/a.md", first.Version, []byte("more\n"), nil)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if _, err := s.direct.Archive("/w/a.md", true); err != nil {
		t.Fatalf("archive: %v", err)
	}
	if res, err := s.direct.Archive("/w/a.md", true); err != nil || res.Changed {
		t.Fatalf("repeated archive: changed=%v err=%v; a no-op must emit nothing", res.Changed, err)
	}
	if _, err := s.direct.WriteVersion("/w/a.md", 42, []byte("# stale\n"), nil); err == nil {
		t.Fatal("conflicting publish succeeded; a refused write must emit nothing")
	}
	s.publish(t, "/w/b.md", "# B\n", nil)

	events := s.nextIn(t, 4)
	assertContiguous(t, events)
	want := []struct {
		path, op, hash string
		version        int
		agent, user    string
	}{
		{"/w/a.md", protocol.OpPublish, storefmt.ContentHash(first.Content), 1, "suite", "alice@example.com"},
		{"/w/a.md", protocol.OpAppend, storefmt.ContentHash(appended.Content), 2, "suite", "alice@example.com"},
		{"/w/a.md", protocol.OpArchive, storefmt.ContentHash(appended.Content), 2, "suite", "alice@example.com"},
		{"/w/b.md", protocol.OpPublish, storefmt.ContentHash([]byte("# B\n")), 1, "", ""},
	}
	for i, w := range want {
		got := events[i]
		if got.Path != w.path || got.Op != w.op || got.Hash != w.hash || got.Version != w.version || got.Agent != w.agent || got.User != w.user {
			t.Fatalf("event %d = %+v, want %+v", i, got, w)
		}
	}
	if head := s.hub.Head(); head.Seq != events[3].Seq || head.Epoch != s.hub.Epoch() {
		t.Fatalf("head = %v after the last event %d", head, events[3].Seq)
	}
	s.quiet(t)
}

func testContiguousUnderConcurrentWriters(t *testing.T, site ChangeSite) {
	s := openChanges(t, site, protocol.Cursor{})
	defer s.close(t)
	// 48 events in flight: below every site's in-process ring, so they are
	// drained after the writers finish. Publishing never waits on a reader
	// (proven in changefeed); this case proves the store numbers in order.
	const writers, each = 8, 6
	var wg sync.WaitGroup
	for w := range writers {
		wg.Go(func() {
			for i := range each {
				if _, err := s.direct.WriteVersion(fmt.Sprintf("/c/%d/%d.md", w, i), 0, []byte("# c\n"), nil); err != nil {
					t.Errorf("write: %v", err)
				}
			}
		})
	}
	wg.Wait()
	events := s.nextIn(t, writers*each)
	assertContiguous(t, events)
	seen := map[string]bool{}
	for _, ev := range events {
		if seen[ev.Path] {
			t.Fatalf("path %s reported twice", ev.Path)
		}
		seen[ev.Path] = true
	}
	s.quiet(t)
}

func testCursorSurvivesReopen(t *testing.T, site ChangeSite) {
	s := openChanges(t, site, protocol.Cursor{})
	s.publish(t, "/r/1.md", "# 1\n", nil)
	s.publish(t, "/r/2.md", "# 2\n", nil)
	s.publish(t, "/r/3.md", "# 3\n", nil)
	events := s.nextIn(t, 3)
	epoch, head := s.hub.Epoch(), s.hub.Head()
	s.close(t)

	resumed := openChanges(t, site, at(s.hub, events[0].Seq))
	defer resumed.close(t)
	if resumed.hub.Epoch() != epoch {
		t.Fatalf("epoch after reopen = %q, want %q", resumed.hub.Epoch(), epoch)
	}
	if resumed.hub.Head() != head {
		t.Fatalf("head after reopen = %v, want %v", resumed.hub.Head(), head)
	}
	replayed := resumed.nextIn(t, 2)
	if replayed[0] != events[1] || replayed[1] != events[2] {
		t.Fatalf("replayed %+v, want %+v", replayed, events[1:])
	}
	resumed.quiet(t)
	// New commits continue the same sequence.
	resumed.publish(t, "/r/4.md", "# 4\n", nil)
	if ev := resumed.next(t); ev.Seq != events[2].Seq+1 || ev.Path != "/r/4.md" {
		t.Fatalf("first event after reopen = %+v, want seq %d", ev, events[2].Seq+1)
	}
}

func testIdleReopenKeepsEpoch(t *testing.T, site ChangeSite) {
	s := openChanges(t, site, protocol.Cursor{})
	s.publish(t, "/i/1.md", "# 1\n", nil)
	s.next(t)
	epoch, head := s.hub.Epoch(), s.hub.Head()
	s.close(t)
	for range 2 {
		again := openChanges(t, site, head)
		if again.hub.Epoch() != epoch || again.hub.Head() != head {
			t.Fatalf("idle reopen: epoch %q head %v, want %q %v", again.hub.Epoch(), again.hub.Head(), epoch, head)
		}
		again.quiet(t)
		again.close(t)
	}
}

func testResumeInsideWindowAfterReopen(t *testing.T, site ChangeSite) {
	s := openChanges(t, site, protocol.Cursor{})
	s.publish(t, "/in/0.md", "# 0\n", nil)
	since := at(s.hub, s.next(t).Seq)
	last := s.publishN(t, "/in", site.Window())
	s.close(t)

	resumed := openChanges(t, site, since)
	defer resumed.close(t)
	events := resumed.nextIn(t, site.Window())
	assertContiguous(t, events)
	if events[0].Seq != since.Seq+1 || events[len(events)-1].Seq != last.Seq {
		t.Fatalf("window replay %d..%d, want %d..%d", events[0].Seq, events[len(events)-1].Seq, since.Seq+1, last.Seq)
	}
	resumed.quiet(t)
}

func testResumeBeyondWindowResyncs(t *testing.T, site ChangeSite) {
	s := openChanges(t, site, protocol.Cursor{})
	s.publish(t, "/out/0.md", "# 0\n", nil)
	since := at(s.hub, s.next(t).Seq)
	last := s.publishN(t, "/out", site.Window()+1)
	s.close(t)

	b := site.Open(t)
	defer b.close(t)
	hub := b.hub(t)
	if _, err := hub.Subscribe(t.Context(), "/", since); !errors.Is(err, changefeed.ErrResync) {
		t.Fatalf("Subscribe(%v) after %d more commits: err = %v, want ErrResync", since, site.Window()+1, err)
	}
	if head := hub.Head(); head != last {
		t.Fatalf("head after reopen = %v, want %v", head, last)
	}
	if _, err := hub.Subscribe(t.Context(), "/", last); err != nil {
		t.Fatalf("Subscribe at the head: %v", err)
	}
}

func testChangeBehindClosedStoreIsNeverSilent(t *testing.T, site ChangeSite) {
	s := openChanges(t, site, protocol.Cursor{})
	s.publish(t, "/t/a.md", "# a\n", nil)
	since := at(s.hub, s.next(t).Seq)
	s.close(t)
	site.Tamper(t, "/t/b.md")

	b := site.Open(t)
	defer b.close(t)
	resumed, err := subscribe(t, b, since)
	if errors.Is(err, changefeed.ErrResync) {
		return // the store could not name the change and said so
	}
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if ev := resumed.next(t); ev.Path != "/t/b.md" {
		t.Fatalf("event after tamper = %+v, want /t/b.md; a change behind the closed store must be replayed or a resync", ev)
	}
}

func testOtherEpochResyncs(t *testing.T, site ChangeSite) {
	b := site.Open(t)
	defer b.close(t)
	if _, err := b.hub(t).Subscribe(t.Context(), "/", protocol.Cursor{Epoch: "other", Seq: 1}); !errors.Is(err, changefeed.ErrResync) {
		t.Fatalf("err = %v, want ErrResync", err)
	}
}
