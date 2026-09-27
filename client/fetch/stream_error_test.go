package fetch

import (
	"sync/atomic"
	"testing"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/quic-go/quic-go"
)

// answerOK writes an ok response and closes the stream.
func answerOK(stream *quic.Stream) {
	_, _ = protocol.Response{Status: protocol.StatusOK, Body: "# OK\n"}.WriteTo(stream)
	_ = stream.Close()
}

// A reset stream is one request's failure: the client retries on a new stream
// over the same pooled connection instead of redialing.
func TestStreamResetKeepsPooledConnection(t *testing.T) {
	var requests atomic.Int32
	srv := serveStreams(t, nil, func(_ *quic.Conn, stream *quic.Stream) {
		if _, err := protocol.ParseRequest(stream); err != nil {
			return
		}
		if requests.Add(1) == 1 {
			stream.CancelWrite(1)
			return
		}
		answerOK(stream)
	})
	c := NewClient(Options{Insecure: true})
	t.Cleanup(c.Close)

	result, err := c.Fetch(t.Context(), FetchRequest{Host: srv.Addr, Path: "/a.md"})
	if err != nil {
		t.Fatalf("fetch after a stream reset: %v", err)
	}
	if result.Response.Status != protocol.StatusOK {
		t.Fatalf("status = %q, want ok", result.Response.Status)
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("server saw %d requests, want the reset one retried once", got)
	}
	if got := srv.Conns.Load(); got != 1 {
		t.Fatalf("server accepted %d connections, want the reset stream to keep the one", got)
	}
}

// A connection closed by the server is evicted: the retry dials a fresh one.
func TestConnectionCloseEvictsPooledConnection(t *testing.T) {
	var requests atomic.Int32
	srv := serveStreams(t, nil, func(conn *quic.Conn, stream *quic.Stream) {
		if _, err := protocol.ParseRequest(stream); err != nil {
			return
		}
		if requests.Add(1) == 1 {
			_ = conn.CloseWithError(0, "restarting")
			return
		}
		answerOK(stream)
	})
	c := NewClient(Options{Insecure: true})
	t.Cleanup(c.Close)

	result, err := c.Fetch(t.Context(), FetchRequest{Host: srv.Addr, Path: "/a.md"})
	if err != nil {
		t.Fatalf("fetch after a connection close: %v", err)
	}
	if result.Response.Status != protocol.StatusOK {
		t.Fatalf("status = %q, want ok", result.Response.Status)
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("server saw %d requests, want the lost one retried once", got)
	}
	if got := srv.Conns.Load(); got != 2 {
		t.Fatalf("server accepted %d connections, want the closed one replaced", got)
	}
}
