package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/graphstore"
	"github.com/latebit-io/demarkus/protocol"
	"github.com/mark3labs/mcp-go/mcp"
)

func TestMarkGraphPartialResults(t *testing.T) {
	for _, mode := range []string{"pre-cancelled", "mid-fetch", "failed-peer"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			started := make(chan struct{})
			sc := &stubClient{FetchCtxFn: func(ctx context.Context, _, path, _ string) (fetch.Result, error) {
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
			h := &handler{client: sc, graphStore: graphstore.New()}
			if mode == "pre-cancelled" {
				cancel()
			}
			done := make(chan struct{})
			var res *mcp.CallToolResult
			var err error
			go func() {
				res, err = h.markGraph(ctx, newCallToolRequest(map[string]any{"url": "mark://host/root.md"}))
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
			text := res.Content[0].(mcp.TextContent).Text
			reason := "caller-cancelled"
			if mode == "failed-peer" {
				reason = "fetch-failure"
			}
			for _, want := range []string{"scope: neighborhood", "outcome: partial", reason} {
				if !strings.Contains(text, want) {
					t.Errorf("missing %q: %s", want, text)
				}
			}
			if mode != "pre-cancelled" && len(h.graphStore.Backlinks("mark://host/child.md")) != 1 {
				t.Fatal("valid edge not persisted")
			}
		})
	}
}
