package fanout

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/server/internal/auth"
	"github.com/latebit-io/demarkus/server/internal/changefeed"
	"github.com/latebit-io/demarkus/server/internal/quicserve"
)

var discardLogger = slog.New(slog.DiscardHandler)

// readTokens is a store where /a/ and /b/ each need their own token and
// everything else is public.
func readTokens() *auth.TokenStore {
	return auth.NewTokenStore(map[string]auth.Token{
		protocol.HashToken("read-a"): {Paths: []string{"/a/**"}, Operations: []string{"read"}},
		protocol.HashToken("read-b"): {Paths: []string{"/b/**"}, Operations: []string{"read"}},
	})
}

func newFanout(t *testing.T, hub *changefeed.Hub, config Config) *Fanout {
	t.Helper()
	config.Hub, config.Logger = hub, discardLogger
	if config.Heartbeat == 0 {
		config.Heartbeat = 20 * time.Millisecond
	}
	f, err := New(config)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return f
}

// stream is one served watch: blocks arrive on C as Serve writes them; err
// is how Serve returned once done is closed.
type stream struct {
	C      <-chan protocol.WatchBlock
	done   chan struct{}
	err    error
	cancel context.CancelFunc
}

// serve runs Serve on its own goroutine into a pipe, and returns after the
// acknowledgement (or the refusal in place of it) is in.
func serve(t *testing.T, f *Fanout, req Request) *stream {
	t.Helper()
	return serveIn(context.Background(), t, f, req)
}

func serveIn(ctx context.Context, t *testing.T, f *Fanout, req Request) *stream {
	t.Helper()
	ctx, cancel := context.WithCancel(ctx)
	pr, pw := io.Pipe()
	s := &stream{done: make(chan struct{}), cancel: cancel}
	go func() {
		s.err = f.Serve(ctx, req, pw)
		_ = pw.Close()
		close(s.done)
	}()
	s.C = readBlocks(pr)
	t.Cleanup(s.stop)
	return s
}

// readBlocks pumps every block on r into a channel closed at EOF.
func readBlocks(r io.Reader) <-chan protocol.WatchBlock {
	blocks := make(chan protocol.WatchBlock, 4096)
	go func() {
		reader := protocol.NewWatchReader(r)
		for {
			block, err := reader.Next()
			if err != nil {
				close(blocks)
				return
			}
			blocks <- block
		}
	}()
	return blocks
}

func (s *stream) stop() {
	s.cancel()
	s.wait()
}

func (s *stream) wait() {
	select {
	case <-s.done:
	case <-time.After(2 * time.Second):
	}
}

func (s *stream) next(t *testing.T) protocol.WatchBlock {
	t.Helper()
	select {
	case block, ok := <-s.C:
		if !ok {
			t.Fatal("stream closed")
		}
		return block
	case <-time.After(2 * time.Second):
		t.Fatal("no block within 2s")
	}
	return protocol.WatchBlock{}
}

// ack reads the acknowledgement and returns its cursor.
func (s *stream) ack(t *testing.T) protocol.Cursor {
	t.Helper()
	block := s.next(t)
	if block.Status != protocol.StatusOK {
		t.Fatalf("ack status = %q", block.Status)
	}
	c, err := block.Cursor()
	if err != nil {
		t.Fatalf("ack cursor: %v", err)
	}
	return c
}

// event skips heartbeats and fails on any other control block.
func (s *stream) event(t *testing.T) protocol.WatchEvent {
	t.Helper()
	for {
		block := s.next(t)
		if block.Status == protocol.StatusOK {
			continue
		}
		ev, err := block.Event()
		if err != nil {
			t.Fatalf("event: %v (block %+v)", err, block)
		}
		return ev
	}
}

// terminal reads until a block with a status other than ok and returns it.
func (s *stream) terminal(t *testing.T) protocol.WatchBlock {
	t.Helper()
	for {
		block := s.next(t)
		if block.Status != "" && block.Status != protocol.StatusOK {
			return block
		}
	}
}

