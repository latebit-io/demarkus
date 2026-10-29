package gateway

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/mcpwatch"
	"github.com/latebit-io/demarkus/protocol"
	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
)

// The client/mcpwatch helper against the real gateway, on clients built the
// way the library builds its pooled ones: one per reader bearer.

// readerClient is an mcp-go streamable HTTP client for bearer, started and
// initialized on version ("" is mcp-go's default), closed when t ends.
func readerClient(t *testing.T, r *watchReplica, bearer, version string) (*mcpclient.Client, *mcp.InitializeResult) {
	t.Helper()
	c, err := mcpclient.NewStreamableHttpClient(r.url+"/mcp", transport.WithHTTPHeaders(map[string]string{"Authorization": "Bearer " + bearer}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Errorf("close %s's client: %v", bearer, err)
		}
	})
	ctx := context.Background()
	if err := c.Start(ctx); err != nil {
		t.Fatal(err)
	}
	init := mcp.InitializeRequest{}
	init.Params.ProtocolVersion = version
	init.Params.ClientInfo = mcp.Implementation{Name: "watch-test"}
	result, err := c.Initialize(ctx, init)
	if err != nil {
		t.Fatal(err)
	}
	return c, result
}

// readerListener is one reader's client as the library pools it, on the
// newest protocol, and its listener.
func readerListener(t *testing.T, r *watchReplica, bearer string) *mcpwatch.Listener {
	t.Helper()
	c, _ := readerClient(t, r, bearer, mcp.LATEST_PROTOCOL_VERSION)
	return mcpwatch.NewListener(c)
}

// sessions is a Source whose answers a test scripts: the first answer, then
// rest for every later call.
type sessions struct {
	mu          sync.Mutex
	first, rest mcpwatch.Session
	calls       int
}

func (s *sessions) source(context.Context) (mcpwatch.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.calls == 1 {
		return s.first, nil
	}
	return s.rest, nil
}

func (s *sessions) set(rest mcpwatch.Session) {
	s.mu.Lock()
	s.rest = rest
	s.mu.Unlock()
}

func (s *sessions) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func openHelper(t *testing.T, src mcpwatch.Source, r mcpwatch.Request) *mcpwatch.Watch {
	t.Helper()
	w, err := mcpwatch.Open(context.Background(), src, r)
	if err != nil {
		t.Fatalf("open %s: %v", r.URI, err)
	}
	t.Cleanup(w.Close)
	return w
}

// expectPath fails unless the next notice is an event for path.
func expectPath(t *testing.T, w *mcpwatch.Watch, path string, within time.Duration) fetch.Notice {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), within)
	defer cancel()
	n, err := w.Next(ctx)
	if err != nil {
		t.Fatalf("waiting for %s: %v", path, err)
	}
	if n.Resync || n.Event.Path != path {
		t.Fatalf("notice = %+v, want an event for %s", n, path)
	}
	return n
}

// expectQuiet fails if a notice arrives within d: nothing was repeated.
func expectQuiet(t *testing.T, w *mcpwatch.Watch, d time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	if n, err := w.Next(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Next = %+v %v, want nothing more", n, err)
	}
}

// A stream cut mid-life resumes from its cursor on whatever the source gives
// next, here another replica's client, without a resync or a repeat.
func TestHelperResumesAfterAStreamLossOnAnotherReplica(t *testing.T) {
	t.Parallel()
	s := newWatchSystem(t)
	a, b := s.replica(), s.replica()
	src := &sessions{first: mcpwatch.Session{Listener: readerListener(t, a, "alice"), Expires: time.Now().Add(time.Hour)}}
	src.rest = src.first
	w := openHelper(t, src.source, mcpwatch.Request{URI: "mark://team/h/"})
	a.publish(t, "/h/1.md")
	expectPath(t, w, "/h/1.md", 5*time.Second)

	src.set(mcpwatch.Session{Listener: readerListener(t, b, "alice"), Expires: time.Now().Add(time.Hour)})
	a.publish(t, "/h/2.md")
	// Bring b to the head first, as in TestListenResumesOnAnotherReplica.
	if _, err := b.composite.Fetch(context.Background(), fetch.FetchRequest{Host: watchWorld, Path: "/h/2.md"}); err != nil {
		t.Fatal(err)
	}
	a.server.CloseClientConnections()
	expectPath(t, w, "/h/2.md", 10*time.Second)
	b.publish(t, "/h/3.md")
	expectPath(t, w, "/h/3.md", 5*time.Second)
	expectQuiet(t, w, 300*time.Millisecond)
	if src.count() < 2 {
		t.Fatalf("source called %d times, want the reopen to ask it again", src.count())
	}
}

