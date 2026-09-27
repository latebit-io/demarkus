package handler

import (
	"context"
	"errors"
	"io"
	"maps"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/server/internal/auth"
	"github.com/latebit-io/demarkus/server/internal/changefeed"
)

// watchStream feeds one request in and hands every written byte to a pipe
// the test reads blocks from while the handler is still serving.
type watchStream struct {
	io.Reader
	out  *io.PipeWriter
	once sync.Once
}

// newWatchStream closes the read side at cleanup, so a block written after
// the test stopped reading fails instead of leaking a blocked handler.
func newWatchStream(t *testing.T, request string) (*watchStream, *protocol.WatchReader) {
	t.Helper()
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pr.Close() })
	return &watchStream{Reader: strings.NewReader(request), out: pw}, protocol.NewWatchReader(pr)
}

func (s *watchStream) Write(p []byte) (int, error) { return s.out.Write(p) }
func (s *watchStream) Close() error {
	s.once.Do(func() { _ = s.out.Close() })
	return nil
}

// watchHandler serves b with WATCH over the hub its writes feed.
func watchHandler(t *testing.T, b backend, ts *auth.TokenStore) *Handler {
	t.Helper()
	config := Config{Store: b.Store, Logger: discardLogger, Changes: b.Changes, HeartbeatInterval: 20 * time.Millisecond}
	if ts != nil {
		config.GetTokenStore = func() *auth.TokenStore { return ts }
	}
	return mustNew(config)
}

// startWatch serves request on its own goroutine and returns the block reader.
func startWatch(ctx context.Context, t *testing.T, h *Handler, request string) (reader *protocol.WatchReader, done <-chan struct{}) {
	t.Helper()
	stream, reader := newWatchStream(t, request)
	served := make(chan struct{})
	go func() {
		defer close(served)
		h.HandleStream(ctx, stream)
	}()
	return reader, served
}

func nextBlock(t *testing.T, reader *protocol.WatchReader) protocol.WatchBlock {
	t.Helper()
	type result struct {
		block protocol.WatchBlock
		err   error
	}
	got := make(chan result, 1)
	go func() {
		b, err := reader.Next()
		got <- result{b, err}
	}()
	select {
	case r := <-got:
		if r.err != nil {
			t.Fatalf("read block: %v", r.err)
		}
		return r.block
	case <-time.After(2 * time.Second):
		t.Fatal("no block within 2s")
		return protocol.WatchBlock{}
	}
}

// nextEvent skips heartbeats, which a short test interval interleaves freely.
func nextEvent(t *testing.T, reader *protocol.WatchReader) protocol.WatchEvent {
	t.Helper()
	for {
		block := nextBlock(t, reader)
		if block.Status == protocol.StatusOK {
			continue
		}
		event, err := block.Event()
		if err != nil {
			t.Fatalf("event: %v (block %+v)", err, block)
		}
		return event
	}
}

func publishVia(t *testing.T, h *Handler, path, body string, meta ...string) protocol.Response {
	t.Helper()
	lines := append([]string{"auth: " + testWriteToken}, meta...)
	resp := sendLookup(t, h, "PUBLISH "+path+"\n---\n"+strings.Join(lines, "\n")+"\n---\n"+body)
	if resp.Status != protocol.StatusCreated {
		t.Fatalf("publish %s: status %q body %q", path, resp.Status, resp.Body)
	}
	return resp
}

const testWriteToken = "write-anywhere"

func writeTokens(extra map[string]auth.Token) *auth.TokenStore {
	tokens := map[string]auth.Token{
		protocol.HashToken(testWriteToken): {Paths: []string{"/**"}, Operations: []string{"publish"}},
	}
	maps.Copy(tokens, extra)
	return auth.NewTokenStore(tokens)
}

func TestWatchDeliversAfterPublish(t *testing.T) {
	forEachBackend(t, func(t *testing.T, newBackend backendFactory) {
		b := newBackend(t)
		hub, h := b.Changes, watchHandler(t, b, writeTokens(nil))
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		// The inbox directory does not exist yet: a watch on it still subscribes.
		reader, done := startWatch(ctx, t, h, "WATCH /agents/me/inbox/\n")

		ack := nextBlock(t, reader)
		if ack.Status != protocol.StatusOK {
			t.Fatalf("ack status = %q", ack.Status)
		}
		if c, err := ack.Cursor(); err != nil || c != hub.Head() {
			t.Fatalf("ack cursor = %v, %v; want head %v", c, err, hub.Head())
		}

		publishVia(t, h, "/elsewhere.md", "# not in scope\n")
		publishVia(t, h, "/agents/me/inbox/01.md", "# hello\n", "agent: session-a")
		event := nextEvent(t, reader)
		if event.Path != "/agents/me/inbox/01.md" || event.Version != 1 || event.Op != protocol.OpPublish || event.Agent != "session-a" {
			t.Fatalf("event = %+v", event)
		}
		if _, ok := protocol.IsHashPath(event.Hash); !ok {
			t.Fatalf("event hash = %q", event.Hash)
		}
		fetched := sendLookup(t, h, "FETCH /"+event.Hash+"\n")
		if fetched.Status != protocol.StatusOK || fetched.Body != "# hello\n" {
			t.Fatalf("fetch by event hash: %q %q", fetched.Status, fetched.Body)
		}

		// Idle: a heartbeat carries the resume cursor, which passed the
		// out of scope event too.
		publishVia(t, h, "/elsewhere.md", "# again\n")
		head := hub.Head()
		for {
			beat := nextBlock(t, reader)
			if beat.Status != protocol.StatusOK {
				t.Fatalf("heartbeat status = %q", beat.Status)
			}
			if c, _ := beat.Cursor(); c == head {
				break
			}
		}

		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("handler did not return after the context ended")
		}
		if _, err := reader.Next(); !errors.Is(err, io.EOF) {
			t.Fatalf("after the handler returned: err = %v, want EOF", err)
		}
	})
}

