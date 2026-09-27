package fetch

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/quic-go/quic-go"
)

// feed is a WATCH server for tests: it keeps every event, replays after a
// since cursor, and can end or drop its open streams on demand.
type feed struct {
	mu         sync.Mutex
	events     []protocol.WatchEvent
	notify     chan struct{}
	subscribes []string // the since each subscribe carried
	refuse     string   // status to answer instead of subscribing
	endStatus  string   // terminal status for open streams
	endGen     int
	dropConn   bool
	address    string
}

const feedEpoch = "w"

func newFeed() *feed { return &feed{notify: make(chan struct{})} }

func (f *feed) publish(path string) protocol.Cursor {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := protocol.Cursor{Epoch: feedEpoch, Seq: uint64(len(f.events) + 1)}
	f.events = append(f.events, protocol.WatchEvent{Cursor: c, Path: path, Version: 1, Op: protocol.OpPublish})
	f.wake()
	return c
}

func (f *feed) wake() {
	close(f.notify)
	f.notify = make(chan struct{})
}

func (f *feed) head() protocol.Cursor {
	f.mu.Lock()
	defer f.mu.Unlock()
	return protocol.Cursor{Epoch: feedEpoch, Seq: uint64(len(f.events))}
}

// end sends status to every open stream once.
func (f *feed) end(status string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.endStatus = status
	f.endGen++
	f.wake()
}

// drop closes the connection under the next open stream.
func (f *feed) drop() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dropConn = true
	f.wake()
}

func (f *feed) setRefuse(status string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refuse = status
}

func (f *feed) sinces() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.subscribes...)
}

func (f *feed) serve(conn *quic.Conn, stream *quic.Stream) {
	req, err := protocol.ParseRequest(stream)
	if err != nil {
		return
	}
	if req.Verb != protocol.VerbWatch {
		answerOK(stream)
		return
	}
	f.mu.Lock()
	f.subscribes = append(f.subscribes, req.Metadata["since"])
	if refuse := f.refuse; refuse != "" {
		f.mu.Unlock()
		if refuse == protocol.StatusResync {
			_, _ = protocol.WatchControl(refuse, f.head()).WriteTo(stream)
		} else {
			_, _ = protocol.Response{Status: refuse, Body: "# refused\n"}.WriteTo(stream)
		}
		_ = stream.Close()
		return
	}
	pos := 0
	start := protocol.Cursor{Epoch: feedEpoch, Seq: uint64(len(f.events))}
	if raw := req.Metadata["since"]; raw != "" {
		since, err := protocol.ParseCursor(raw)
		if err != nil || since.Epoch != feedEpoch {
			f.mu.Unlock()
			_, _ = protocol.WatchControl(protocol.StatusResync, start).WriteTo(stream)
			_ = stream.Close()
			return
		}
		pos = int(since.Seq)
		start = since
	}
	gen := f.endGen
	f.mu.Unlock()
	if _, err := protocol.WatchControl(protocol.StatusOK, start).WriteTo(stream); err != nil {
		return
	}
	// delivered is the last position written, which closing and heartbeats
	// carry; only resync carries the head, as the real server does.
	delivered := start
	for {
		f.mu.Lock()
		if f.dropConn {
			f.dropConn = false
			f.mu.Unlock()
			_ = conn.CloseWithError(0, "dropped")
			return
		}
		if f.endGen > gen {
			status := f.endStatus
			f.mu.Unlock()
			cursor := delivered
			if status == protocol.StatusResync {
				cursor = f.head()
			}
			_, _ = protocol.WatchControl(status, cursor).WriteTo(stream)
			_ = stream.Close()
			return
		}
		pending := f.events[pos:]
		pos = len(f.events)
		wait := f.notify
		f.mu.Unlock()
		for _, ev := range pending {
			if _, err := ev.Block().WriteTo(stream); err != nil {
				return
			}
			delivered = ev.Cursor
		}
		select {
		case <-wait:
		case <-time.After(50 * time.Millisecond):
			if _, err := protocol.WatchControl(protocol.StatusOK, delivered).WriteTo(stream); err != nil {
				return
			}
		}
	}
}