func publish(hub *changefeed.Hub, path string) changefeed.Event {
	ev := changefeed.Event{Seq: hub.Head().Seq + 1, Path: path, Version: 1, Hash: "sha256-" + strings.Repeat("0", 64), Op: protocol.OpPublish}
	hub.PublishAt(ev)
	return ev
}

// waitAppended returns once the reader has put seq in the ring.
func waitAppended(t *testing.T, f *Fanout, seq uint64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		appended := f.run != nil && f.run.next > seq
		f.mu.Unlock()
		if appended {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("seq %d not appended within 2s", seq)
}

// waitWatches returns once n watches are attached.
func waitWatches(t *testing.T, f *Fanout, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for f.Watches() != n {
		if time.Now().After(deadline) {
			t.Fatalf("watches = %d, want %d after 2s", f.Watches(), n)
		}
		time.Sleep(time.Millisecond)
	}
}

func groupCount(f *Fanout) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.groups)
}

func running(f *Fanout) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.run != nil
}

// Watchers sharing a scope and a token are one group: one permission check
// per event, the same bytes to each.
func TestWatchersShareAGroupAndTheBytes(t *testing.T) {
	hub := changefeed.New("w", 0)
	f := newFanout(t, hub, Config{Tokens: readTokens})
	first := serve(t, f, Request{Scope: "/a/", Token: "read-a"})
	second := serve(t, f, Request{Scope: "/a/", Token: "read-a"})
	other := serve(t, f, Request{Scope: "/", Token: "read-b"})
	for _, s := range []*stream{first, second, other} {
		s.ack(t)
	}
	if got := groupCount(f); got != 2 {
		t.Fatalf("groups = %d, want 2", got)
	}
	if got := f.Watches(); got != 3 {
		t.Fatalf("watches = %d, want 3", got)
	}
	want := publish(hub, "/a/doc.md")
	for _, s := range []*stream{first, second} {
		if ev := s.event(t); ev.Path != want.Path || ev.Cursor.Seq != want.Seq {
			t.Fatalf("event = %+v, want %+v", ev, want)
		}
	}
	publish(hub, "/public.md")
	if ev := other.event(t); ev.Path != "/public.md" {
		t.Fatalf("the other group got %+v before the public event", ev)
	}
}

// A group never sees an event its token does not read, whatever it watches.
func TestGroupsFenceEventsByToken(t *testing.T) {
	hub := changefeed.New("w", 0)
	f := newFanout(t, hub, Config{Tokens: readTokens})
	a := serve(t, f, Request{Scope: "/", Token: "read-a"})
	b := serve(t, f, Request{Scope: "/", Token: "read-b"})
	anon := serve(t, f, Request{Scope: "/"})
	for _, s := range []*stream{a, b, anon} {
		s.ack(t)
	}
	publish(hub, "/a/one.md")
	publish(hub, "/b/two.md")
	publish(hub, "/three.md")
	if got := []string{a.event(t).Path, a.event(t).Path}; got[0] != "/a/one.md" || got[1] != "/three.md" {
		t.Fatalf("read-a saw %v", got)
	}
	if got := []string{b.event(t).Path, b.event(t).Path}; got[0] != "/b/two.md" || got[1] != "/three.md" {
		t.Fatalf("read-b saw %v", got)
	}
	if got := anon.event(t).Path; got != "/three.md" {
		t.Fatalf("anonymous saw %s first", got)
	}
}

// An exact-document scope matches only that path; a prefix its subtree.
func TestScopesMatchPrefixesAndDocuments(t *testing.T) {
	hub := changefeed.New("w", 0)
	f := newFanout(t, hub, Config{})
	doc := serve(t, f, Request{Scope: "/x/doc.md"})
	tree := serve(t, f, Request{Scope: "/x/"})
	doc.ack(t)
	tree.ack(t)
	publish(hub, "/x/other.md")
	publish(hub, "/x/sub/deep.md")
	publish(hub, "/x/doc.md")
	if got := doc.event(t).Path; got != "/x/doc.md" {
		t.Fatalf("document scope saw %s", got)
	}
	for _, want := range []string{"/x/other.md", "/x/sub/deep.md", "/x/doc.md"} {
		if got := tree.event(t).Path; got != want {
			t.Fatalf("prefix scope saw %s, want %s", got, want)
		}
	}
}

