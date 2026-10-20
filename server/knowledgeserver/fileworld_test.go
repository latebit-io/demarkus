package knowledgeserver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/publishpolicy"
	"github.com/latebit-io/demarkus/server/blob"
	"github.com/latebit-io/demarkus/server/internal/knowledgeconfig"
	"github.com/latebit-io/demarkus/server/knowledgeserver/bucketworlds"
)

// capturedSoul is the upgrade capture's soul directory, as the plugin wrote it.
var capturedSoul = filepath.Join("..", "..", "testdata", "upgrade", "home", ".demarkus", "soul")

func fileWorldFragment(name, dir, tokensFile string, readOnly bool) string {
	return fmt.Sprintf(`  - name: %s
    authorities:
      - %s.example.com
    dir: %s
    readOnly: %t
    auth:
      tokensFile: %s
`, name, name, dir, readOnly, tokensFile)
}

// noBuckets is the bucket half of the factory a file-world test never reaches.
func noBuckets(_ context.Context, world *knowledgeconfig.WorldConfig) (blob.Store, error) {
	return nil, fmt.Errorf("world %s asked for a bucket", world.Name)
}

// openFileWorld opens a manager over one file world and returns the server
// front that exchanges with it in process.
func openFileWorld(t *testing.T, worldsSection string) (*Server, error) {
	t.Helper()
	h := newHarness(t.TempDir())
	if err := h.open(t, worldsSection, worldStores(bucketworlds.ForWorlds(noBuckets))); err != nil {
		return nil, err
	}
	return &Server{worlds: h.manager}, nil
}

func exchangeGranted(t *testing.T, server *Server, authority string, req protocol.Request) protocol.Response {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	granted := protocol.WithGrant(ctx, protocol.Grant{Label: "alice@example.com", Paths: []string{"/**"}})
	resp, err := server.Exchange(granted, authority, req)
	if err != nil {
		t.Fatalf("exchange %s %s: %v", req.Verb, req.Path, err)
	}
	return resp
}

// TestFileWorldServesAcrossARestart: a world on a directory takes a write,
// seeds its policy, and after the manager closes a second manager on the
// same directory reads the document at its version and extends it.
func TestFileWorldServesAcrossARestart(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "worlds", "team")
	tokens := writeTokens(t, t.TempDir(), "team")
	worlds := "worlds:\n" + fileWorldFragment("team", dir, tokens, false)
	const authority = "team.example.com"
	publish := protocol.Request{Verb: protocol.VerbPublish, Path: "/notes/a.md", Body: "# A\n", Metadata: map[string]string{"expected-version": "0", "tags": "notes"}}

	t.Run("first open", func(t *testing.T) {
		server, err := openFileWorld(t, worlds)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		if resp := exchangeGranted(t, server, authority, publish); resp.Status != protocol.StatusCreated {
			t.Fatalf("publish: status %s, body %q", resp.Status, resp.Body)
		}
		policy := exchangeGranted(t, server, authority, protocol.Request{Verb: protocol.VerbFetch, Path: publishpolicy.DocumentPath})
		if policy.Status != protocol.StatusOK {
			t.Errorf("seeded policy: status %s; want the world's policy on first open", policy.Status)
		}
	})
	if _, err := os.Stat(filepath.Join(dir, "notes", "a.md")); err != nil {
		t.Fatalf("the write is not on disk under the world's directory: %v", err)
	}

	server, err := openFileWorld(t, worlds)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	fetched := exchangeGranted(t, server, authority, protocol.Request{Verb: protocol.VerbFetch, Path: "/notes/a.md"})
	if fetched.Status != protocol.StatusOK || fetched.Metadata["version"] != "1" || !strings.Contains(fetched.Body, "# A") {
		t.Fatalf("after restart: status %s, version %q, body %q", fetched.Status, fetched.Metadata["version"], fetched.Body)
	}
	publish.Metadata["expected-version"] = "1"
	publish.Body = "# A\n\nMore.\n"
	if resp := exchangeGranted(t, server, authority, publish); resp.Status != protocol.StatusCreated || resp.Metadata["version"] != "2" {
		t.Fatalf("publish after restart: status %s, version %q, body %q", resp.Status, resp.Metadata["version"], resp.Body)
	}
}