// A bearer the source says will lapse is replaced before it does: the new
// stream opens from the cursor under the fresh bearer, the old one closes,
// and the overlap repeats nothing.
func TestHelperRenewsAheadOfABearerRotation(t *testing.T) {
	t.Parallel()
	r := newWatchSystem(t).replica()
	lapses := time.Now().Add(briefBearer)
	src := &sessions{
		first: mcpwatch.Session{Listener: readerListener(t, r, "alice-brief"), Expires: lapses},
		rest:  mcpwatch.Session{Listener: readerListener(t, r, "alice"), Expires: time.Now().Add(time.Hour)},
	}
	w := openHelper(t, src.source, mcpwatch.Request{URI: "mark://team/r/"})
	r.publish(t, "/r/1.md")
	expectPath(t, w, "/r/1.md", 5*time.Second)

	deadline := time.Now().Add(5 * time.Second)
	for src.count() < 2 {
		if time.Now().After(deadline) {
			t.Fatal("the helper never renewed")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if time.Now().After(lapses) {
		t.Fatal("renewed only after the bearer lapsed")
	}
	r.publish(t, "/r/2.md")
	expectPath(t, w, "/r/2.md", 5*time.Second)
	time.Sleep(time.Until(lapses) + 500*time.Millisecond)
	r.publish(t, "/r/3.md")
	expectPath(t, w, "/r/3.md", 5*time.Second)
	expectQuiet(t, w, 300*time.Millisecond)
	if err := w.Err(); err != nil {
		t.Fatalf("watch ended: %v", err)
	}
}

// With no expiry known, the server's unauthorized ends the lapsed stream and
// the helper renews from the source at once.
func TestHelperRenewsWhenTheServerEndsALapsedStream(t *testing.T) {
	t.Parallel()
	r := newWatchSystem(t).replica()
	src := &sessions{
		first: mcpwatch.Session{Listener: readerListener(t, r, "alice-brief")},
		rest:  mcpwatch.Session{Listener: readerListener(t, r, "alice")},
	}
	w := openHelper(t, src.source, mcpwatch.Request{URI: "mark://team/u/"})
	r.publish(t, "/u/1.md")
	expectPath(t, w, "/u/1.md", 5*time.Second)
	time.Sleep(briefBearer + time.Second)
	r.publish(t, "/u/2.md")
	expectPath(t, w, "/u/2.md", 5*time.Second)
	if src.count() != 2 {
		t.Fatalf("source called %d times, want one renewal", src.count())
	}
}

// A consumer that stops reading overflows the queue; the stream stops and
// reopens from the last queued cursor once read again, losing nothing.
func TestHelperSlowConsumerLosesNothing(t *testing.T) {
	t.Parallel()
	r := newWatchSystem(t).replica()
	session := mcpwatch.Session{Listener: readerListener(t, r, "alice"), Expires: time.Now().Add(time.Hour)}
	src := &sessions{first: session, rest: session}
	w := openHelper(t, src.source, mcpwatch.Request{URI: "mark://team/q/", Queue: 4})
	const total = 20
	for i := range total {
		r.publish(t, "/q/"+string(rune('a'+i))+".md")
	}
	for i := range total {
		expectPath(t, w, "/q/"+string(rune('a'+i))+".md", 5*time.Second)
	}
	expectQuiet(t, w, 300*time.Millisecond)
	if src.count() < 2 {
		t.Fatalf("source called %d times, want a reopen after the overflow", src.count())
	}
}

func TestHelperRefusalsAndClose(t *testing.T) {
	t.Parallel()
	r := newWatchSystem(t).replica()
	session := mcpwatch.Session{Listener: readerListener(t, r, "alice"), Expires: time.Now().Add(time.Hour)}
	src := &sessions{first: session, rest: session}
	_, err := mcpwatch.Open(context.Background(), src.source, mcpwatch.Request{URI: "mark://nope/"})
	var refused *fetch.StatusError
	if !errors.As(err, &refused) || refused.Status != protocol.StatusNotFound {
		t.Fatalf("open on an unknown world = %v, want not-found", err)
	}
	failing := func(context.Context) (mcpwatch.Session, error) {
		return mcpwatch.Session{}, errors.New("no reader signed in")
	}
	if _, err := mcpwatch.Open(context.Background(), failing, mcpwatch.Request{URI: "mark://team/"}); err == nil {
		t.Fatal("open without a session succeeded")
	}
	w, err := mcpwatch.Open(context.Background(), src.source, mcpwatch.Request{URI: "mark://Team/c/"})
	if err != nil {
		t.Fatalf("open with an uppercase world: %v", err)
	}
	r.publish(t, "/c/1.md")
	expectPath(t, w, "/c/1.md", 5*time.Second)
	w.Close()
	r.waitSubscriptions(t, 0)
}