// A watcher that stops reading is lapped and told to resync while the fast
// one beside it receives everything; publishing never waited.
func TestSlowWatcherResyncsAlone(t *testing.T) {
	hub := changefeed.New("w", 8)
	f := newFanout(t, hub, Config{})
	fast := serve(t, f, Request{Scope: "/"})
	fast.ack(t)
	// Nobody reads the pipe yet: the ack write blocks the slow watcher.
	pr, pw := io.Pipe()
	slowDone := make(chan struct{})
	go func() {
		_ = f.Serve(t.Context(), Request{Scope: "/"}, pw)
		_ = pw.Close()
		close(slowDone)
	}()
	waitWatches(t, f, 2)
	for i := range 20 {
		start := time.Now()
		publish(hub, fmt.Sprintf("/doc-%d.md", i))
		if took := time.Since(start); took > 100*time.Millisecond {
			t.Fatalf("publish %d took %v under a blocked watcher", i, took)
		}
		if got := fast.event(t).Path; got != fmt.Sprintf("/doc-%d.md", i) {
			t.Fatalf("fast watcher saw %s at %d", got, i)
		}
	}
	// Unblock the slow writer: its first gather finds the ring past it.
	var last protocol.WatchBlock
	for block := range readBlocks(pr) {
		last = block
	}
	<-slowDone
	if c, _ := last.Cursor(); last.Status != protocol.StatusResync || c != hub.Head() {
		t.Fatalf("slow watcher ended %+v, want resync at %v", last, hub.Head())
	}
}

// With coalesce, a lagging watcher receives the newest version per path,
// in order, and nothing else.
func TestCoalesceKeepsTheNewestPerPath(t *testing.T) {
	hub := changefeed.New("w", 0)
	f := newFanout(t, hub, Config{})
	// The ack write blocks until the test reads, so every event below is
	// pending at the watcher's first gather.
	pr, pw := io.Pipe()
	go func() {
		_ = f.Serve(t.Context(), Request{Scope: "/", Coalesce: true}, pw)
		_ = pw.Close()
	}()
	waitWatches(t, f, 1)
	for i := range 10 {
		hub.PublishAt(changefeed.Event{Seq: hub.Head().Seq + 1, Path: "/a.md", Version: i + 1, Op: protocol.OpPublish})
	}
	hub.PublishAt(changefeed.Event{Seq: hub.Head().Seq + 1, Path: "/b.md", Version: 1, Op: protocol.OpPublish})
	hub.PublishAt(changefeed.Event{Seq: hub.Head().Seq + 1, Path: "/a.md", Version: 11, Op: protocol.OpPublish})
	waitAppended(t, f, hub.Head().Seq)
	s := &stream{C: readBlocks(pr)}
	s.ack(t)
	first, second := s.event(t), s.event(t)
	if got := fmt.Sprintf("%s@%d %s@%d", first.Path, first.Version, second.Path, second.Version); got != "/b.md@1 /a.md@11" {
		t.Fatalf("coalesced events = %s, want /b.md@1 /a.md@11", got)
	}
}

// A resume older than the fan-out's ring replays through the hub, checked
// for this watcher alone, then continues from the ring.
func TestResumeCatchesUpThroughTheHub(t *testing.T) {
	hub := changefeed.New("w", 0)
	f := newFanout(t, hub, Config{Tokens: readTokens})
	var since protocol.Cursor
	for i := range 5 {
		ev := publish(hub, fmt.Sprintf("/a/%d.md", i))
		if i == 1 {
			since = protocol.Cursor{Epoch: "w", Seq: ev.Seq}
		}
	}
	publish(hub, "/b/hidden.md")
	s := serve(t, f, Request{Scope: "/", Token: "read-a", Since: since})
	if got := s.ack(t); got != since {
		t.Fatalf("ack cursor = %v, want since %v", got, since)
	}
	for _, want := range []string{"/a/2.md", "/a/3.md", "/a/4.md"} {
		if got := s.event(t).Path; got != want {
			t.Fatalf("replayed %s, want %s", got, want)
		}
	}
	publish(hub, "/a/live.md")
	if got := s.event(t).Path; got != "/a/live.md" {
		t.Fatalf("live event after catch-up = %s", got)
	}
}

