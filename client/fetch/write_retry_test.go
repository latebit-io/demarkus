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

	tests := []struct {
		name string
		call func() error
	}{
		{"publish", func() error { _, err := c.Publish(addr, "/a.md", "body", "", 0, nil); return err }},
		{"append", func() error { _, err := c.Append(addr, "/a.md", "more", "", 1, nil); return err }},
		{"archive", func() error { _, err := c.Archive(addr, "/a.md", ""); return err }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := writes.Load()
			err := tt.call()
			if !errors.Is(err, ErrOutcomeUnknown) {
				t.Fatalf("error = %v, want ErrOutcomeUnknown", err)
			}
			if sent := writes.Load() - before; sent != 1 {
				t.Fatalf("server saw %d copies of the write, want 1", sent)
			}
		})
	}

	if _, err := c.Fetch(addr, "/a.md", ""); err == nil {
		t.Fatal("fetch against a silent server returned no error")
	}
	if got := reads.Load(); got < 2 {
		t.Fatalf("server saw %d fetch attempts, want reads to keep retrying", got)
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

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-received
		cancel()
	}()
	_, err := c.PublishContext(ctx, addr, "/a.md", "body", "", 0, nil)
	if !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("error = %v, want ErrOutcomeUnknown", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want it to still unwrap to context.Canceled", err)
	}
}
