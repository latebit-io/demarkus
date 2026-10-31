package mcpwatch

import (
	"errors"
	"maps"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/protocol"
	"github.com/mark3labs/mcp-go/mcp"
)

const testURI = "mark://team/docs/"

func update(meta map[string]any) mcp.JSONRPCNotification {
	full := map[string]any{MetaSubscription: testURI}
	maps.Copy(full, meta)
	return mcp.JSONRPCNotification{Notification: mcp.Notification{
		Method: string(mcp.MethodNotificationResourceUpdated),
		Params: mcp.NotificationParams{Meta: full, AdditionalFields: map[string]any{"uri": "mark://team/docs/a.md"}},
	}}
}

func event(seq int) mcp.JSONRPCNotification {
	return update(map[string]any{MetaCursor: "e:" + strconv.Itoa(seq), MetaPath: "/docs/a.md", MetaVersion: float64(seq), MetaOp: protocol.OpPublish})
}

func newTestWatch(queue int) (*Watch, *stream) {
	w := &Watch{uri: testURI, notices: make(chan fetch.Notice, queue), room: make(chan struct{}, 1), done: make(chan struct{})}
	return w, &stream{watch: w, signal: make(chan string, 1)}
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
	w, s := newTestWatch(2)
	w.receive(s, event(1))
	w.receive(s, event(1))
	w.receive(s, event(2))
	if len(w.notices) != 2 || w.stream.Seq != 2 {
		t.Fatalf("queued %d, stream at %v; want 2 and e:2", len(w.notices), w.stream)
	}
	w.receive(s, event(3))
	if got := signalOf(s); got != signalOverflow || !s.dead {
		t.Fatalf("signal %q dead %v, want overflow and a dead stream", got, s.dead)
	}
	w.receive(s, event(4))
	if w.stream.Seq != 2 {
		t.Fatalf("stream cursor moved to %v past the overflow; the reopen would skip e:3", w.stream)
	}
}

// A reopen from no cursor owes a resync, queued at the stream cursor; a full
// queue stops the stream and keeps the debt for the next reopen.
func TestResyncPaysWhatAReopenOwes(t *testing.T) {
	tests := []struct {
		name       string
		owed       bool
		queued     int // events already in a queue of 2
		wantResync bool
		wantOwed   bool
		wantSignal string
	}{
		{name: "nothing owed", queued: 1},
		{name: "owed", owed: true, queued: 1, wantResync: true},
		{name: "owed to a full queue", owed: true, queued: 2, wantOwed: true, wantSignal: signalOverflow},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w, s := newTestWatch(2)
			for seq := 1; seq <= tt.queued; seq++ {
				w.receive(s, event(seq))
			}
			w.owed = tt.owed
			w.resync(s)
			if got := signalOf(s); got != tt.wantSignal || w.owed != tt.wantOwed {
				t.Fatalf("signal %q owed %v, want %q and %v", got, w.owed, tt.wantSignal, tt.wantOwed)
			}
			var last fetch.Notice
			for range len(w.notices) {
				last = <-w.notices
			}
			if last.Resync != tt.wantResync || (tt.wantResync && last.Event.Cursor != w.stream) {
				t.Errorf("last notice = %+v, want resync %v at %v", last, tt.wantResync, w.stream)
			}
		})
	}
}

func TestReceiveSignalsAndSkips(t *testing.T) {
	tests := []struct {
		name       string
		meta       map[string]any
		wantSignal string
		wantQueued int
	}{
		{name: "terminal status", meta: map[string]any{MetaCursor: "e:1", MetaStatus: protocol.StatusUnauthorized}, wantSignal: protocol.StatusUnauthorized},
		{name: "malformed cursor", meta: map[string]any{MetaCursor: "nope"}, wantSignal: signalMalformed},
		{name: "event without a version", meta: map[string]any{MetaCursor: "e:1", MetaPath: "/a.md", MetaOp: protocol.OpPublish}, wantSignal: signalMalformed},
		{name: "another subscription", meta: map[string]any{MetaSubscription: "mark://team/other/", MetaCursor: "e:1"}},
		{name: "unknown op", meta: map[string]any{MetaCursor: "e:1", MetaPath: "/a.md", MetaVersion: float64(1), MetaOp: "rename"}},
		{name: "resync", meta: map[string]any{MetaCursor: "e:9", MetaResync: true}, wantQueued: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w, s := newTestWatch(4)
			w.receive(s, update(tt.meta))
			if got := signalOf(s); got != tt.wantSignal {
				t.Errorf("signal = %q, want %q", got, tt.wantSignal)
			}
			if len(w.notices) != tt.wantQueued {
				t.Errorf("queued %d, want %d", len(w.notices), tt.wantQueued)
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
	tests := []struct {
		message, status, rest string
	}{
		{"not-permitted: access denied", protocol.StatusNotPermitted, "access denied"},
		{"rate-limited: more than 64 URIs in one listen", protocol.StatusRateLimited, "more than 64 URIs in one listen"},
		{"Method not found", protocol.StatusBadRequest, "Method not found"},
		{"weird: thing", protocol.StatusBadRequest, "weird: thing"},
	}
	for _, tt := range tests {
		if got := refusal(tt.message); got.Status != tt.status || got.Message != tt.rest {
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
		name              string
		expires, previous time.Time
		want              time.Time
	}{
		{name: "unknown expiry", want: time.Time{}},
		{name: "a 15 minute bearer renews a minute early", expires: now.Add(15 * time.Minute), want: now.Add(14 * time.Minute)},
		{name: "a short bearer renews at half its life", expires: now.Add(10 * time.Second), want: now.Add(5 * time.Second)},
		{name: "too close to renew ahead", expires: now.Add(time.Second)},
		{name: "no fresher than the one it replaced", expires: now.Add(time.Hour), previous: now.Add(time.Hour)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := renewTime(tt.expires, tt.previous, now); !got.Equal(tt.want) {
				t.Errorf("renewTime = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestAfter(t *testing.T) {
	last := protocol.Cursor{Epoch: "e", Seq: 5}
	for _, tt := range []struct {
		cursor protocol.Cursor
		want   bool
	}{
		{protocol.Cursor{Epoch: "e", Seq: 6}, true},
		{protocol.Cursor{Epoch: "e", Seq: 5}, false},
		{protocol.Cursor{Epoch: "e", Seq: 4}, false},
		{protocol.Cursor{Epoch: "f", Seq: 1}, true},
	} {
		if got := after(tt.cursor, last); got != tt.want {
			t.Errorf("after(%v, %v) = %v", tt.cursor, last, got)
		}
	}
	if !after(protocol.Cursor{Epoch: "e", Seq: 1}, protocol.Cursor{}) {
		t.Error("nothing consumed yet: every cursor is new")
	}
}