// A resume into a group that did not exist while the ring filled goes
// through the hub: ring entries never named the new group.
func TestResumeIntoANewGroupCatchesUp(t *testing.T) {
	hub := changefeed.New("w", 0)
	f := newFanout(t, hub, Config{})
	keeper := serve(t, f, Request{Scope: "/"})
	keeper.ack(t)
	gone := serve(t, f, Request{Scope: "/x/"})
	since := gone.ack(t)
	gone.stop()
	publish(hub, "/x/while-away.md")
	keeper.event(t)
	back := serve(t, f, Request{Scope: "/x/", Since: since})
	back.ack(t)
	if got := back.event(t).Path; got != "/x/while-away.md" {
		t.Fatalf("resumed watcher saw %s", got)
	}
	publish(hub, "/x/live.md")
	if got := back.event(t).Path; got != "/x/live.md" {
		t.Fatalf("live event after the resume = %s", got)
	}
}

// Coalescing a resume: 100 edits to one path and one to another are two
// events, newest versions, in order.
func TestCoalescedResumeReplaysTwoEvents(t *testing.T) {
	hub := changefeed.New("w", 0)
	f := newFanout(t, hub, Config{})
	start := hub.Head()
	for i := range 100 {
		hub.PublishAt(changefeed.Event{Seq: hub.Head().Seq + 1, Path: "/a.md", Version: i + 1, Op: protocol.OpPublish})
	}
	hub.PublishAt(changefeed.Event{Seq: hub.Head().Seq + 1, Path: "/b.md", Version: 1, Op: protocol.OpPublish})
	s := serve(t, f, Request{Scope: "/", Since: start, Coalesce: true})
	s.ack(t)
	first, second := s.event(t), s.event(t)
	if first.Path != "/a.md" || first.Version != 100 || second.Path != "/b.md" {
		t.Fatalf("coalesced resume = %+v, %+v", first, second)
	}
	plain := serve(t, f, Request{Scope: "/", Since: start})
	plain.ack(t)
	for range 101 {
		plain.event(t)
	}
}

// Another epoch, or a cursor past what the hub keeps, is answered with a
// resync block in place of the acknowledgement, and holds nothing.
func TestUnresumableCursorGetsResyncFirst(t *testing.T) {
	hub := changefeed.New("w", 4)
	f := newFanout(t, hub, Config{})
	for range 10 {
		publish(hub, "/x.md")
	}
	for _, since := range []protocol.Cursor{{Epoch: "other", Seq: 1}, {Epoch: "w", Seq: 1}} {
		var out bytes.Buffer
		if err := f.Serve(t.Context(), Request{Scope: "/", Since: since}, &out); err != nil {
			t.Fatalf("since %v: %v", since, err)
		}
		first := <-readBlocks(&out)
		if c, _ := first.Cursor(); first.Status != protocol.StatusResync || c != hub.Head() {
			t.Fatalf("since %v: first block %+v, want resync at %v", since, first, hub.Head())
		}
	}
	if f.Watches() != 0 || running(f) {
		t.Fatalf("a refused watch left state: %d watches, run %v", f.Watches(), running(f))
	}
}

// One sweep serves every idle stream: heartbeats carry the resume cursor.
func TestIdleStreamsHeartbeat(t *testing.T) {
	hub := changefeed.New("w", 0)
	f := newFanout(t, hub, Config{Heartbeat: 10 * time.Millisecond})
	streams := []*stream{serve(t, f, Request{Scope: "/"}), serve(t, f, Request{Scope: "/deep/"})}
	publish(hub, "/deep/x.md")
	head := hub.Head()
	for _, s := range streams {
		for {
			block := s.next(t)
			if block.Status != protocol.StatusOK {
				continue
			}
			if c, _ := block.Cursor(); c == head {
				break
			}
		}
	}
}

