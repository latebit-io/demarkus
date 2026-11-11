package handler

import (
	"context"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/server/internal/auth"
)

// storedIdentity writes request under ctx and returns the agent and user the
// document then carries.
func storedIdentity(ctx context.Context, t *testing.T, h *Handler, request string) (agent, user string) {
	t.Helper()
	stream := newMockStream(request)
	h.HandleStream(ctx, stream)
	resp, err := protocol.ParseResponse(&stream.output)
	if err != nil || !protocol.IsWriteSuccess(resp.Status) {
		t.Fatalf("write %q = %v, %v", strings.SplitN(request, "\n", 2)[0], resp.Status, err)
	}
	path := strings.Fields(request)[1]
	fetched := newMockStream("FETCH " + path + "\n")
	h.HandleStream(context.Background(), fetched)
	doc, err := protocol.ParseResponse(&fetched.output)
	if err != nil || doc.Status != protocol.StatusOK {
		t.Fatalf("fetch %s = %v, %v", path, doc.Status, err)
	}
	return doc.Metadata[protocol.MetaAgent], doc.Metadata[protocol.MetaUser]
}

// The stored user is the grant's, never the request's: a caller verified as
// one person cannot sign a write as another, and a token names nobody.
func TestStoredUserIsTheGrants(t *testing.T) {
	ts := auth.NewTokenStore(map[string]auth.Token{
		protocol.HashToken("writer"): {Label: "writer", Paths: []string{"/**"}, Operations: []string{"publish"}},
	})
	h := newHandler(fileBackend(t), ts)
	alice := protocol.WithGrant(context.Background(), protocol.Grant{Label: "alice@example.com", User: "alice@example.com", Paths: []string{"/**"}})
	service := protocol.WithGrant(context.Background(), protocol.Grant{Label: "federation", Paths: []string{"/**"}})
	publish := func(path, meta string) string { return "PUBLISH " + path + "\n---\n" + meta + "---\n# Doc\n" }

	if agent, user := storedIdentity(alice, t, h, publish("/forged.md", "user: bob@example.com\nagent: cli\n")); user != "alice@example.com" || agent != "cli" {
		t.Errorf("grant write claiming bob: agent %q, user %q; want cli, alice", agent, user)
	}
	if _, user := storedIdentity(alice, t, h, publish("/plain.md", "tags: a\n")); user != "alice@example.com" {
		t.Errorf("grant write with no user: user %q, want alice", user)
	}
	if _, user := storedIdentity(service, t, h, publish("/service.md", "user: bob@example.com\n")); user != "" {
		t.Errorf("service grant write claiming bob: user %q, want none", user)
	}
	if agent, user := storedIdentity(context.Background(), t, h, publish("/token.md", "auth: writer\nuser: bob@example.com\nagent: demarkus-mcp\n")); user != "" || agent != "demarkus-mcp" {
		t.Errorf("token write claiming bob: agent %q, user %q; want demarkus-mcp, none", agent, user)
	}
	// An append is its own write: it does not inherit the base's author.
	if agent, user := storedIdentity(context.Background(), t, h, "APPEND /forged.md\n---\nauth: writer\nexpected-version: 1\n---\nmore\n"); user != "" || agent != "" {
		t.Errorf("token append over alice's version: agent %q, user %q; want neither", agent, user)
	}
	if _, user := storedIdentity(alice, t, h, "APPEND /token.md\n---\nexpected-version: 1\nuser: bob@example.com\n---\nmore\n"); user != "alice@example.com" {
		t.Errorf("grant append claiming bob: user %q, want alice", user)
	}
}
