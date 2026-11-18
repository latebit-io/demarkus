package mcpwatch

import (
	"context"
	"errors"
	"maps"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/protocol"
	"github.com/mark3labs/mcp-go/mcp"
)

const testURI = "mark://team/docs/"

func update(meta map[string]any) mcp.JSONRPCNotification {
	full := map[string]any{MetaSubscription: testURI}
	maps.Copy(full, meta)
	return mcp.JSONRPCNotification{
		Method: string(mcp.MethodNotificationResourceUpdated),
		Params: mcp.NotificationParams{Meta: full, AdditionalFields: map[string]any{"uri": "mark://team/docs/a.md"}}}
}

func event(seq int) mcp.JSONRPCNotification {
	return update(map[string]any{MetaCursor: "e:" + strconv.Itoa(seq), MetaPath: "/docs/a.md", MetaVersion: float64(seq), MetaOp: protocol.OpPublish})
}

func newTestWatch(t *testing.T, queue int) (*Watch, *stream) {
	t.Helper()
	feed, err := fetch.NewFeed(protocol.Cursor{}, queue)
	if err != nil {
		t.Fatal(err)
	}
	feed.Start(func() {})
	w := &Watch{Feed: feed, uri: testURI}
	return w, &stream{watch: w, signal: make(chan string, 1)}
}

// queued drains the notices the feed holds.
func queued(t *testing.T, w *Watch) []fetch.Notice {
	t.Helper()
	out := make([]fetch.Notice, 0, w.Queued())
	for range w.Queued() {
		n, err := w.Next(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, n)
	}
	return out
}

func signalOf(s *stream) string {
	select {
	case signal := <-s.signal:
		return signal
	default:
		return ""
	}
}

// A renewal's overlap repeats cursors; only what is past the stream cursor
// is queued, and a full queue stops the stream rather than skip a notice.
func TestReceiveDropsRepeatsAndStopsOnOverflow(t *testing.T) {
	w, s := newTestWatch(t, 2)
	w.receive(s, event(1))
	w.receive(s, event(1))
	w.receive(s, event(2))
	if w.Queued() != 2 || w.StreamCursor().Seq != 2 {
		t.Fatalf("queued %d, stream at %v; want 2 and e:2", w.Queued(), w.StreamCursor())
	}
	w.receive(s, event(3))
	if got := signalOf(s); got != signalOverflow || !s.dead.Load() {
		t.Fatalf("signal %q dead %v, want overflow and a dead stream", got, s.dead.Load())
	}
	// Room again: the stopped stream still may not jump the event it dropped.
	queued(t, w)
	w.receive(s, event(4))
	if w.StreamCursor().Seq != 2 || w.Queued() != 2 {
		t.Fatalf("stream cursor moved to %v past the overflow; the reopen would skip e:3", w.StreamCursor())
	}
}

// A reopened stream's first event queues behind the resync it owes, so
// consuming the event cannot move Cursor past the gap.
func TestReceivePaysTheOwedResyncFirst(t *testing.T) {
	w, s := newTestWatch(t, 4)
	w.Owe()
	w.receive(s, event(1))
	got := queued(t, w)
	if len(got) != 2 || !got[0].Resync || !got[0].Event.Cursor.IsZero() || got[1].Event.Cursor.Seq != 1 {
		t.Errorf("queue = %+v; want the resync at no cursor, then e:1", got)
	}
}

// A notice that names no cursor ends its stream as unplaced, which the loop
// counts; every other signal keeps its pause.
func TestSignalledEndings(t *testing.T) {
	w, _ := newTestWatch(t, 1)
	for signal, want := range map[string]ending{
		signalMalformed:             {pause: endedPause, unplaced: true},
		protocol.StatusClosing:      {pause: closingPause},
		protocol.StatusUnauthorized: {},
		signalOverflow:              {},
	} {
		if got, err := w.signalled(t.Context(), signal); err != nil || got != want {
			t.Errorf("signalled(%s) = %+v, %v; want %+v", signal, got, err, want)
		}
	}
	if _, err := w.signalled(t.Context(), protocol.StatusNotPermitted); err == nil {
		t.Error("a terminal status must end the watch")
	}
}

