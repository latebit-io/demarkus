package handler

import (
	"context"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/server/internal/auth"
)

func TestGovernanceWrites(t *testing.T) { forEachBackend(t, testGovernanceWrites) }

// A restricting world writes governance documents only with govern: a
// publish token or a plain grant is refused under every path spelling.
func testGovernanceWrites(t *testing.T, newBackend backendFactory) {
	ts := auth.NewTokenStore(map[string]auth.Token{
		protocol.HashToken("writer"):   {Label: "writer", Paths: []string{"/**"}, Operations: []string{"publish"}},
		protocol.HashToken("governor"): {Label: "governor", Paths: []string{"/.well-known/demarkus/**"}, Operations: []string{protocol.OpGovern}},
	})
	plain := protocol.WithGrant(context.Background(), protocol.Grant{Label: "alice", Paths: []string{"/**"}})
	governs := protocol.WithGrant(context.Background(), protocol.Grant{Label: "broker", Paths: []string{"/**"}, Govern: true})
	publish := func(token, path string) string {
		return "PUBLISH " + path + "\n---\nauth: " + token + "\n---\n# Write Policy\n\nstrictness: warn\n"
	}
	tests := []struct {
		name, request string
		ctx           context.Context
		restrict      bool
		want          string
	}{
		{"publish token refused", publish("writer", "/.well-known/demarkus/policy.md"), context.Background(), true, protocol.StatusNotPermitted},
		{"doubled slash refused", publish("writer", "/.well-known//demarkus/policy.md"), context.Background(), true, protocol.StatusNotPermitted},
		{"dot segment refused", publish("writer", "/./.well-known/demarkus/template.md"), context.Background(), true, protocol.StatusNotPermitted},
		{"case variant refused", publish("writer", "/.well-known/Demarkus/policy.md"), context.Background(), true, protocol.StatusNotPermitted},
		{"archive refused", "ARCHIVE /.well-known/demarkus/style.md\n---\nauth: writer\n---\n", context.Background(), true, protocol.StatusNotPermitted},
		{"plain grant refused", "PUBLISH /.well-known/demarkus/policy.md\n\n# P\n", plain, true, protocol.StatusNotPermitted},
		{"govern token writes", publish("governor", "/.well-known/demarkus/policy.md"), context.Background(), true, protocol.StatusCreated},
		{"govern grant writes", "PUBLISH /.well-known/demarkus/world.md\n\n# W\n", governs, true, protocol.StatusCreated},
		{"govern does not imply publish", publish("governor", "/notes/a.md"), context.Background(), true, protocol.StatusNotPermitted},
		{"publish token elsewhere", publish("writer", "/notes/a.md"), context.Background(), true, protocol.StatusCreated},
		{"open world takes publish", publish("writer", "/.well-known/demarkus/policy.md"), context.Background(), false, protocol.StatusCreated},
		{"open world takes govern", publish("governor", "/.well-known/demarkus/policy.md"), context.Background(), false, protocol.StatusCreated},
		{"open world govern stays scoped", publish("governor", "/notes/a.md"), context.Background(), false, protocol.StatusNotPermitted},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := mustNew(Config{Store: newBackend(t).Store, Logger: discardLogger, GetTokenStore: func() *auth.TokenStore { return ts }, RestrictGovernance: tt.restrict})
			stream := newMockStream(tt.request)
			h.HandleStream(tt.ctx, stream)
			resp, err := protocol.ParseResponse(&stream.output)
			if err != nil {
				t.Fatalf("parse response: %v", err)
			}
			if resp.Status != tt.want {
				t.Fatalf("status = %q, want %q (body %q)", resp.Status, tt.want, resp.Body)
			}
			if tt.want == protocol.StatusNotPermitted && tt.restrict && strings.Contains(tt.request, "well-known") && !strings.Contains(resp.Body, "requires the govern operation") {
				t.Errorf("refusal does not name the govern operation: %q", resp.Body)
			}
		})
	}
}
