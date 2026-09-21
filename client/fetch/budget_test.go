package fetch

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/protocol"
)

func TestResponseBudgetConcurrentReads(t *testing.T) {
	ctx, budget := WithResponseBudget(t.Context(), 1000)
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			_, err := io.Copy(io.Discard, responseReader(ctx, strings.NewReader(strings.Repeat("x", 200))))
			if err != nil && !errors.Is(err, ErrResponseBudget) {
				t.Errorf("read: %v", err)
			}
		})
	}
	wg.Wait()
	if budget.BytesRead() > 1000 || budget.BytesRead() == 0 {
		t.Fatalf("read bytes = %d", budget.BytesRead())
	}
}

func TestFetchContextResponseBudget(t *testing.T) {
	host := startTestServer(t, func(protocol.Request) protocol.Response {
		return protocol.Response{Status: "ok", Body: strings.Repeat("x", 10000)}
	})
	c := NewClient(Options{Insecure: true})
	defer c.Close()
	ctx, budget := WithResponseBudget(t.Context(), 100)
	_, err := c.Fetch(ctx, FetchRequest{Host: host, Path: "/doc.md"})
	if !errors.Is(err, ErrResponseBudget) || budget.BytesRead() != 100 || isTransientError(err) {
		t.Fatalf("budget read = %d, err = %v", budget.BytesRead(), err)
	}
}

func TestFetchContextCancelsBlockedResponse(t *testing.T) {
	received := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	host := startTestServer(t, func(protocol.Request) protocol.Response {
		close(received)
		<-release
		return protocol.Response{Status: "ok"}
	})
	c := NewClient(Options{Insecure: true, RequestTimeout: 10 * time.Second})
	defer c.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := c.Fetch(ctx, FetchRequest{Host: host, Path: "/doc.md"})
		done <- err
	}()
	select {
	case <-received:
	case <-time.After(time.Second):
		t.Fatal("request not received")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("fetch: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("network fetch did not cancel")
	}
}