func TestReceiveSignalsAndSkips(t *testing.T) {
	tests := []struct {
		name       string
		meta       map[string]any
		wantSignal string
		wantQueued int
		wantResync bool
	}{
		{name: "terminal status", meta: map[string]any{MetaCursor: "e:1", MetaStatus: protocol.StatusUnauthorized}, wantSignal: protocol.StatusUnauthorized},
		{name: "malformed cursor", meta: map[string]any{MetaCursor: "nope"}, wantSignal: signalMalformed},
		// Passed, and said: the stream goes on from its cursor.
		{name: "event without a version", meta: map[string]any{MetaCursor: "e:1", MetaPath: "/a.md", MetaOp: protocol.OpPublish}, wantQueued: 1, wantResync: true},
		{name: "another subscription", meta: map[string]any{MetaSubscription: "mark://team/other/", MetaCursor: "e:1"}},
		{name: "unknown op", meta: map[string]any{MetaCursor: "e:1", MetaPath: "/a.md", MetaVersion: float64(1), MetaOp: "rename"}},
		{name: "resync", meta: map[string]any{MetaCursor: "e:9", MetaResync: true}, wantQueued: 1, wantResync: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w, s := newTestWatch(t, 4)
			w.receive(s, update(tt.meta))
			if got := signalOf(s); got != tt.wantSignal {
				t.Errorf("signal = %q, want %q", got, tt.wantSignal)
			}
			got := queued(t, w)
			if len(got) != tt.wantQueued || (tt.wantResync && !got[0].Resync) {
				t.Errorf("queued %+v, want %d (resync %v)", got, tt.wantQueued, tt.wantResync)
			}
		})
	}
}

func TestNoticeOf(t *testing.T) {
	cursor := protocol.Cursor{Epoch: "e", Seq: 3}
	hash := "sha256-" + strings.Repeat("ab", 32)
	n, err := noticeOf(map[string]any{MetaPath: "/a.md", MetaVersion: float64(2), MetaOp: protocol.OpAppend, MetaHash: hash, MetaAgent: "a", MetaUser: "u"}, cursor)
	want := protocol.WatchEvent{Cursor: cursor, Path: "/a.md", Version: 2, Op: protocol.OpAppend, Hash: hash, Agent: "a", User: "u"}
	if err != nil || n.Resync || n.Event != want {
		t.Errorf("event notice = %+v %v, want %+v", n, err, want)
	}
	// The QUIC binding's event rules apply: a hash that is not one is refused.
	if _, err := noticeOf(map[string]any{MetaPath: "/a.md", MetaVersion: float64(2), MetaOp: protocol.OpAppend, MetaHash: "sha256-x"}, cursor); err == nil {
		t.Error("a malformed hash was accepted")
	}
	if n, err := noticeOf(map[string]any{MetaResync: true}, cursor); err != nil || !n.Resync || n.Event.Cursor != cursor {
		t.Errorf("resync notice = %+v %v", n, err)
	}
}

func TestRefusal(t *testing.T) {
	named := map[string]any{MetaStatus: protocol.StatusRateLimited}
	tests := []struct {
		message      string
		data         any
		status, rest string
	}{
		{"not-permitted: access denied", nil, protocol.StatusNotPermitted, "access denied"},
		{"rate-limited: more than 64 URIs in one listen", nil, protocol.StatusRateLimited, "more than 64 URIs in one listen"},
		{"Method not found", nil, protocol.StatusBadRequest, "Method not found"},
		{"weird: thing", nil, protocol.StatusBadRequest, "weird: thing"},
		// The error's data names the status: the message is prose, not grammar.
		{"too many listens: slow down", named, protocol.StatusRateLimited, "too many listens: slow down"},
		{"not-found: gone", map[string]any{MetaStatus: "made-up"}, protocol.StatusNotFound, "gone"},
	}
	for _, tt := range tests {
		if got := refusal(tt.message, tt.data); got.Status != tt.status || got.Message != tt.rest {
			t.Errorf("refusal(%q) = %+v, want %s %q", tt.message, got, tt.status, tt.rest)
		}
	}
}

