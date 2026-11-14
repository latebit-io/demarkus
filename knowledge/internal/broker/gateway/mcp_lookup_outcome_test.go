package gateway

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/fetchtest"
	"github.com/latebit-io/demarkus/protocol/render"
)

func TestLookupOutcomesLogWholeWorldSubjectsOnly(t *testing.T) {
	d := &fakeDispatcher{LookupFn: func(_ context.Context, r fetch.LookupRequest) (fetch.Result, error) {
		var rows []render.LookupRow
		if r.Query == "auth" {
			rows = append(rows, authRow)
		}
		echo := r.Match
		if r.Query == "hairpin" {
			echo = "" // a body request answered from the catalog
		}
		return fetchtest.Lookup(r.Query, r.Scope, echo, rows...), nil
	}}
	g := newGatewayWithDispatcher(t, mcpTestConfig(), d)
	var logs bytes.Buffer
	deps := *g.deps
	deps.Log = slog.New(slog.NewJSONHandler(&logs, nil))
	g.deps = &deps

	ctx := withAliceClaims(context.Background())
	for _, args := range []map[string]any{
		{"url": "mark://team-a/", "query": "kafka retention"},
		{"url": "mark://team-a/", "query": "auth"},
		{"url": "mark://team-a/", "query": "quic", "filter": "tag=net"},
		{"url": "mark://team-a/", "query": "*"},
		{"url": "mark://team-a/adr/", "query": "grpc"},
		{"url": "mark://team-a/", "query": "hairpin", "match": "body"},
	} {
		if res, err := g.handleMarkLookup(ctx, callToolReq("mark_lookup", args)); err != nil || res.IsError {
			t.Fatalf("mark_lookup %v: err %v res %+v", args, err, res)
		}
	}
	if res, err := g.handleMarkLookupAll(ctx, callToolReq("mark_lookup_all", map[string]any{"query": "kafka retention"})); err != nil || res.IsError {
		t.Fatalf("mark_lookup_all: err %v res %+v", err, res)
	}

	got := logs.String()
	for _, want := range []string{
		`"msg":"lookup outcome","tool":"mark_lookup","world":"team-a","found":false`,
		`"msg":"lookup outcome","tool":"mark_lookup","world":"team-a","found":true`,
		`"msg":"lookup outcome","tool":"mark_lookup_all","worlds":1,"found":false`,
	} {
		if strings.Count(got, want) != 1 {
			t.Errorf("want one %s in:\n%s", want, got)
		}
	}
	if n := strings.Count(got, `"msg":"lookup outcome"`); n != 3 {
		t.Errorf("%d outcome lines, want 3: filtered, match-all, scoped and catalog-fallback lookups log none:\n%s", n, got)
	}
	if strings.Contains(got, "kafka") || strings.Contains(got, "alice") {
		t.Errorf("query text or identity logged:\n%s", got)
	}
}
