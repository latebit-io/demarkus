package knowledgeserver

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/publishpolicy"
	"github.com/latebit-io/demarkus/server/internal/knowledgeconfig"
)

// localProfile is the soul server's own profile on an ephemeral port: the
// constructor the thin main calls, defaults left to SetDefaults.
func localProfile(dir, tokensFile string) *knowledgeconfig.Config {
	return knowledgeconfig.LocalProfile(&knowledgeconfig.LocalOptions{
		Dir: dir, TokensFile: tokensFile, ListenAddress: "127.0.0.1:0", MaxStreams: 10,
		IdleTimeout: knowledgeconfig.Duration(30 * time.Second), RequestsPerSecond: 50, Burst: 100,
	})
}

// TestOpenServesTheLocalProfile: the shared runtime opens from memory with
// no file, no certificate, no health port and no seed, answers every server
// name, and judges a soul by the empty policy at warn.
func TestOpenServesTheLocalProfile(t *testing.T) {
	dir := t.TempDir()
	tokens := writeTokens(t, dir, "soul")
	server, err := Open(Options{Config: localProfile(filepath.Join(dir, "soul"), tokens), Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatalf("Open the local profile: %v", err)
	}
	defer server.Close()
	if server.health != nil {
		t.Error("the local profile opened a health listener")
	}
	for _, authority := range []string{"", "localhost", "127.0.0.1", "soul.example.com"} {
		if !server.Routes(authority) {
			t.Errorf("Routes(%q) = false; want every server name", authority)
		}
	}
	untagged := protocol.Request{Verb: protocol.VerbPublish, Path: "/notes/a.md", Body: "# A\n", Metadata: map[string]string{"expected-version": "0"}}
	resp := exchangeGranted(t, server, "", untagged)
	if resp.Status != protocol.StatusCreated || resp.Metadata["warning"] != "publish policy (warn): missing-tags" {
		t.Errorf("untagged publish on a soul: status %s, warning %q; want created with the empty policy's warning", resp.Status, resp.Metadata["warning"])
	}
	if resp := exchangeGranted(t, server, "", protocol.Request{Verb: protocol.VerbFetch, Path: publishpolicy.DocumentPath}); resp.Status != protocol.StatusNotFound {
		t.Errorf("policy document on an unseeded world: status %s; want not-found", resp.Status)
	}
	server.Reload() // a static certificate and the tokens file; neither may fail

	// Over the wire, by IP, with no server name: what the CLI does.
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- server.Serve(ctx) }()
	conn := dialBearer(t, server.listeners[0].server.Addr().String(), "") // no server name: a dial by IP
	if resp := exchange(t, conn, protocol.Request{Verb: protocol.VerbFetch, Path: "/health"}); resp.Status != protocol.StatusOK {
		t.Errorf("FETCH /health by IP: status %s, want ok", resp.Status)
	}
	cancel()
	if err := <-served; err != nil {
		t.Errorf("Serve: %v", err)
	}
}
