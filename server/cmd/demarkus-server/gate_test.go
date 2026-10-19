package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/publishpolicy"
	"github.com/latebit-io/demarkus/server/internal/storetest"
)

// TestSoulWritesAreGated holds the soul server to the knowledge server's
// write gate: the content scan on every body (the DSN the doctor found
// three times in the demarkus soul), and a policy document once published.
func TestSoulWritesAreGated(t *testing.T) {
	world, token := openUpgradeCapture(t, filepath.Join("..", "..", "..", "testdata", "upgrade", "home"))
	send := func(path string, meta map[string]string, body string) protocol.Response {
		meta["auth"] = token
		return exchange(t, world, protocol.Request{Verb: protocol.VerbPublish, Path: path, Metadata: meta, Body: body})
	}
	tagged := func(version string) map[string]string {
		return map[string]string{"expected-version": version, "tags": "postgres"}
	}

	r := send("/fixture/dsn.md", tagged("0"), storetest.WireSecret)
	if r.Status != protocol.StatusCreated || !strings.HasPrefix(r.Metadata["warning"], "content check: secret/connection-string-password at line 4") || strings.Contains(r.Metadata["warning"], "55432") {
		t.Errorf("secret on a soul: status %s, warning %q; want created with the rule and line, never the value", r.Status, r.Metadata["warning"])
	}
	clean := strings.Replace(storetest.WireSecret, "postgres:postgres@", "postgres:${POSTGRES_PASSWORD}@", 1)
	if r := send("/fixture/dsn.md", tagged("1"), clean); r.Status != protocol.StatusCreated || r.Metadata["warning"] != "" {
		t.Errorf("placeholder password: status %s, warning %q; want created, no warning", r.Status, r.Metadata["warning"])
	}
	// No policy document is the empty policy at warn: an untagged write lands
	// and says so, the hook's tag gate mirrored on the server.
	if r := send("/fixture/dsn.md", map[string]string{"expected-version": "2"}, "# Untagged\n"); r.Status != protocol.StatusCreated || r.Metadata["warning"] != "publish policy (warn): missing-tags" {
		t.Errorf("no policy document: status %s, warning %q; want created with the tag warning", r.Status, r.Metadata["warning"])
	}

	policy := "# Write Policy\n\nstrictness: block\n"
	if r := send(publishpolicy.DocumentPath, map[string]string{"expected-version": "0", "tags": "policy", "type": "Policy"}, policy); r.Status != protocol.StatusCreated {
		t.Fatalf("publish a soul policy: status %s, body %q", r.Status, r.Body)
	}
	if r := send("/fixture/dsn.md", map[string]string{"expected-version": "3"}, "# Untagged again\n"); r.Status != protocol.StatusBadRequest || !strings.Contains(r.Body, "missing-tags") {
		t.Errorf("untagged under a block policy: status %s, body %q; want bad-request naming missing-tags", r.Status, r.Body)
	}
	if r := send(publishpolicy.DocumentPath, map[string]string{"expected-version": "1", "tags": "policy"}, "# Write Policy\n\nstrictness: nonsense\n"); r.Status != protocol.StatusBadRequest {
		t.Errorf("malformed policy candidate: status %s; want bad-request", r.Status)
	}
}