// A token scoped to /a/** watching / sees /a/ and public events, never /b/.
func TestWatchOmitsEventsTheTokenMayNotRead(t *testing.T) {
	forEachBackend(t, func(t *testing.T, newBackend backendFactory) {
		ts := writeTokens(map[string]auth.Token{
			protocol.HashToken("read-a"): {Paths: []string{"/a/**"}, Operations: []string{"read"}},
			protocol.HashToken("read-b"): {Paths: []string{"/b/**"}, Operations: []string{"read"}},
		})
		h := watchHandler(t, newBackend(t), ts)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		reader, _ := startWatch(ctx, t, h, "WATCH /\n---\nauth: read-a\n---\n")
		if ack := nextBlock(t, reader); ack.Status != protocol.StatusOK {
			t.Fatalf("ack status = %q", ack.Status)
		}
		publishVia(t, h, "/b/secret.md", "# b\n")
		publishVia(t, h, "/a/mine.md", "# a\n")
		publishVia(t, h, "/c/public.md", "# c\n")
		for _, want := range []string{"/a/mine.md", "/c/public.md"} {
			ev := nextEvent(t, reader)
			if ev.Path != want {
				t.Fatalf("event path = %q, want %q", ev.Path, want)
			}
		}
		// Subscribing to the protected subtree needs the token.
		denied := sendLookup(t, h, "WATCH /b/\n")
		if denied.Status != protocol.StatusUnauthorized {
			t.Fatalf("watch on a protected prefix without a token: %q", denied.Status)
		}
		wrong := sendLookup(t, h, "WATCH /b/\n---\nauth: read-a\n---\n")
		if wrong.Status != protocol.StatusNotPermitted {
			t.Fatalf("watch on a protected prefix with the wrong token: %q", wrong.Status)
		}
	})
}

func TestWatchEndsWhenTheTokenIsRevoked(t *testing.T) {
	forEachBackend(t, func(t *testing.T, newBackend backendFactory) {
		var mu sync.Mutex
		current := writeTokens(map[string]auth.Token{
			protocol.HashToken("read-a"): {Paths: []string{"/a/**"}, Operations: []string{"read"}},
		})
		b := newBackend(t)
		h := mustNew(Config{
			Store: b.Store, Logger: discardLogger, Changes: b.Changes, HeartbeatInterval: 20 * time.Millisecond,
			GetTokenStore: func() *auth.TokenStore { mu.Lock(); defer mu.Unlock(); return current },
		})
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		reader, done := startWatch(ctx, t, h, "WATCH /a/\n---\nauth: read-a\n---\n")
		if ack := nextBlock(t, reader); ack.Status != protocol.StatusOK {
			t.Fatalf("ack status = %q", ack.Status)
		}
		// A reload that drops the token: the path stays protected by another.
		mu.Lock()
		current = writeTokens(map[string]auth.Token{
			protocol.HashToken("read-other"): {Paths: []string{"/a/**"}, Operations: []string{"read"}},
		})
		mu.Unlock()
		var last protocol.WatchBlock
		for last.Status == "" || last.Status == protocol.StatusOK {
			last = nextBlock(t, reader)
		}
		if last.Status != protocol.StatusUnauthorized {
			t.Fatalf("terminal status = %q, want unauthorized", last.Status)
		}
		if _, err := last.Cursor(); err != nil {
			t.Fatalf("terminal block cursor: %v", err)
		}
		<-done
	})
}