func startFeed(t *testing.T) (*feed, *testServer, *Client) {
	t.Helper()
	f := newFeed()
	srv := serveStreams(t, nil, f.serve)
	f.mu.Lock()
	f.address = srv.Addr
	f.mu.Unlock()
	c := NewClient(Options{Insecure: true, RequestTimeout: 2 * time.Second})
	t.Cleanup(c.Close)
	return f, srv, c
}

func nextNotice(t *testing.T, w *Watch) Notice {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	n, err := w.Next(ctx)
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	return n
}

func expectEvent(t *testing.T, w *Watch, path string) protocol.WatchEvent {
	t.Helper()
	n := nextNotice(t, w)
	if n.Resync || n.Event.Path != path {
		t.Fatalf("notice = %+v, want event for %s", n, path)
	}
	return n.Event
}

func TestWatchDeliversEventsAndTracksCursor(t *testing.T) {
	f, _, c := startFeed(t)
	w, err := c.Watch(t.Context(), WatchRequest{Host: f.addr(t), Path: "/inbox/"})
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer w.Close()
	if !w.Cursor().IsZero() && w.Cursor() != f.head() {
		t.Fatalf("cursor after ack = %v", w.Cursor())
	}
	first := f.publish("/inbox/1.md")
	if got := expectEvent(t, w, "/inbox/1.md"); got.Cursor != first {
		t.Fatalf("event cursor = %v, want %v", got.Cursor, first)
	}
	if w.Cursor() != first {
		t.Fatalf("watch cursor = %v, want %v", w.Cursor(), first)
	}
	w.Close()
	if _, err := w.Next(t.Context()); !errors.Is(err, context.Canceled) {
		t.Fatalf("Next after Close: err = %v, want canceled", err)
	}
}

func (f *feed) addr(t *testing.T) string {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.address == "" {
		t.Fatal("feed has no address")
	}
	return f.address
}

func TestWatchReopensAfterClosingFromItsCursor(t *testing.T) {
	f, _, c := startFeed(t)
	w, err := c.Watch(t.Context(), WatchRequest{Host: f.addr(t), Path: "/"})
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer w.Close()
	last := f.publish("/a.md")
	expectEvent(t, w, "/a.md")
	f.end(protocol.StatusClosing)
	f.publish("/b.md")
	expectEvent(t, w, "/b.md")
	sinces := f.sinces()
	if len(sinces) != 2 || sinces[0] != "" || sinces[1] != last.String() {
		t.Fatalf("subscribes = %v, want a resume from %v after closing", sinces, last)
	}
}

func TestWatchSurfacesResyncAndContinues(t *testing.T) {
	f, _, c := startFeed(t)
	w, err := c.Watch(t.Context(), WatchRequest{Host: f.addr(t), Path: "/", Since: protocol.Cursor{Epoch: "old", Seq: 9}})
	if err != nil {
		t.Fatalf("Watch with a stale cursor: %v", err)
	}
	defer w.Close()
	if n := nextNotice(t, w); !n.Resync || n.Cursor != f.head() {
		t.Fatalf("first notice = %+v, want resync at the head", n)
	}
	f.publish("/a.md")
	expectEvent(t, w, "/a.md")
	f.end(protocol.StatusResync)
	if n := nextNotice(t, w); !n.Resync {
		t.Fatalf("notice after a resync block = %+v", n)
	}
	f.publish("/b.md")
	expectEvent(t, w, "/b.md")
	// The stale cursor, then the head the first resync gave (w:0), then the
	// head the terminal resync gave (w:1).
	if got := fmt.Sprint(f.sinces()); got != "[old:9 w:0 w:1]" {
		t.Fatalf("subscribes = %s", got)
	}
}

