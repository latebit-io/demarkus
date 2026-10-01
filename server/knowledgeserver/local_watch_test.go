package knowledgeserver

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/protocol"
)

const aliceAuthority = "alice.memory.svc.cluster.local"

func newAliceServer(t *testing.T) (*Server, *worldsTestHarness) {
	t.Helper()
	tokens := writeTokens(t, t.TempDir(), "alice")
	h := newWorldsHarness(t, "worlds:\n"+worldFragment("alice", testWorldID, tokens, true))
	return &Server{worlds: h.manager}, h
}

func publishDoc(t *testing.T, server *Server, path string) {
	t.Helper()
	ctx := protocol.WithGrant(context.Background(), protocol.Grant{Label: "test", Paths: []string{"/**"}})
	req := protocol.Request{Verb: protocol.VerbPublish, Path: path, Body: "# " + path + "\n", Metadata: map[string]string{"expected-version": "0"}}
	resp, err := server.Exchange(ctx, aliceAuthority, req)
	if err != nil || resp.Status != protocol.StatusCreated {
		t.Fatalf("publish %s: %q %v", path, resp.Status, err)
	}
}

// watchAlice opens an in-process watch of alice's world and reads its first block.
func watchAlice(ctx context.Context, t *testing.T, server *Server, since protocol.Cursor) (net.Conn, *protocol.WatchReader, protocol.WatchBlock) {
	t.Helper()
	req := protocol.Request{Verb: protocol.VerbWatch, Path: "/", Metadata: map[string]string{}}
	if !since.IsZero() {
		req.Metadata["since"] = since.String()
	}
	conn, err := server.Watch(ctx, aliceAuthority, req)
	if err != nil {
		t.Fatalf("watch: %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
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

func (h *worldsTestHarness) watches(world string) int {
	h.manager.mu.Lock()
	defer h.manager.mu.Unlock()
	return h.manager.entries[world].runtime.Watches()
}

func waitNoWatches(t *testing.T, h *worldsTestHarness) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for h.watches("alice") != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("alice still holds %d watches", h.watches("alice"))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestWatchStreamsChangesInProcessAndReleasesOnClose(t *testing.T) {
	server, h := newAliceServer(t)
	conn, reader, ack := watchAlice(context.Background(), t, server, protocol.Cursor{})
	if ack.Status != protocol.StatusOK {
		t.Fatalf("ack = %+v", ack)
	}
	if h.watches("alice") != 1 {
		t.Errorf("open watches = %d, want 1: the poll backstop counts them", h.watches("alice"))
	}
	publishDoc(t, server, "/a.md")
	ev := nextEvent(t, reader)
	if ev.Path != "/a.md" || ev.Op != protocol.OpPublish || ev.Version != 1 {
		t.Errorf("event = %+v, want publish /a.md v1", ev)
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	waitNoWatches(t, h)
}

func TestWatchResumesAfterACursor(t *testing.T) {
	server, _ := newAliceServer(t)
	conn, reader, _ := watchAlice(context.Background(), t, server, protocol.Cursor{})
	publishDoc(t, server, "/a.md")
	seen := nextEvent(t, reader)
	if err := conn.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	publishDoc(t, server, "/b.md")

	_, reader, ack := watchAlice(context.Background(), t, server, seen.Cursor)
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

func TestWatchEndsWithItsContext(t *testing.T) {
	server, h := newAliceServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	_, reader, _ := watchAlice(ctx, t, server, protocol.Cursor{})
	cancel()
	for {
		block, err := reader.Next()
		if err != nil {
			break // the world closed its end
		}
		if block.Status != protocol.StatusOK && block.Status != protocol.StatusClosing {
			t.Fatalf("block after cancel = %+v", block)
		}
	}
	waitNoWatches(t, h)
}

func TestWatchRefusesUnknownAuthorityAndOtherVerbs(t *testing.T) {
	server, _ := newAliceServer(t)
	watch := protocol.Request{Verb: protocol.VerbWatch, Path: "/"}
	if _, err := server.Watch(context.Background(), "bob.memory.svc.cluster.local", watch); !errors.Is(err, ErrUnknownAuthority) {
		t.Errorf("unknown authority: err = %v, want ErrUnknownAuthority", err)
	}
	fetch := protocol.Request{Verb: protocol.VerbFetch, Path: "/a.md"}
	if _, err := server.Watch(context.Background(), aliceAuthority, fetch); err == nil {
		t.Error("a FETCH over Watch was accepted")
	}
}
