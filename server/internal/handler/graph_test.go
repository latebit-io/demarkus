package handler

import (
	"context"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/server/internal/auth"
)

func TestGraphDataWrites(t *testing.T) { forEachBackend(t, testGraphDataWrites) }

// An identity's grant, a governor's included, never writes generated graph
// data; the deriver's grant does. A token is unchanged.
func testGraphDataWrites(t *testing.T, newBackend backendFactory) {
	ts := auth.NewTokenStore(map[string]auth.Token{
		protocol.HashToken("writer"): {Label: "writer", Paths: []string{"/**"}, Operations: []string{"publish"}},
	})
	plain := protocol.WithGrant(context.Background(), protocol.Grant{Label: "alice", Paths: []string{"/**"}, Govern: true})
	derives := protocol.WithGrant(context.Background(), protocol.Grant{Label: "federation", Paths: []string{"/graph/worlds/**", "/graph.md"}, Graph: true})
	publish := func(path string) string { return "PUBLISH " + path + "\n\n# Mine\n" }
	tests := []struct {
		name, request string
		ctx           context.Context
		want          string
	}{
		{"identity publish", publish("/graph.md"), plain, protocol.StatusNotPermitted},
		{"identity publish, other spelling", publish("//Graph/Worlds/team-a/manifest.md"), plain, protocol.StatusNotPermitted},
		{"identity archive", "ARCHIVE /graph.md\n", plain, protocol.StatusNotPermitted},
		{"identity beside the graph data", publish("/graph/notes.md"), plain, protocol.StatusCreated},
		{"deriver", publish("/graph/worlds/team-a/manifest.md"), derives, protocol.StatusCreated},
		{"token", "PUBLISH /graph.md\n---\nauth: writer\n---\n# Graph\n", context.Background(), protocol.StatusCreated},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHandler(newBackend(t), ts)
			stream := newMockStream(tt.request)
			h.HandleStream(tt.ctx, stream)
			resp, err := protocol.ParseResponse(&stream.output)
			if err != nil {
				t.Fatalf("parse response: %v", err)
			}
			if resp.Status != tt.want {
				t.Fatalf("status = %q, want %q (body %q)", resp.Status, tt.want, resp.Body)
			}
			if tt.want == protocol.StatusNotPermitted && !strings.Contains(resp.Body, "generated graph data") {
				t.Errorf("refusal does not name graph data: %q", resp.Body)
			}
		})
	}
}