func TestWatchRefusedAndEndedByStatus(t *testing.T) {
	f, _, c := startFeed(t)
	f.setRefuse(protocol.StatusNotPermitted)
	_, err := c.Watch(t.Context(), WatchRequest{Host: f.addr(t), Path: "/private/"})
	var refused *StatusError
	if !errors.As(err, &refused) || refused.Status != protocol.StatusNotPermitted {
		t.Fatalf("err = %v, want a not-permitted StatusError", err)
	}

	f.setRefuse("")
	w, err := c.Watch(t.Context(), WatchRequest{Host: f.addr(t), Path: "/"})
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer w.Close()
	f.end(protocol.StatusUnauthorized)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_, err = w.Next(ctx)
	if !errors.As(err, &refused) || refused.Status != protocol.StatusUnauthorized {
		t.Fatalf("err after unauthorized = %v", err)
	}
	if !errors.Is(w.Err(), err) {
		t.Fatal("Err() disagrees with Next")
	}
}

func TestWatchReconnectsAfterConnectionLoss(t *testing.T) {
	f, srv, c := startFeed(t)
	w, err := c.Watch(t.Context(), WatchRequest{Host: f.addr(t), Path: "/"})
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer w.Close()
	last := f.publish("/a.md")
	expectEvent(t, w, "/a.md")
	f.drop()
	f.publish("/b.md")
	expectEvent(t, w, "/b.md")
	if got := srv.Conns.Load(); got != 2 {
		t.Fatalf("server accepted %d connections, want a redial after the drop", got)
	}
	if sinces := f.sinces(); sinces[len(sinces)-1] != last.String() {
		t.Fatalf("subscribes = %v, want the last one resumed from %v", sinces, last)
	}
}

// A consumer that stops reading fills the queue; the watch then stops
// reading the stream and reopens later, and the consumer still sees every
// event once, in order.
func TestWatchSlowConsumerLosesNothing(t *testing.T) {
	f, _, c := startFeed(t)
	w, err := c.Watch(t.Context(), WatchRequest{Host: f.addr(t), Path: "/"})
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer w.Close()
	const total = 3 * watchQueueSize
	for i := 1; i <= total; i++ {
		f.publish("/n/" + strconv.Itoa(i) + ".md")
	}
	time.Sleep(300 * time.Millisecond)
	for i := 1; i <= total; i++ {
		got := expectEvent(t, w, "/n/"+strconv.Itoa(i)+".md")
		if got.Cursor.Seq != uint64(i) {
			t.Fatalf("seq = %d, want %d", got.Cursor.Seq, i)
		}
	}
	if len(f.sinces()) < 2 {
		t.Fatalf("subscribes = %v, want the watch to have reopened from its cursor", f.sinces())
	}
}

// waitSubscribes blocks until the feed has seen n subscribes.
func waitSubscribes(t *testing.T, f *feed, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for len(f.sinces()) < n {
		if time.Now().After(deadline) {
			t.Fatalf("subscribes = %v, want %d", f.sinces(), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A retry-later refusal on reopen keeps the watch alive on the backoff.
func TestWatchRetriesARateLimitedReopen(t *testing.T) {
	f, _, c := startFeed(t)
	w, err := c.Watch(t.Context(), WatchRequest{Host: f.addr(t), Path: "/"})
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer w.Close()
	f.setRefuse(protocol.StatusRateLimited)
	f.end(protocol.StatusClosing)
	waitSubscribes(t, f, 3)
	f.setRefuse("")
	f.publish("/a.md")
	expectEvent(t, w, "/a.md")
}

// Close returns while the queue is full and the reopen answers resync.
func TestWatchCloseWithAFullQueueOnResync(t *testing.T) {
	f, _, c := startFeed(t)
	w, err := c.Watch(t.Context(), WatchRequest{Host: f.addr(t), Path: "/"})
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	for i := range watchQueueSize + 1 {
		f.publish("/" + strconv.Itoa(i) + ".md")
	}
	// Let the overflow cancel the stream; the next subscribe is the reopen,
	// which the feed answers with resync while the queue is still full.
	time.Sleep(300 * time.Millisecond)
	f.setRefuse(protocol.StatusResync)
	expectEvent(t, w, "/0.md")
	waitSubscribes(t, f, 2)
	closed := make(chan struct{})
	go func() {
		w.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close hung")
	}
	if !errors.Is(w.Err(), context.Canceled) {
		t.Fatalf("Err() = %v", w.Err())
	}
}