func TestWatchResumeAndResync(t *testing.T) {
	forEachBackend(t, func(t *testing.T, newBackend backendFactory) {
		b := newBackend(t)
		hub, h := b.Changes, watchHandler(t, b, writeTokens(nil))
		publishVia(t, h, "/a.md", "# 1\n")
		first := hub.Head()
		publishVia(t, h, "/a.md", "# 2\n")
		publishVia(t, h, "/b.md", "# 1\n")

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		reader, _ := startWatch(ctx, t, h, "WATCH /\n---\nsince: "+first.String()+"\n---\n")
		ack := nextBlock(t, reader)
		if c, _ := ack.Cursor(); ack.Status != protocol.StatusOK || c != first {
			t.Fatalf("resumed ack = %+v, want ok at the since cursor", ack)
		}
		for _, want := range []uint64{2, 3} {
			if ev := nextEvent(t, reader); ev.Cursor.Seq != want {
				t.Fatalf("replayed event = %+v, want seq %d", ev, want)
			}
		}

		// Another epoch: resync in place of the acknowledgement, with the head.
		resync := sendLookup(t, h, "WATCH /\n---\nsince: other:1\n---\n")
		if resync.Status != protocol.StatusResync || resync.Metadata["cursor"] != hub.Head().String() {
			t.Fatalf("resync block = %+v", resync)
		}
		if bad := sendLookup(t, h, "WATCH /\n---\nsince: nonsense\n---\n"); bad.Status != protocol.StatusBadRequest {
			t.Fatalf("malformed since: %q", bad.Status)
		}
		if bad := sendLookup(t, h, "WATCH /sha256-"+strings.Repeat("a", 64)+"\n"); bad.Status != protocol.StatusBadRequest {
			t.Fatalf("hash path: %q", bad.Status)
		}
	})
}

// A watch that falls behind the ring is told to resync and ends.
func TestWatchResyncsWhenLapped(t *testing.T) {
	b := fileBackendAt(t.TempDir(), changefeed.New("w", 4))
	hub, h := b.Changes, watchHandler(t, b, writeTokens(nil))
	reader, done := startWatch(t.Context(), t, h, "WATCH /\n")
	if ack := nextBlock(t, reader); ack.Status != protocol.StatusOK {
		t.Fatalf("ack status = %q", ack.Status)
	}
	// The pipe has no buffer: the handler blocks on the first event write
	// while the ring wraps twice behind it.
	for i := range 12 {
		publishVia(t, h, "/a.md", "# "+strings.Repeat("x", i+1)+"\n")
	}
	var last protocol.WatchBlock
	for last.Status != protocol.StatusResync {
		last = nextBlock(t, reader)
	}
	if c, _ := last.Cursor(); c != hub.Head() {
		t.Fatalf("resync cursor = %v, want head %v", c, hub.Head())
	}
	<-done
}

// WATCH parses as a verb, so a server without a change hub answers
// bad-request at once instead of hanging.
func TestWatchAnswersBadRequestWithoutAHub(t *testing.T) {
	forEachBackend(t, func(t *testing.T, newBackend backendFactory) {
		h := newHandler(newBackend(t), nil)
		resp := sendLookup(t, h, "WATCH /agents/me/inbox/\n---\nsince: w:1\n---\n")
		if resp.Status != protocol.StatusBadRequest || !strings.Contains(resp.Body, "unsupported verb") {
			t.Fatalf("status = %q body = %q", resp.Status, resp.Body)
		}
	})
}

// Events the token may not read are omitted, but they must not silence the
// heartbeat: the revocation recheck rides on it.
func TestWatchHeartbeatsThroughOmittedEvents(t *testing.T) {
	forEachBackend(t, func(t *testing.T, newBackend backendFactory) {
		ts := writeTokens(map[string]auth.Token{
			protocol.HashToken("read-a"): {Paths: []string{"/a/**"}, Operations: []string{"read"}},
			protocol.HashToken("read-b"): {Paths: []string{"/b/**"}, Operations: []string{"read"}},
		})
		b := newBackend(t)
		hub, h := b.Changes, watchHandler(t, b, ts)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		reader, _ := startWatch(ctx, t, h, "WATCH /\n---\nauth: read-a\n---\n")
		if ack := nextBlock(t, reader); ack.Status != protocol.StatusOK {
			t.Fatalf("ack status = %q", ack.Status)
		}
		stop := make(chan struct{})
		defer close(stop)
		go func() {
			for {
				select {
				case <-stop:
					return
				case <-time.After(time.Millisecond):
					hub.Publish(changefeed.Event{Path: "/b/noise.md", Version: 1, Op: protocol.OpPublish})
				}
			}
		}()
		if beat := nextBlock(t, reader); beat.Status != protocol.StatusOK {
			t.Fatalf("first block under unreadable events = %+v, want a heartbeat", beat)
		}
	})
}

// A block the codec refuses is skipped rather than ending the stream,
// which would replay the same event forever. Stores bound every field
// below the block limit; only a hub fed directly can produce one.
func TestWatchSkipsAnUnencodableEvent(t *testing.T) {
	forEachBackend(t, func(t *testing.T, newBackend backendFactory) {
		b := newBackend(t)
		hub, h := b.Changes, watchHandler(t, b, nil)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		reader, _ := startWatch(ctx, t, h, "WATCH /\n")
		if ack := nextBlock(t, reader); ack.Status != protocol.StatusOK {
			t.Fatalf("ack status = %q", ack.Status)
		}
		hub.Publish(changefeed.Event{Path: "/big.md", Version: 1, Op: protocol.OpPublish, Agent: strings.Repeat("a", protocol.MaxWatchBlockLength)})
		hub.Publish(changefeed.Event{Path: "/next.md", Version: 1, Op: protocol.OpPublish, Agent: "fits"})
		if ev := nextEvent(t, reader); ev.Path != "/next.md" || ev.Agent != "fits" {
			t.Fatalf("event after the oversize one = %+v, want /next.md", ev)
		}
	})
}