// A token reload the sweep sees ends the group's watchers with the verdict.
func TestSweepEndsRevokedGroups(t *testing.T) {
	hub := changefeed.New("w", 0)
	var mu sync.Mutex
	current := readTokens()
	f := newFanout(t, hub, Config{Tokens: func() *auth.TokenStore { mu.Lock(); defer mu.Unlock(); return current }})
	revoked := serve(t, f, Request{Scope: "/a/", Token: "read-a"})
	kept := serve(t, f, Request{Scope: "/b/", Token: "read-b"})
	revoked.ack(t)
	kept.ack(t)
	mu.Lock()
	current = auth.NewTokenStore(map[string]auth.Token{
		protocol.HashToken("read-b"): {Paths: []string{"/a/**", "/b/**"}, Operations: []string{"read"}},
	})
	mu.Unlock()
	if last := revoked.terminal(t); last.Status != protocol.StatusUnauthorized {
		t.Fatalf("revoked watcher ended %+v, want unauthorized", last)
	}
	publish(hub, "/b/still.md")
	if got := kept.event(t).Path; got != "/b/still.md" {
		t.Fatalf("kept watcher saw %s", got)
	}
}

// A credential that lapses ends its own watch with unauthorized at its
// cursor; the group's other watchers keep going.
func TestLapsedCredentialEndsItsWatchAlone(t *testing.T) {
	hub := changefeed.New("w", 0)
	f := newFanout(t, hub, Config{})
	ctx, cancel := context.WithDeadlineCause(context.Background(), time.Now().Add(150*time.Millisecond), auth.ErrTokenExpired)
	defer cancel()
	lapsing := serveIn(ctx, t, f, Request{Scope: "/"})
	kept := serve(t, f, Request{Scope: "/"})
	lapsing.ack(t)
	kept.ack(t)
	publish(hub, "/x.md")
	lapsing.event(t)
	kept.event(t)
	last := lapsing.terminal(t)
	if c, _ := last.Cursor(); last.Status != protocol.StatusUnauthorized || c != hub.Head() {
		t.Fatalf("end = %+v, want unauthorized at %v", last, hub.Head())
	}
	publish(hub, "/y.md")
	if got := kept.event(t).Path; got != "/y.md" {
		t.Fatalf("kept watcher saw %s", got)
	}
}

// A closed hub ends every watcher with closing at its own cursor.
func TestHubCloseEndsWatchersWithClosing(t *testing.T) {
	hub := changefeed.New("w", 0)
	f := newFanout(t, hub, Config{})
	s := serve(t, f, Request{Scope: "/"})
	s.ack(t)
	publish(hub, "/x.md")
	s.event(t)
	hub.Close()
	last := s.terminal(t)
	if c, _ := last.Cursor(); last.Status != protocol.StatusClosing || c != hub.Head() {
		t.Fatalf("end = %+v, want closing at %v", last, hub.Head())
	}
	s.wait()
	if s.err != nil {
		t.Fatalf("Serve: %v", s.err)
	}
}

// The world cap and the connection cap refuse with the limit named, and a
// refused watch holds nothing.
func TestCapsRefuseWithTheLimit(t *testing.T) {
	hub := changefeed.New("w", 0)
	f := newFanout(t, hub, Config{MaxWatches: 2, MaxWatchesPerConn: 1})
	conn := quicserve.WithConnState(context.Background())
	serveIn(conn, t, f, Request{Scope: "/"}).ack(t)
	var limited *LimitError
	if err := f.Serve(conn, Request{Scope: "/"}, io.Discard); !errors.As(err, &limited) || limited.Limit != "connection" {
		t.Fatalf("second on the connection: %v", err)
	}
	serveIn(quicserve.WithConnState(context.Background()), t, f, Request{Scope: "/"}).ack(t)
	if err := f.Serve(quicserve.WithConnState(context.Background()), Request{Scope: "/"}, io.Discard); !errors.As(err, &limited) || limited.Limit != "world" {
		t.Fatalf("over the world cap: %v", err)
	}
	if state := quicserve.ConnStateFromContext(conn); state.Watches.Load() != 1 {
		t.Fatalf("connection count = %d after a refusal", state.Watches.Load())
	}
}

