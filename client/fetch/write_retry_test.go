package fetch

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/protocol"
)

// A write whose response never arrives may have landed. Resending it makes the
// client conflict with its own first attempt, so writes go out once; reads retry.
func TestWritesAreNotResentAfterTheRequestIsSent(t *testing.T) {
	var writes, reads atomic.Int32
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	addr := startTestServer(t, func(req protocol.Request) protocol.Response {
		if req.Verb == protocol.VerbFetch {
			reads.Add(1)
		} else {
			writes.Add(1)
		}
		<-release // the response is lost
		return protocol.Response{Status: protocol.StatusOK}
	})
	c := NewClient(Options{Insecure: true, RequestTimeout: 150 * time.Millisecond})
	t.Cleanup(c.Close)

	for _, tt := range writeVerbs(c, addr) {
		t.Run(tt.name, func(t *testing.T) {
			before := writes.Load()
			err := tt.call(t.Context())
			if !errors.Is(err, protocol.ErrOutcomeUnknown) {
				t.Fatalf("error = %v, want ErrOutcomeUnknown", err)
			}
			if sent := writes.Load() - before; sent != 1 {
				t.Fatalf("server saw %d copies of the write, want 1", sent)
			}
		})
	}

	if _, err := c.Fetch(t.Context(), FetchRequest{Host: addr, Path: "/a.md"}); err == nil {
		t.Fatal("fetch against a silent server returned no error")
	}
	if got := reads.Load(); got < 2 {
		t.Fatalf("server saw %d fetch attempts, want reads to keep retrying", got)
	}
}

// writeVerbs is every non idempotent verb, each taking the caller's context.
func writeVerbs(c *Client, addr string) []struct {
	name string
	call func(context.Context) error
} {
	write := WriteRequest{Host: addr, Path: "/a.md", Body: "body", ExpectedVersion: 1}
	return []struct {
		name string
		call func(context.Context) error
	}{
		{"publish", func(ctx context.Context) error { _, err := c.Publish(ctx, write); return err }},
		{"append", func(ctx context.Context) error { _, err := c.Append(ctx, write); return err }},
		{"archive", func(ctx context.Context) error {
			_, err := c.Archive(ctx, ArchiveRequest{Host: addr, Path: "/a.md"})
			return err
		}},
	}
}

// Caller cancellation after the request went out must still read as outcome
// unknown; a plain context error would invite a resend of a landed write.
func TestCancelAfterSendStaysOutcomeUnknown(t *testing.T) {
	received := make(chan struct{}, 1)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	addr := startTestServer(t, func(protocol.Request) protocol.Response {
		received <- struct{}{}
		<-release
		return protocol.Response{Status: protocol.StatusOK}
	})
	c := NewClient(Options{Insecure: true, RequestTimeout: 5 * time.Second})
	t.Cleanup(c.Close)

	for _, tt := range writeVerbs(c, addr) {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			go func() {
				<-received
				cancel()
			}()
			err := tt.call(ctx)
			if !errors.Is(err, protocol.ErrOutcomeUnknown) {
				t.Fatalf("error = %v, want ErrOutcomeUnknown", err)
			}
			if !errors.Is(err, context.Canceled) {
				t.Errorf("error = %v, want it to still unwrap to context.Canceled", err)
			}
		})
	}
}

// A context that is already done sends nothing, for every verb: a write that
// never left is a definite failure, not an unknown outcome.
func TestDoneContextSendsNothing(t *testing.T) {
	var requests atomic.Int32
	addr := startTestServer(t, func(protocol.Request) protocol.Response {
		requests.Add(1)
		return protocol.Response{Status: protocol.StatusOK}
	})
	c := NewClient(Options{Insecure: true})
	t.Cleanup(c.Close)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	calls := writeVerbs(c, addr)
	calls = append(calls, []struct {
		name string
		call func(context.Context) error
	}{
		{"fetch", func(ctx context.Context) error {
			_, err := c.Fetch(ctx, FetchRequest{Host: addr, Path: "/a.md"})
			return err
		}},
		{"list", func(ctx context.Context) error { _, err := c.List(ctx, ListRequest{Host: addr, Path: "/"}); return err }},
		{"versions", func(ctx context.Context) error {
			_, err := c.Versions(ctx, VersionsRequest{Host: addr, Path: "/a.md"})
			return err
		}},
		{"lookup", func(ctx context.Context) error {
			_, err := c.Lookup(ctx, LookupRequest{Host: addr, Scope: "/", Query: "q"})
			return err
		}},
	}...)
	for _, tt := range calls {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.call(ctx)
			if !errors.Is(err, context.Canceled) || errors.Is(err, protocol.ErrOutcomeUnknown) {
				t.Fatalf("error = %v, want a plain context.Canceled", err)
			}
		})
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("server saw %d requests, want none", got)
	}
}
