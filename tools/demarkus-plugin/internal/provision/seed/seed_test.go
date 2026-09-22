package seed

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/config"
)

// mockSeedClient records Publish calls and returns canned responses.
type publishCall struct {
	path, body string
	expected   int
}

type mockSeedClient struct {
	fetchStatus   string
	fetchErr      error
	publishStatus string
	published     []publishCall
}

func TestSeedDocPublishesThroughProtocol(t *testing.T) {
	// Already served (or archived, or unreachable): never publishes.
	for _, tt := range []struct {
		name string
		mock *mockSeedClient
	}{
		{"already served", &mockSeedClient{fetchStatus: protocol.StatusOK}},
		{"archived", &mockSeedClient{fetchStatus: protocol.StatusArchived}},
		{"server unreachable", &mockSeedClient{fetchErr: errors.New("dial refused")}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			seedDoc(t.Context(), tt.mock, seedSpec{Host: "localhost:1", Token: "tok", Name: "index.md"})
			if len(tt.mock.published) != 0 {
				t.Errorf("published %d docs, want 0", len(tt.mock.published))
			}
		})
	}

	// Not found: publishes the embedded seed, create-only.
	m := &mockSeedClient{fetchStatus: protocol.StatusNotFound}
	seedDoc(t.Context(), m, seedSpec{Host: "localhost:1", Token: "tok", Name: "index.md"})
	if len(m.published) != 1 {
		t.Fatalf("published %d docs, want 1", len(m.published))
	}
	if m.published[0].path != "/index.md" || m.published[0].expected != 0 {
		t.Errorf("publish = %+v, want path /index.md expected-version 0", m.published[0])
	}
	if !strings.Contains(m.published[0].body, "# Projects") {
		t.Errorf("seeded content does not look like index.md: %q", m.published[0].body[:min(40, len(m.published[0].body))])
	}
}

// TestPristineTemplateHashes pins the hash set to the checked-in copies of
// every seed variant ever shipped; a drifted hash would misclassify a pristine
// flat file as user-customized and publish it as a stale layout override.
func TestPristineTemplateHashes(t *testing.T) {
	variants, err := filepath.Glob("testdata/project-template-v*.md")
	if err != nil || len(variants) != len(pristineTemplateHashes) {
		t.Fatalf("testdata variants = %d (err %v), want %d", len(variants), err, len(pristineTemplateHashes))
	}
	for _, f := range variants {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(b)
		if !pristineTemplateHashes[hex.EncodeToString(sum[:])] {
			t.Errorf("%s hash %x not in pristineTemplateHashes", f, sum)
		}
	}
}

func TestCleanupLegacyTemplate(t *testing.T) {
	// A real shipped seed variant; its hash is in pristineTemplateHashes.
	pristine, err := os.ReadFile("testdata/project-template-v2.md")
	if err != nil {
		t.Fatal(err)
	}

	write := func(t *testing.T, content []byte) string {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "project-template.md"), content, 0o644); err != nil {
			t.Fatal(err)
		}
		return dir
	}

	// Pristine seed: deleted.
	dir := write(t, pristine)
	CleanupLegacyTemplate(dir)
	if config.FileExists(filepath.Join(dir, "project-template.md")) {
		t.Error("pristine flat template not deleted")
	}

	// Customized: left in place for the store's flat-file migration.
	custom := []byte("# My layout\n\ncustomized\n")
	dir = write(t, custom)
	CleanupLegacyTemplate(dir)
	got, err := os.ReadFile(filepath.Join(dir, "project-template.md"))
	if err != nil || !bytes.Equal(got, custom) {
		t.Errorf("customized template altered: %q (err %v)", got, err)
	}

	// Absent: nothing happens.
	CleanupLegacyTemplate(t.TempDir())
}

func (m *mockSeedClient) Fetch(context.Context, fetch.FetchRequest) (fetch.Result, error) {
	if m.fetchErr != nil {
		return fetch.Result{}, m.fetchErr
	}
	return fetch.Result{Response: protocol.Response{Status: m.fetchStatus}}, nil
}

func (m *mockSeedClient) Publish(_ context.Context, r fetch.WriteRequest) (fetch.Result, error) {
	m.published = append(m.published, publishCall{r.Path, r.Body, r.ExpectedVersion})
	st := m.publishStatus
	if st == "" {
		st = protocol.StatusCreated
	}
	return fetch.Result{Response: protocol.Response{Status: st}}, nil
}
