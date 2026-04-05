package fedcrawl

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadStateNonexistent(t *testing.T) {
	s, err := LoadState("/nonexistent/state.json")
	if err != nil {
		t.Fatalf("LoadState() error: %v", err)
	}
	if s.ServerCount() != 0 {
		t.Errorf("ServerCount() = %d, want 0", s.ServerCount())
	}
	if s.URLCount() != 0 {
		t.Errorf("URLCount() = %d, want 0", s.URLCount())
	}
}

func TestStateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	s1, err := LoadState(path)
	if err != nil {
		t.Fatalf("LoadState() error: %v", err)
	}

	// Record some state.
	s1.RecordVisit("mark://example.com/index.md", "etag123", "ok", "sha256-abc")
	s1.RecordVisit("mark://example.com/about.md", "etag456", "ok", "sha256-def")
	s1.RecordServer("example.com:6309", 2)

	if err := s1.Save(); err != nil {
		t.Fatalf("Save() error: %v", err)
	}

	// Load again.
	s2, err := LoadState(path)
	if err != nil {
		t.Fatalf("LoadState() second call error: %v", err)
	}

	if s2.ServerCount() != 1 {
		t.Errorf("ServerCount() = %d, want 1", s2.ServerCount())
	}
	if s2.URLCount() != 2 {
		t.Errorf("URLCount() = %d, want 2", s2.URLCount())
	}

	u := s2.GetURL("mark://example.com/index.md")
	if u == nil {
		t.Fatal("GetURL() returned nil")
	}
	if u.Etag != "etag123" {
		t.Errorf("Etag = %q, want etag123", u.Etag)
	}
	if u.ContentHash != "sha256-abc" {
		t.Errorf("ContentHash = %q, want sha256-abc", u.ContentHash)
	}

	sv := s2.GetServer("example.com:6309")
	if sv == nil {
		t.Fatal("GetServer() returned nil")
	}
	if sv.DocumentCount != 2 {
		t.Errorf("DocumentCount = %d, want 2", sv.DocumentCount)
	}
}

func TestStateInvalidJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := os.WriteFile(path, []byte("not valid json {{{"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := LoadState(path)
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestStateInvalidVersion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	content := `{"version": 999, "servers": {}, "urls": {}}`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := LoadState(path)
	if err == nil {
		t.Fatal("expected error for invalid version")
	}
}

func TestShouldFetch(t *testing.T) {
	s, err := LoadState("/nonexistent/state.json")
	if err != nil {
		t.Fatalf("LoadState() error: %v", err)
	}

	// Not visited.
	should, etag := s.ShouldFetch("mark://example.com/new.md")
	if !should {
		t.Error("ShouldFetch() = false for new URL, want true")
	}
	if etag != "" {
		t.Errorf("etag = %q, want empty", etag)
	}

	// Visited with etag.
	s.RecordVisit("mark://example.com/visited.md", "etag789", "ok", "sha256-xyz")
	should, etag = s.ShouldFetch("mark://example.com/visited.md")
	if !should {
		t.Error("ShouldFetch() = false for visited URL, want true (always refetch for now)")
	}
	if etag != "etag789" {
		t.Errorf("etag = %q, want etag789", etag)
	}

	// Visited without etag (error case).
	s.RecordVisit("mark://example.com/error.md", "", "error", "")
	should, etag = s.ShouldFetch("mark://example.com/error.md")
	if !should {
		t.Error("ShouldFetch() = false for error URL, want true")
	}
	if etag != "" {
		t.Errorf("etag = %q, want empty for error URL", etag)
	}
}

func TestAllContentHashes(t *testing.T) {
	s, err := LoadState("/nonexistent/state.json")
	if err != nil {
		t.Fatalf("LoadState() error: %v", err)
	}

	s.RecordVisit("mark://a.com/x.md", "e1", "ok", "hash1")
	s.RecordVisit("mark://b.com/y.md", "e2", "ok", "hash2")
	s.RecordVisit("mark://c.com/z.md", "e3", "not-found", "") // no hash

	hashes := s.AllContentHashes()
	if len(hashes) != 2 {
		t.Errorf("len(AllContentHashes()) = %d, want 2", len(hashes))
	}
	if hashes["hash1"] != "mark://a.com/x.md" {
		t.Errorf("hashes[hash1] = %q, want mark://a.com/x.md", hashes["hash1"])
	}
}

func TestKnownServers(t *testing.T) {
	s, err := LoadState("/nonexistent/state.json")
	if err != nil {
		t.Fatalf("LoadState() error: %v", err)
	}

	s.RecordServer("a.com:6309", 10)
	s.RecordServer("b.com:6309", 20)

	servers := s.KnownServers()
	if len(servers) != 2 {
		t.Errorf("len(KnownServers()) = %d, want 2", len(servers))
	}
}

func TestRecordServerUpdate(t *testing.T) {
	s, err := LoadState("/nonexistent/state.json")
	if err != nil {
		t.Fatalf("LoadState() error: %v", err)
	}

	// First record.
	s.RecordServer("example.com:6309", 5)
	sv1 := s.GetServer("example.com:6309")
	if sv1.DocumentCount != 5 {
		t.Errorf("DocumentCount = %d, want 5", sv1.DocumentCount)
	}
	discoveredAt := sv1.DiscoveredAt

	// Wait a bit to ensure time difference.
	time.Sleep(10 * time.Millisecond)

	// Update.
	s.RecordServer("example.com:6309", 10)
	sv2 := s.GetServer("example.com:6309")
	if sv2.DocumentCount != 10 {
		t.Errorf("DocumentCount = %d, want 10", sv2.DocumentCount)
	}
	// DiscoveredAt should not change.
	if !sv2.DiscoveredAt.Equal(discoveredAt) {
		t.Errorf("DiscoveredAt changed on update")
	}
	// LastCrawled should update.
	if !sv2.LastCrawled.After(sv1.LastCrawled) {
		t.Errorf("LastCrawled should be updated")
	}
}

func TestDefaultStatePath(t *testing.T) {
	path := DefaultStatePath()
	if path == "" {
		t.Skip("could not determine home directory")
	}
	if len(path) < 20 { // arbitrary minimum
		t.Errorf("DefaultStatePath() = %q, seems too short", path)
	}
}