func TestRetryable(t *testing.T) {
	tests := []struct {
		err  error
		want bool
	}{
		{&fetch.StatusError{Status: protocol.StatusRateLimited}, true},
		{&fetch.StatusError{Status: protocol.StatusServerError}, true},
		{&fetch.StatusError{Status: protocol.StatusNotPermitted}, false},
		{errors.New("listen: connection reset"), true},
		{errNotModern, false},
		{errNotOffered, false},
		{errSource, false},
	}
	for _, tt := range tests {
		if got := retryable(tt.err); got != tt.want {
			t.Errorf("retryable(%v) = %v, want %v", tt.err, got, tt.want)
		}
	}
}

func TestRenewTime(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name    string
		expires time.Time
		want    time.Time
	}{
		{name: "unknown expiry", want: time.Time{}},
		{name: "a 15 minute bearer renews a minute early", expires: now.Add(15 * time.Minute), want: now.Add(14 * time.Minute)},
		{name: "a short bearer renews at half its life", expires: now.Add(10 * time.Second), want: now.Add(5 * time.Second)},
		{name: "too close to renew ahead", expires: now.Add(time.Second)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := renewTime(tt.expires, now); !got.Equal(tt.want) {
				t.Errorf("renewTime = %v, want %v", got, tt.want)
			}
		})
	}
}

// A renewal with no bearer that outlives the stream's opens nothing, since a
// listen on the same bearer would be cut with it: the stream is kept and the
// source asked again until too little life is left to open a successor.
func TestFollowKeepsTheStreamWithoutAFresherBearer(t *testing.T) {
	tests := []struct {
		name   string
		answer func(expires time.Time) (Session, error)
	}{
		{"the same bearer", func(expires time.Time) (Session, error) { return Session{Expires: expires}, nil }},
		{"an older bearer", func(expires time.Time) (Session, error) { return Session{Expires: expires.Add(-time.Second)}, nil }},
		{"an unknown expiry", func(time.Time) (Session, error) { return Session{}, nil }},
		{"no session", func(time.Time) (Session, error) { return Session{}, errors.New("no signed-in reader") }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				expires := time.Now().Add(15 * time.Minute)
				var asked atomic.Int32
				// No session has a listener: a listen on one would panic.
				w := &Watch{source: func(context.Context) (Session, error) {
					asked.Add(1)
					return tt.answer(expires)
				}}
				s := &stream{watch: w, cancel: func() {}, ended: make(chan struct{}), signal: make(chan string, 1), expires: expires}
				returned := make(chan struct{})
				go func() {
					defer close(returned)
					if next, end, err := w.follow(t.Context(), s); next != nil || err != nil || end != (ending{pause: endedPause}) {
						t.Errorf("follow = %v, %+v, %v; want the stream's own end", next, end, err)
					}
				}()

				time.Sleep(time.Until(expires) - renewBefore - time.Second)
				synctest.Wait()
				if got := asked.Load(); got != 0 {
					t.Fatalf("source asked %d times before the renewal is due", got)
				}
				time.Sleep(time.Until(expires))
				synctest.Wait()
				before := asked.Load()
				if before < 2 {
					t.Errorf("source asked %d times, want it asked again after the first answer", before)
				}
				time.Sleep(time.Minute)
				synctest.Wait()
				if got := asked.Load(); got != before {
					t.Errorf("source asked %d more times after the bearer lapsed", got-before)
				}
				if s.dead.Load() {
					t.Fatal("the stream was stopped with no successor")
				}

				close(s.ended)
				<-returned
			})
		})
	}
}