// Serve refuses a token that does not read the scope now.
func TestServeChecksTheScope(t *testing.T) {
	hub := changefeed.New("w", 0)
	f := newFanout(t, hub, Config{Tokens: readTokens})
	if err := f.Serve(t.Context(), Request{Scope: "/a/"}, io.Discard); !errors.Is(err, auth.ErrNoToken) {
		t.Fatalf("no token: %v", err)
	}
	if err := f.Serve(t.Context(), Request{Scope: "/a/", Token: "read-b"}, io.Discard); !errors.Is(err, auth.ErrNotPermitted) {
		t.Fatalf("wrong token: %v", err)
	}
}

// The last watcher out stops the reader and the sweep and frees the ring;
// the next one in starts them again at the head.
func TestIdleFanoutHoldsNothing(t *testing.T) {
	hub := changefeed.New("w", 0)
	f := newFanout(t, hub, Config{})
	s := serve(t, f, Request{Scope: "/"})
	s.ack(t)
	publish(hub, "/x.md")
	s.event(t)
	s.stop()
	if running(f) || f.Watches() != 0 || groupCount(f) != 0 {
		t.Fatalf("idle fanout kept state: run %v, %d watches, %d groups", running(f), f.Watches(), groupCount(f))
	}
	publish(hub, "/missed.md")
	again := serve(t, f, Request{Scope: "/"})
	if got := again.ack(t); got != hub.Head() {
		t.Fatalf("fresh watch acks %v, want head %v", got, hub.Head())
	}
	publish(hub, "/y.md")
	if got := again.event(t).Path; got != "/y.md" {
		t.Fatalf("fresh watch saw %s", got)
	}
}

// peerBacklog is a store shared with a peer replica: CatchUp publishes the
// peer's commits this hub has not seen.
type peerBacklog struct {
	hub     *changefeed.Hub
	pending []changefeed.Event
}

func (b *peerBacklog) Events(context.Context, uint64, uint64) ([]changefeed.Event, error) {
	return nil, errors.New("no sealed changes")
}

func (b *peerBacklog) CatchUp(context.Context) error {
	for _, ev := range b.pending {
		b.hub.PublishAt(ev)
	}
	b.pending = nil
	return nil
}

// A watch that moved here from a replica further ahead resumes once the hub
// catches up, from after its cursor: nothing it saw there is replayed.
func TestResumeAheadOfTheRingCatchesUpWithoutReplay(t *testing.T) {
	backlog := &peerBacklog{}
	hub := changefeed.NewWithBacklog("w", 0, backlog)
	backlog.hub = hub
	f := newFanout(t, hub, Config{})
	attached := serve(t, f, Request{Scope: "/"})
	attached.ack(t)
	publish(hub, "/local.md")
	waitAppended(t, f, 1)
	peer := func(seq uint64, path string) changefeed.Event {
		return changefeed.Event{Seq: seq, Path: path, Version: 1, Hash: "sha256-" + strings.Repeat("0", 64), Op: protocol.OpPublish}
	}
	backlog.pending = []changefeed.Event{peer(2, "/peer-2.md"), peer(3, "/peer-3.md")}

	since := protocol.Cursor{Epoch: "w", Seq: 2}
	moved := serve(t, f, Request{Scope: "/", Since: since})
	if got := moved.ack(t); got != since {
		t.Fatalf("ack = %v, want since %v", got, since)
	}
	if got := moved.event(t).Path; got != "/peer-3.md" {
		t.Fatalf("first event = %s, want /peer-3.md with nothing replayed", got)
	}
}
