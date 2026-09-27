package worldruntime

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/server/internal/changefeed"
	"github.com/latebit-io/demarkus/server/internal/quicserve"
)

// pipeStream feeds one request in and streams every written byte to a pipe
// the test reads blocks from while the runtime is still serving.
type pipeStream struct {
	io.Reader
	out  *io.PipeWriter
	once sync.Once
}

// newPipeStream closes the read side at cleanup, so a block written after
// the test stopped reading fails instead of blocking the drain.
func newPipeStream(t *testing.T, request string) (*pipeStream, *protocol.WatchReader) {
	t.Helper()
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pr.Close() })
	return &pipeStream{Reader: strings.NewReader(request), out: pw}, protocol.NewWatchReader(pr)
}

func (s *pipeStream) Write(p []byte) (int, error)      { return s.out.Write(p) }
func (s *pipeStream) SetReadDeadline(time.Time) error  { return nil }
func (s *pipeStream) SetWriteDeadline(time.Time) error { return nil }
func (s *pipeStream) Close() error                     { s.once.Do(func() { _ = s.out.Close() }); return nil }

// serveWatch starts one WATCH on the runtime and returns after its ack.
func serveWatch(ctx context.Context, t *testing.T, runtime *Runtime, request string) (reader *protocol.WatchReader, served <-chan struct{}) {
	t.Helper()
	stream, reader := newPipeStream(t, request)
	done := make(chan struct{})
	go func() {
		defer close(done)
		runtime.ServeStream(ctx, testAddr("127.0.0.1:1234"), stream)
	}()
	return reader, done
}

func readBlock(t *testing.T, reader *protocol.WatchReader) protocol.WatchBlock {
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

// Close tells every open watch closing and the drain completes.
func TestRuntimeCloseSendsClosingToWatches(t *testing.T) {
	hub := changefeed.New("w", 0)
	runtime := newTestRuntime(t, &Config{Changes: hub, RequestTimeout: time.Second})
	ctx := quicserve.WithConnState(context.Background())
	reader, served := serveWatch(ctx, t, runtime, "WATCH /\n")
	if ack := readBlock(t, reader); ack.Status != protocol.StatusOK {
		t.Fatalf("ack status = %q", ack.Status)
	}
	if runtime.Watches() != 1 {
		t.Fatalf("watches = %d, want 1", runtime.Watches())
	}

	closed := make(chan error, 1)
	go func() { closed <- runtime.Close() }()
	if last := readBlock(t, reader); last.Status != protocol.StatusClosing {
		t.Fatalf("terminal status = %q, want closing", last.Status)
	}
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not return after the watch ended")
	}
	<-served
	if runtime.Watches() != 0 {
		t.Fatalf("watches after close = %d", runtime.Watches())
	}
}

// Drain ends watches with closing while requests are still served, so a
// listener shutdown is not held by subscriptions.
func TestRuntimeDrainEndsWatchesAndKeepsServing(t *testing.T) {
	hub := changefeed.New("w", 0)
	runtime := newTestRuntime(t, &Config{Changes: hub, RequestTimeout: time.Second})
	t.Cleanup(func() {
		if err := runtime.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	ctx := quicserve.WithConnState(context.Background())
	reader, served := serveWatch(ctx, t, runtime, "WATCH /\n")
	if ack := readBlock(t, reader); ack.Status != protocol.StatusOK {
		t.Fatalf("ack status = %q", ack.Status)
	}
	runtime.Drain()
	if last := readBlock(t, reader); last.Status != protocol.StatusClosing {
		t.Fatalf("terminal status = %q, want closing", last.Status)
	}
	select {
	case <-served:
	case <-time.After(2 * time.Second):
		t.Fatal("watch stream did not end after Drain")
	}
	if got := serveStatus(t, runtime, "FETCH /health\n"); got != protocol.StatusOK {
		t.Fatalf("request after Drain = %q, want ok", got)
	}
}

// Watches at the limit are refused with rate-limited; requests still pass.
func TestRuntimeWatchLimits(t *testing.T) {
	hub := changefeed.New("w", 0)
	runtime := newTestRuntime(t, &Config{Changes: hub, MaxWatches: 2, MaxWatchesPerConn: 1, RequestTimeout: time.Second})
	t.Cleanup(func() {
		if err := runtime.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	first := quicserve.WithConnState(context.Background())
	if ack := readBlock(t, mustWatch(first, t, runtime)); ack.Status != protocol.StatusOK {
		t.Fatalf("first watch: %q", ack.Status)
	}
	// Same connection: the per connection cap.
	if got := serveStatusIn(first, t, runtime, "WATCH /\n"); got != protocol.StatusRateLimited {
		t.Fatalf("second watch on a connection with a cap of 1 = %q, want rate-limited", got)
	}
	second := quicserve.WithConnState(context.Background())
	if ack := readBlock(t, mustWatch(second, t, runtime)); ack.Status != protocol.StatusOK {
		t.Fatalf("watch on another connection: %q", ack.Status)
	}
	// World cap of 2 reached.
	third := quicserve.WithConnState(context.Background())
	if got := serveStatusIn(third, t, runtime, "WATCH /\n"); got != protocol.StatusRateLimited {
		t.Fatalf("third watch over the world cap = %q, want rate-limited", got)
	}
	if got := serveStatusIn(third, t, runtime, "FETCH /health\n"); got != protocol.StatusOK {
		t.Fatalf("request while watches are capped = %q, want ok", got)
	}
	if runtime.Watches() != 2 {
		t.Fatalf("watches = %d, want 2", runtime.Watches())
	}
}

// A subscribed watch holds no concurrency slot: with one slot, a watch and a
// request are served together.
func TestRuntimeWatchReleasesConcurrencySlot(t *testing.T) {
	hub := changefeed.New("w", 0)
	runtime := newTestRuntime(t, &Config{Changes: hub, MaxConcurrent: 1, RequestTimeout: 100 * time.Millisecond})
	t.Cleanup(func() {
		if err := runtime.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	ctx := quicserve.WithConnState(context.Background())
	if ack := readBlock(t, mustWatch(ctx, t, runtime)); ack.Status != protocol.StatusOK {
		t.Fatalf("watch: %q", ack.Status)
	}
	if got := serveStatus(t, runtime, "FETCH /health\n"); got != protocol.StatusOK {
		t.Fatalf("request beside an open watch = %q, want ok", got)
	}
}

func mustWatch(ctx context.Context, t *testing.T, runtime *Runtime) *protocol.WatchReader {
	t.Helper()
	reader, _ := serveWatch(ctx, t, runtime, "WATCH /\n")
	return reader
}

func serveStatusIn(ctx context.Context, t *testing.T, runtime *Runtime, request string) string {
	t.Helper()
	stream := newTestStream(request)
	runtime.ServeStream(ctx, testAddr("127.0.0.1:1234"), stream)
	response, err := protocol.ParseResponse(&stream.output)
	if err != nil {
		t.Fatalf("parse response: %v", err)
	}
	return response.Status
}
