package marktools_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/fetchtest"
	"github.com/latebit-io/demarkus/client/graphstore"
	"github.com/latebit-io/demarkus/client/marktools"
	"github.com/latebit-io/demarkus/protocol"
)

// mvGraphPublisher is a stdio server with an empty graph store and a token;
// the backend records what it publishes.
func mvGraphPublisher(t *testing.T) (*marktools.Tools, *fetchtest.Client) {
	t.Helper()
	gs, err := graphstore.Load(filepath.Join(t.TempDir(), "graph.json"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	backend := &fetchtest.Client{
		PublishFn: func(context.Context, fetch.WriteRequest) (fetch.Result, error) {
			return fetch.Result{Response: protocol.Response{
				Status:   "created",
				Metadata: map[string]string{"version": "1", "modified": "2026-03-08T12:00:00Z"},
			}}, nil
		},
	}
	return (&clientSurface{DefaultHost: "mark://target.com", Token: "test-token", Store: gs}).tools(t, backend), backend
}

// The default of 20 is the surface's argument parsing and is pinned there.
func TestGraphPublishRetention(t *testing.T) {
	tests := []struct {
		name          string
		retention     int
		wantRetention string
	}{
		{name: "explicit override", retention: 5, wantRetention: "5"},
		{name: "zero disables retention", retention: 0, wantRetention: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tools, backend := mvGraphPublisher(t)
			got := tools.GraphPublish(t.Context(), marktools.GraphPublishArgs{
				URL: "mark://target.com/graph.md", ExpectedVersion: new(0), Retention: tt.retention,
			})
			if got.IsError {
				t.Fatalf("unexpected tool error: %v", got.Text)
			}
			if len(backend.PublishCalls) != 1 {
				t.Fatalf("publishes = %d, want 1", len(backend.PublishCalls))
			}
			if got := backend.PublishCalls[0].Metadata["retention"]; got != tt.wantRetention {
				t.Errorf("retention meta = %q, want %q", got, tt.wantRetention)
			}
		})
	}

	t.Run("negative rejected", func(t *testing.T) {
		tools, _ := mvGraphPublisher(t)
		got := tools.GraphPublish(t.Context(), marktools.GraphPublishArgs{
			URL: "mark://target.com/graph.md", ExpectedVersion: new(0), Retention: -1,
		})
		mvAssertError(t, got, "retention must be >= 0")
	})
}

func TestGraphPublishNegativeVersion(t *testing.T) {
	tools, _ := mvGraphPublisher(t)
	got := tools.GraphPublish(t.Context(), marktools.GraphPublishArgs{
		URL: "mark://target.com/graph.md", ExpectedVersion: new(-1), Retention: 20,
	})
	mvAssertError(t, got, "expected_version must be >= 0")
}
