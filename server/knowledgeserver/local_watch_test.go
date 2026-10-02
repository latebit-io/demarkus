package knowledgeserver

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/protocol"
)

func newAliceServer(t *testing.T) (*Server, *worldsTestHarness) {
	t.Helper()
	tokens := writeTokens(t, t.TempDir(), "alice")
	h := newWorldsHarness(t, "worlds:\n"+worldFragment("alice", testWorldID, tokens, true))
	return &Server{worlds: h.manager, logger: slog.New(slog.DiscardHandler)}, h
}

func publishDoc(t *testing.T, server *Server, path string) {
	t.Helper()
	ctx := protocol.WithGrant(context.Background(), protocol.Grant{Label: "test", Paths: []string{"/**"}})
	resp, err := server.Exchange(ctx, bearerAuthority, bearerRequest(protocol.VerbPublish, path, ""))
	if err != nil || resp.Status != protocol.StatusCreated {
		t.Fatalf("publish %s: %q %v", path, resp.Status, err)
	}
}

// watchAlice opens an in-process watch of alice's world and reads its first block.
func watchAlice(ctx context.Context, t *testing.T, server *Server, since protocol.Cursor) (net.Conn, *protocol.WatchReader, protocol.WatchBlock) {
	t.Helper()
	req := bearerRequest(protocol.VerbWatch, "/", "")
	if !since.IsZero() {
		req.Metadata["since"] = since.String()
	}
	conn, err := server.Watch(ctx, bearerAuthority, req)
	if err != nil {
		t.Fatalf("watch: %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Close(); err != nil {
			t.Errorf("close watch: %v", err)
		}
	})
	if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	reader := protocol.NewWatchReader(conn)
	first, err := reader.Next()
	if err != nil {
		t.Fatalf("first block: %v", err)
	}
	return conn, reader, first
}

// nextEvent skips heartbeats to the next event block.
func nextEvent(t *testing.T, reader *protocol.WatchReader) protocol.WatchEvent {
	t.Helper()
	for {
		block, err := reader.Next()
		if err != nil {
			t.Fatalf("next block: %v", err)
		}
		if block.Status == protocol.StatusOK {
			continue
		}
		if block.Status != "" {
			t.Fatalf("block = %+v, want an event", block)
		}
		ev, err := block.Event()
		if err != nil {
			t.Fatalf("event: %v", err)
		}
		return ev
	}
}

// waitNoWatches allows well under the 10 s write budget, so a world that
// only lets go at its deadline fails it.
func waitNoWatches(t *testing.T, h *worldsTestHarness) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for h.runtime(t, "alice").Watches() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("alice still holds %d watches", h.runtime(t, "alice").Watches())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestWatchStreamsChangesAndResumesAfterACursor(t *testing.T) {
	server, h := newAliceServer(t)
	conn, reader, ack := watchAlice(context.Background(), t, server, protocol.Cursor{})
	if ack.Status != protocol.StatusOK {
		t.Fatalf("ack = %+v", ack)
	}
	if got := h.runtime(t, "alice").Watches(); got != 1 {
		t.Errorf("open watches = %d, want 1: the poll backstop counts them", got)
	}
	publishDoc(t, server, "/a.md")
	seen := nextEvent(t, reader)
	if seen.Path != "/a.md" || seen.Op != protocol.OpPublish || seen.Version != 1 {
		t.Errorf("event = %+v, want publish /a.md v1", seen)
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	waitNoWatches(t, h)

	publishDoc(t, server, "/b.md")
	_, reader, ack = watchAlice(context.Background(), t, server, seen.Cursor)
	if ack.Status != protocol.StatusOK {
		t.Fatalf("resume ack = %+v", ack)
	}
	if ev := nextEvent(t, reader); ev.Path != "/b.md" {
		t.Errorf("resumed event = %+v, want /b.md only", ev)
	}
}

func TestWatchAnswersResyncForAnotherEpoch(t *testing.T) {
	server, _ := newAliceServer(t)
	_, _, first := watchAlice(context.Background(), t, server, protocol.Cursor{Epoch: "another", Seq: 1})
	if first.Status != protocol.StatusResync {
		t.Fatalf("first block = %+v, want resync", first)
	}
	if _, err := first.Cursor(); err != nil {
		t.Errorf("resync carries no cursor to rebuild from: %v", err)
	}
}

// The open's context bounds only the open, as a dial's does: the watch
// carries changes made after it ends.
func TestWatchOutlivesItsOpenContext(t *testing.T) {
	server, _ := newAliceServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	conn, reader, _ := watchAlice(ctx, t, server, protocol.Cursor{})
	defer conn.Close() //nolint:errcheck // the test is over either way
	cancel()
	publishDoc(t, server, "/a.md")
	if ev := nextEvent(t, reader); ev.Path != "/a.md" {
		t.Errorf("event after the open's context ended = %+v, want /a.md", ev)
	}
}

// Closing lets go of the world at once, even with an event written that
// nobody reads.
func TestWatchCloseLetsGoOfTheWorld(t *testing.T) {
	server, h := newAliceServer(t)
	conn, _, _ := watchAlice(context.Background(), t, server, protocol.Cursor{})
	publishDoc(t, server, "/a.md")
	time.Sleep(50 * time.Millisecond) // the event's write is now blocked on the pipe
	if err := conn.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	waitNoWatches(t, h)
}

func TestWatchRefusesUnknownAuthorityAndOtherVerbs(t *testing.T) {
	server, _ := newAliceServer(t)
	watch := bearerRequest(protocol.VerbWatch, "/", "")
	if _, err := server.Watch(context.Background(), otherAuthority, watch); !errors.Is(err, ErrUnknownAuthority) {
		t.Errorf("unknown authority: err = %v, want ErrUnknownAuthority", err)
	}
	if _, err := server.Watch(context.Background(), bearerAuthority, bearerRequest(protocol.VerbFetch, "/a.md", "")); err == nil {
		t.Error("a FETCH over Watch was accepted")
	}
}
