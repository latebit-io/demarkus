package fetch

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/protocol"
)

// pipeFeed serves each dialed stream from a script over net.Pipe, the shape
// of an in-process watch. Every stream but the last hangs up after its script.
type pipeFeed struct {
	t        *testing.T
	mu       sync.Mutex
	sinces   []string
	scripts  [][]protocol.WatchBlock
	released chan error // why each stream the client released ended
}

func (f *pipeFeed) dial(_ context.Context, req protocol.Request) (WatchStream, error) {
	f.mu.Lock()
	n := len(f.sinces)
	f.sinces = append(f.sinces, req.Metadata["since"])
	f.mu.Unlock()
	if n >= len(f.scripts) {
		return nil, errors.New("no more streams scripted")
	}
	client, server := net.Pipe()
	go func() {
		for _, block := range f.scripts[n] {
			if _, err := block.WriteTo(server); err != nil {
				return
			}
		}
		if n < len(f.scripts)-1 {
			if err := server.Close(); err != nil {
				f.t.Errorf("hang up: %v", err)
			}
		}
		// Hold the stream open until the client end closes it.
		if _, err := io.Copy(io.Discard, server); err != nil && !errors.Is(err, io.ErrClosedPipe) {
			f.t.Errorf("drain: %v", err)
		}
	}()
	return releaseProbe{Conn: client, t: f.t, released: f.released}, nil
}

// releaseProbe is a conn stream that reports why each stream was released.
type releaseProbe struct {
	net.Conn
	t        *testing.T
	released chan<- error
}

func (s releaseProbe) Abort() {
	if err := s.Close(); err != nil {
		s.t.Errorf("close pipe: %v", err)
	}
}

func (s releaseProbe) Release(err error) {
	s.Abort()
	s.released <- err
}

func (f *pipeFeed) since(i int) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sinces[i]
}

func TestNewWatchOverAConnReopensFromItsCursor(t *testing.T) {
	at := func(seq uint64) protocol.Cursor { return protocol.Cursor{Epoch: "e1", Seq: seq} }
	event := func(seq uint64, path string) protocol.WatchBlock {
		return protocol.WatchEvent{Cursor: at(seq), Path: path, Version: 1, Op: protocol.OpPublish}.Block()
	}
	f := &pipeFeed{t: t, released: make(chan error, 2), scripts: [][]protocol.WatchBlock{
		{protocol.WatchControl(protocol.StatusOK, at(1)), event(2, "/a.md")},
		{protocol.WatchControl(protocol.StatusOK, at(2)), event(3, "/b.md")},
	}}
	w, err := NewWatch(context.Background(), f.dial, WatchRequest{Path: "/"}, 2*time.Second)
	if err != nil {
		t.Fatalf("NewWatch: %v", err)
	}
	expectEvent(t, w, "/a.md")
	expectEvent(t, w, "/b.md")
	if got, want := f.since(1), at(2).String(); got != want {
		t.Errorf("reopen since = %q, want %q", got, want)
	}
	if got := w.Cursor(); got != at(3) {
		t.Errorf("cursor = %v, want %v", got, at(3))
	}
	w.Close()
	// The cut stream is released with its read error, so a dead connection
	// could be evicted; the one Close ended is released clean.
	for i, cut := range []bool{true, false} {
		select {
		case got := <-f.released:
			if (got != nil) != cut {
				t.Errorf("stream %d released with %v, want an error %v", i, got, cut)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("stream %d was not released", i)
		}
	}
}
