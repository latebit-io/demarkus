package fetch

import (
	"errors"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/protocol"
)

func feedEvent(seq uint64) *protocol.WatchEvent {
	return &protocol.WatchEvent{Cursor: protocol.Cursor{Epoch: "e", Seq: seq}, Path: "/a.md", Version: 1, Op: protocol.OpPublish}
}

func newTestFeed(t *testing.T, queue int) *Feed {
	t.Helper()
	f, err := NewFeed(protocol.Cursor{}, queue)
	if err != nil {
		t.Fatal(err)
	}
	f.Start(func() {})
	return f
}

// Only what is past the stream cursor is queued, an unknown op is passed,
// and a full queue refuses the event rather than skip it.
func TestFeedTakesEachEventOnce(t *testing.T) {
	f := newTestFeed(t, 2)
	rename := feedEvent(2)
	rename.Op = "rename"
	for _, ev := range []*protocol.WatchEvent{feedEvent(1), feedEvent(1), rename, feedEvent(3)} {
		if !f.Event(ev) {
			t.Fatalf("event %v refused with room in the queue", ev.Cursor)
		}
	}
	if len(f.notices) != 2 || f.Queued() != 2 || f.StreamCursor().Seq != 3 {
		t.Fatalf("queued %d, stream at %v; want 2 and e:3", len(f.notices), f.StreamCursor())
	}
	if f.Event(feedEvent(4)) || f.StreamCursor().Seq != 3 {
		t.Fatalf("a full queue took e:4, stream at %v; the reopen would skip it", f.StreamCursor())
	}
	if _, err := NewFeed(protocol.Cursor{}, -1); err == nil {
		t.Error("a negative queue was accepted")
	}
}

// A resync is owed until queued and goes ahead of any later event, at the
// cursor the stream continues from, so Cursor never passes the gap.
func TestFeedPaysAnOwedResyncFirst(t *testing.T) {
	f := newTestFeed(t, 2)
	f.Event(feedEvent(1))
	f.Event(feedEvent(2))
	at := protocol.Cursor{Epoch: "e", Seq: 9}
	if f.Resync(at) || f.Pay() || f.Event(feedEvent(10)) {
		t.Fatal("a full queue took a resync or the event behind it")
	}
	if f.StreamCursor() != at {
		t.Fatalf("stream at %v, want the resync's %v", f.StreamCursor(), at)
	}
	<-f.notices
	<-f.notices
	if !f.Event(feedEvent(10)) {
		t.Fatal("the event was refused with room for it and the resync")
	}
	if first, second := <-f.notices, <-f.notices; !first.Resync || first.Event.Cursor != at || second.Event.Cursor.Seq != 10 {
		t.Errorf("queue = %+v, %+v; want the resync at %v, then e:10", first, second, at)
	}
	f.Owe()
	if !f.Pay() || len(f.notices) != 1 {
		t.Error("an owed resync was not paid into an empty queue")
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

// A stream without progress reopens after a growing pause; progress resets
// it, and blocks no stream can pass end the watch with their cause.
func TestPaceGrowsWithoutProgress(t *testing.T) {
	var p Pace
	for i, want := range []time.Duration{retryDelay, 2 * retryDelay, 4 * retryDelay} {
		if got := p.AfterStream(false, 0); got != want {
			t.Errorf("pause %d = %v, want %v", i, got, want)
		}
	}
	if got := p.AfterStream(false, time.Second); got != time.Second {
		t.Errorf("pause under a floor = %v, want the floor", got)
	}
	for range 10 {
		p.AfterStream(false, 0)
	}
	if got := p.AfterStream(false, 0); got != maxPause {
		t.Errorf("pause = %v, want the cap %v", got, maxPause)
	}
	if got := p.AfterStream(true, 0); got != 0 {
		t.Errorf("pause after progress = %v, want none", got)
	}
	if got := p.AfterStream(false, 0); got != retryDelay {
		t.Errorf("pause after a reset = %v, want %v", got, retryDelay)
	}
	cause := errors.New("no cursor")
	for i := 1; i < maxUnreadable; i++ {
		if err := p.Unreadable(cause); err != nil {
			t.Fatalf("try %d ended the watch: %v", i, err)
		}
	}
	if err := p.Unreadable(cause); !errors.Is(err, cause) {
		t.Errorf("try %d = %v, want the cause", maxUnreadable, err)
	}
	if p.AfterFailedOpen() != retryDelay || p.AfterFailedOpen() != 2*retryDelay {
		t.Error("failed opens do not back off from the first delay")
	}
	p.Opened()
	if p.AfterFailedOpen() != retryDelay {
		t.Error("an open did not reset the backoff")
	}
}
