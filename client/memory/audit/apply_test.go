package audit

import (
	"context"
	"testing"

	"github.com/latebit-io/demarkus/client/docwrite"
	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/fetchtest"
	"github.com/latebit-io/demarkus/protocol"
)

// A create that finds the path taken is conflicted, the parent that would
// move bullets into it is skipped, and an independent write still lands
// under the surface's identity.
func TestApplySkipsWhatARequiredWriteDidNotLand(t *testing.T) {
	backend := &fetchtest.Client{
		Published: map[string]fetch.Result{
			"h/h/index.md": fetchtest.Head("# Taken\n", 1, nil),
			"h/index.md":   fetchtest.Head("# Hub\n", 4, nil),
			"h/x.md":       fetchtest.Head("# X\n", 2, nil),
		},
		PublishFn: func(_ context.Context, r fetch.WriteRequest) (fetch.Result, error) {
			return fetchtest.Created(r.ExpectedVersion + 1), nil
		},
	}
	docs := func(p string) *docwrite.Doc {
		return &docwrite.Doc{Backend: backend, Host: "h", Path: p, Write: docwrite.SendOnce("w")}
	}
	plan := &Plan{Writes: []PlanWrite{
		{Path: "/h/index.md", Body: "# H\n", Metadata: map[string]string{"tags": "index"}},
		{Path: "/index.md", Version: 4, Body: "# Hub\n\n- [H](/h/)\n", Requires: []string{"/h/index.md"}},
		{Path: "/x.md", Version: 2, Body: "# X\n\nLinked.\n", Metadata: map[string]string{"tags": "x"}},
	}}
	got := Apply(context.Background(), docs, plan, map[string]string{"agent": "a"})
	want := []Outcome{
		{Path: "/h/index.md", Result: Conflicted, Detail: "exists now; rerun the proposal"},
		{Path: "/index.md", Result: Skipped, Detail: "needs /h/index.md, which did not land"},
		{Path: "/x.md", Result: Applied, Version: 3},
	}
	for i := range want {
		if got[i].Path != want[i].Path || got[i].Result != want[i].Result || got[i].Detail != want[i].Detail || got[i].Version != want[i].Version {
			t.Errorf("outcome %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if len(backend.PublishCalls) != 2 || backend.PublishCall("/x.md").Metadata["agent"] != "a" {
		t.Errorf("publishes = %+v", backend.PublishCalls)
	}
}

// A refused write's outcome carries the server's reason, not the status alone.
func TestApplyReportsTheRefusalReason(t *testing.T) {
	backend := &fetchtest.Client{
		Published: map[string]fetch.Result{"h/x.md": fetchtest.Head("# X\n", 2, nil)},
		PublishFn: func(_ context.Context, _ fetch.WriteRequest) (fetch.Result, error) {
			return fetch.Result{Response: protocol.Response{Status: protocol.StatusBadRequest, Body: "\n# Bad Request\n\nmissing required tag axis category\n"}}, nil
		},
	}
	docs := func(p string) *docwrite.Doc {
		return &docwrite.Doc{Backend: backend, Host: "h", Path: p, Write: docwrite.SendOnce("w")}
	}
	plan := &Plan{Writes: []PlanWrite{{Path: "/x.md", Version: 2, Body: "# X\n\nLinked.\n"}}}
	got := Apply(context.Background(), docs, plan, nil)
	if len(got) != 1 || got[0].Result != Refused || got[0].Detail != "bad-request: missing required tag axis category" {
		t.Errorf("outcomes = %+v", got)
	}
}

func TestApplyStampsAWriteWithoutMetadata(t *testing.T) {
	backend := &fetchtest.Client{
		Published: map[string]fetch.Result{"h/x.md": fetchtest.Head("# X\n", 2, nil)},
		PublishFn: func(_ context.Context, r fetch.WriteRequest) (fetch.Result, error) {
			return fetchtest.Created(r.ExpectedVersion + 1), nil
		},
	}
	docs := func(p string) *docwrite.Doc {
		return &docwrite.Doc{Backend: backend, Host: "h", Path: p, Write: docwrite.SendOnce("w")}
	}
	plan := &Plan{Writes: []PlanWrite{{Path: "/x.md", Version: 2, Body: "# X\n\nLinked.\n"}}}
	got := Apply(context.Background(), docs, plan, map[string]string{"agent": "a"})
	if got[0].Result != Applied || backend.PublishCall("/x.md").Metadata["agent"] != "a" {
		t.Errorf("outcome = %+v, publishes = %+v", got[0], backend.PublishCalls)
	}
}
