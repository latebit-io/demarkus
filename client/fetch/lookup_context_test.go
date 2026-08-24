package fetch

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/protocol"
)

func TestLookupContextCancelsBlockedResponse(t *testing.T) {
	received := make(chan struct{})
	release := make(chan struct{})
	host := startTestServer(t, func(protocol.Request) protocol.Response {
		close(received)
		<-release
		return protocol.Response{Status: protocol.StatusOK}
	})
	c := NewClient(Options{Insecure: true, RequestTimeout: 10 * time.Second})
	defer c.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := c.LookupContext(ctx, host, "/", "auth", "", LookupOptions{})
		done <- err
	}()
	<-received
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("LookupContext error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("LookupContext did not stop after cancellation")
	}
	close(release)
}
