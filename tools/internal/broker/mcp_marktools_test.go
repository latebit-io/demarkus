package broker

import (
	"context"
	"errors"
	"testing"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/fetchtest"
)

// The gateway's bindings of the shared tool bodies: a url names a world, and a
// failed call is worded for an agent behind the broker.
func TestToolBodiesBinding(t *testing.T) {
	var failWith error
	d := &fakeDispatcher{VersionsFn: func(context.Context, fetch.VersionsRequest) (fetch.Result, error) {
		return fetch.Result{}, failWith
	}}
	g := newGatewayWithDispatcher(t, mcpTestConfig(), d)
	tests := []struct {
		name, url string
		err       error
		want      string
	}{
		{"unknown world", "mark://team-z/foo.md", &errWorldNotFound{worldName: "team-z"}, `broker: unknown world "team-z" (not in broker config)`},
		{"refused identity", "mark://team-a/foo.md", ErrNotAuthorized, `not authorized for world "team-a"`},
		{"unverified email", "mark://team-a/foo.md", ErrEmailUnverified, "identity email is not verified"},
		{"transport", "mark://team-a/foo.md", errors.New("boom"), "versions failed: boom"},
		{"a port is refused", "mark://team-a:7000/foo.md", nil,
			`invalid URL: invalid URL "mark://team-a:7000/foo.md": world host must not carry a port (the broker resolves world names internally)`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			failWith = tt.err
			res, err := g.handleMarkVersions(withAliceClaims(context.Background()), callToolReq("mark_versions", map[string]any{"url": tt.url}))
			if err != nil || !res.IsError {
				t.Fatalf("handleMarkVersions = (%+v, %v), want a tool error", res, err)
			}
			if got := toolResultText(t, res); got != tt.want {
				t.Errorf("text = %q\nwant   %q", got, tt.want)
			}
		})
	}
	if got := d.VersionsCalls[0]; got.Host != "team-z" || got.Path != "/foo.md" || got.Token != "" {
		t.Errorf("request = %+v, want the world as host and no token", got)
	}
}

// A write whose response was lost is looked for, never resent, and the agent
// the landed version carries is the caller's verified email.
func TestLostWriteResponseIsReconciled(t *testing.T) {
	d := &fakeDispatcher{
		AppendFn: func(context.Context, fetch.WriteRequest) (fetch.Result, error) {
			return fetch.Result{}, fetchtest.LostResponse()
		},
		FetchFn: fetchtest.History("/foo.md", map[string]string{"agent": "alice@example.com"}, "a", "b", "old", "old\nmore"),
	}
	g := newGatewayWithDispatcher(t, mcpTestConfig(), d)
	res, err := g.handleMarkAppend(withAliceClaims(context.Background()), callToolReq("mark_append", map[string]any{
		"url": "mark://team-a/foo.md", "body": "more", "expected_version": float64(3),
	}))
	if err != nil || res.IsError {
		t.Fatalf("handleMarkAppend = (%+v, %v)", res, err)
	}
	if got := toolResultText(t, res); got != "status: ok\nversion: 4\n" {
		t.Errorf("text = %q, want the landed write reported", got)
	}
	if len(d.AppendCalls) != 1 {
		t.Errorf("appends sent = %d, want exactly one", len(d.AppendCalls))
	}
}

// An explored document is observed under its world's real address, the same
// source a crawl and a seeded snapshot record, so the three never disagree.
func TestExploreObservesUnderTheWorldsAddress(t *testing.T) {
	d := &fakeDispatcher{FetchFn: func(context.Context, fetch.FetchRequest) (fetch.Result, error) {
		return fetchtest.Head("# Doc\n", 2, nil), nil
	}}
	g := newGatewayWithDispatcher(t, mcpTestConfig(), d)
	res, err := g.handleMarkExplore(withAliceClaims(context.Background()), callToolReq("mark_explore", map[string]any{"url": "mark://team-a/docs/a.md"}))
	if err != nil || res.IsError {
		t.Fatalf("handleMarkExplore = (%+v, %v)", res, err)
	}
	node := g.knowledgeGraph.graphStore.GetNode("mark://team-a/docs/a.md")
	if node == nil || node.Observation.Source != "mark://team-a.team-a.svc.cluster.local/docs/a.md" {
		t.Errorf("node = %+v, want it sourced at the world's address", node)
	}
}

// Host case is not identity (ADR 0018): a world is reached however its name is
// typed, and every world name is lowercase (config load refuses anything else).
func TestToolURLWorldNamesAreCaseInsensitive(t *testing.T) {
	world, path, err := parseToolURL("mark://Team-A/Docs/A.md")
	if err != nil || world != "team-a" || path != "/Docs/A.md" {
		t.Errorf("parseToolURL = %q, %q, %v; want the world lowercased and the path as written", world, path, err)
	}
}