// TestFileWorldReadsACapturedSoul: the soul server's directory opens as a
// knowledge world and serves what the soul wrote, no migration.
func TestFileWorldReadsACapturedSoul(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "soul")
	if err := os.CopyFS(dir, os.DirFS(capturedSoul)); err != nil {
		t.Fatalf("copy the captured soul: %v", err)
	}
	tokens := writeTokens(t, t.TempDir(), "soul")
	server, err := openFileWorld(t, "worlds:\n"+fileWorldFragment("soul", dir, tokens, false))
	if err != nil {
		t.Fatalf("open the captured soul as a world: %v", err)
	}
	index := exchangeGranted(t, server, "soul.example.com", protocol.Request{Verb: protocol.VerbFetch, Path: "/index.md"})
	if index.Status != protocol.StatusOK || index.Body == "" {
		t.Fatalf("the soul's index: status %s, body %q", index.Status, index.Body)
	}
	listed := exchangeGranted(t, server, "soul.example.com", protocol.Request{Verb: protocol.VerbList, Path: "/"})
	if listed.Status != protocol.StatusOK || !strings.Contains(listed.Body, "index.md") {
		t.Fatalf("the soul's root listing: status %s, body %q", listed.Status, listed.Body)
	}
}

// TestReadOnlyFileWorldWritesNothing: a read-only world needs its directory
// and its policy to exist; the open authors neither.
func TestReadOnlyFileWorldWritesNothing(t *testing.T) {
	tokens := writeTokens(t, t.TempDir(), "ro")
	missing := filepath.Join(t.TempDir(), "absent")
	if _, err := openFileWorld(t, "worlds:\n"+fileWorldFragment("ro", missing, tokens, true)); err == nil || !strings.Contains(err.Error(), "file world") {
		t.Fatalf("read-only world on a missing directory: err %v, want the open refused", err)
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the refused open created the directory: %v", err)
	}

	empty := t.TempDir()
	if _, err := openFileWorld(t, "worlds:\n"+fileWorldFragment("ro", empty, tokens, true)); err == nil {
		t.Fatal("read-only world on an empty directory came up without a policy")
	}
	entries, err := os.ReadDir(empty)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("read-only open wrote %d entries into the directory", len(entries))
	}
}

// TestOpenServesFileWorldsWithoutGCS: a server whose worlds are all
// directories opens with no bucket factory at all.
func TestOpenServesFileWorldsWithoutGCS(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile := testCertFiles(t, "team.example.com")
	tokens := writeTokens(t, dir, "team")
	configFile := filepath.Join(dir, "config.yaml")
	config := fmt.Sprintf("version: 1\nlisten:\n  address: 127.0.0.1:0\nhealth:\n  address: 127.0.0.1:0\ntls:\n  certFile: %s\n  keyFile: %s\nworlds:\n%s", certFile, keyFile, fileWorldFragment("team", filepath.Join(dir, "team"), tokens, false))
	if err := os.WriteFile(configFile, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	server, err := Open(Options{ConfigFile: configFile})
	if err != nil {
		t.Fatalf("Open with file worlds and no GCS credentials: %v", err)
	}
	defer server.Close()
	if !server.Routes("team.example.com") {
		t.Error("the file world is not routed")
	}
}

// TestOpenRefusesABucketWorldWithoutAFactory: a binary with no bucket store
// names the world it cannot open instead of failing somewhere inside GCS.
func TestOpenRefusesABucketWorldWithoutAFactory(t *testing.T) {
	tokens := writeTokens(t, t.TempDir(), "alice")
	h := newHarness(t.TempDir())
	err := h.open(t, "worlds:\n"+worldFragment("alice", testWorldID, tokens, false), worldStores(nil))
	if err == nil || !strings.Contains(err.Error(), "world alice is on a bucket, which this binary cannot open") {
		t.Fatalf("bucket world with no factory: err %v; want the refusal by name", err)
	}
}
