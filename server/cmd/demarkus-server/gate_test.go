package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/neardup/neardtest"
	"github.com/latebit-io/demarkus/protocol/publishpolicy"
	"github.com/latebit-io/demarkus/protocol/wiretest"
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
	tagged := func() map[string]string { return map[string]string{"expected-version": "0", "tags": "postgres"} }

	// The refusal is the wire golden's, path aside: one wording, pinned once.
	want := wiretest.Response(t, "publish-refused")
	if r := send("/fixture/dsn.md", tagged(), storetest.WireSecret); r.Status != want.Status || r.Body != want.Body {
		t.Errorf("secret on a soul: status %s, body %q; want the publish-refused golden", r.Status, r.Body)
	}
	if r := send("/fixture/dsn.md", tagged(), storetest.WireSecretFixed); r.Status != protocol.StatusCreated || r.Metadata["warning"] != "" {
		t.Errorf("placeholder password: status %s, warning %q; want created, no warning", r.Status, r.Metadata["warning"])
	}
	// A reworded copy of a document the soul holds is refused, naming it.
	if r := send("/fixture/lesson.md", tagged(), neardtest.LessonDoc); r.Status != protocol.StatusCreated || r.Metadata["warning"] != "" {
		t.Fatalf("lesson: status %s, warning %q; want created, no warning", r.Status, r.Metadata["warning"])
	}
	if r := send("/fixture/lesson-again.md", tagged(), neardtest.RewordedDoc); r.Status != protocol.StatusBadRequest || !strings.Contains(r.Body, "near-duplicate check refused this write: near-duplicate of /fixture/lesson.md") {
		t.Errorf("reworded lesson: status %s, body %q; want bad-request naming the lesson", r.Status, r.Body)
	}
	// No policy document is the empty policy at warn: an untagged write lands
	// and says so, the hook's tag gate mirrored on the server.
	if r := send("/fixture/dsn.md", map[string]string{"expected-version": "1"}, "# Untagged\n"); r.Status != protocol.StatusCreated || r.Metadata["warning"] != "publish policy (warn): missing-tags" {
		t.Errorf("no policy document: status %s, warning %q; want created with the tag warning", r.Status, r.Metadata["warning"])
	}

	policy := "# Write Policy\n\nstrictness: block\n"
	if r := send(publishpolicy.DocumentPath, map[string]string{"expected-version": "0", "tags": "policy", "type": "Policy"}, policy); r.Status != protocol.StatusCreated {
		t.Fatalf("publish a soul policy: status %s, body %q", r.Status, r.Body)
	}
	if r := send("/fixture/dsn.md", map[string]string{"expected-version": "2"}, "# Untagged again\n"); r.Status != protocol.StatusBadRequest || !strings.Contains(r.Body, "missing-tags") {
		t.Errorf("untagged under a block policy: status %s, body %q; want bad-request naming missing-tags", r.Status, r.Body)
	}
	if r := send(publishpolicy.DocumentPath, map[string]string{"expected-version": "1", "tags": "policy"}, "# Write Policy\n\nstrictness: nonsense\n"); r.Status != protocol.StatusBadRequest {
		t.Errorf("malformed policy candidate: status %s; want bad-request", r.Status)
	}
}
