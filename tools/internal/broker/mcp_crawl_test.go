package broker

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/protocol"
	"github.com/mark3labs/mcp-go/mcp"
)

func TestBrokerCrawlPartialResults(t *testing.T) {
	for _, mode := range []string{"pre-cancelled", "mid-fetch", "failed-peer"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(withAliceClaims(t.Context()))
			defer cancel()
			started := make(chan struct{})
			d := &fakeDispatcher{FetchFn: func(ctx context.Context, r fetch.FetchRequest) (fetch.Result, error) {
				path := r.Path
				if mode == "pre-cancelled" {
					t.Error("pre-cancelled crawl fetched a document")
				}
				if path == "/root.md" {
					return fetch.Result{Response: protocol.Response{Status: "ok", Body: "[child](/child.md)"}}, nil
				}
				if mode == "mid-fetch" {
					close(started)
					<-ctx.Done()
					return fetch.Result{}, ctx.Err()
				}
				return fetch.Result{}, errors.New("peer disconnected")
			}}
			g := newGatewayWithDispatcher(t, mcpTestConfig(), d)
			if mode == "pre-cancelled" {
				cancel()
			}
			done := make(chan struct{})
			var res *mcp.CallToolResult
			var err error
			go func() {
				res, err = g.handleMarkGraph(ctx, callToolReq("mark_graph", map[string]any{"url": "mark://team-a/root.md"}))
				close(done)
			}()
			if mode == "mid-fetch" {
				select {
				case <-started:
					cancel()
				case <-time.After(time.Second):
					t.Fatal("child fetch did not start")
				}
			}
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("handler did not finish")
			}
			if err != nil || res.IsError {
				t.Fatalf("partial graph discarded: %v, %+v", err, res)
			}
			text := toolResultText(t, res)
			reason := "caller-cancelled"
			if mode == "failed-peer" {
				reason = "fetch-failure"
			}
			for _, want := range []string{"scope: neighborhood", "outcome: partial", reason} {
				if !strings.Contains(text, want) {
					t.Errorf("missing %q: %s", want, text)
				}
			}
			if mode != "pre-cancelled" && !strings.Contains(text, "mark://team-a/root.md -> mark://team-a/child.md") {
				t.Fatal("valid edge not surfaced")
			}
		})
	}
}
