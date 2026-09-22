package join

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/registry/catalog"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/registry/knowledge"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/registry/registrytest"

	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/config"
)

func TestMemoryJoinAndCollision(t *testing.T) {
	home := registrytest.SetupHome(t)
	repo := filepath.Join(home, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	res, err := Memory("soul.demarkus.io", "tok", true, repo)
	if err != nil {
		t.Fatal(err)
	}
	if res.Slug != "soul" {
		t.Fatalf("slug: want soul, got %s", res.Slug)
	}
	// token written 0600
	info, err := os.Stat(res.TokenFile)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("token file perms: %v %v", err, info)
	}
	// binding recorded
	if b, _ := catalog.IsMemory("soul"); !b {
		t.Error("soul should be a catalog memory")
	}
	// collision: a different host under the same slug is rejected
	if _, err := Memory("soul.other.net", "t2", false, ""); err == nil {
		t.Error("expected slug collision error for a different host")
	}
	// re-join same host is fine (idempotent upsert)
	if _, err := Memory("soul.demarkus.io", "tok2", true, ""); err != nil {
		t.Errorf("re-join same host should succeed: %v", err)
	}
	// reserved slug rejected
	if _, err := Memory("demarkus-memory.example.com", "", false, ""); err == nil {
		t.Error("expected reserved-slug rejection")
	}
	// the branded MCP server name is reserved too
	if _, _, err := catalog.SetLocalAlias("brain"); err != nil {
		t.Fatal(err)
	}
	if _, err := Memory("brain.example.com", "", false, ""); err == nil {
		t.Error("expected alias-slug rejection")
	}
	if err := knowledge.Register("brain"); err == nil {
		t.Error("expected alias-slug rejection on knowledge register")
	}
	// a slug the routing predicate would read as "<plugin>_<alias>" is reserved too
	if _, _, err := catalog.SetLocalAlias("memory"); err != nil {
		t.Fatal(err)
	}
	if _, err := Memory("acme-memory.example.com", "", false, ""); err == nil {
		t.Error("expected plugin-prefixed alias rejection on join")
	}
	if err := knowledge.Register("acme-memory"); err == nil {
		t.Error("expected plugin-prefixed alias rejection on knowledge register")
	}
}

func TestMemoryJoinRollsBackBindingFailure(t *testing.T) {
	home := registrytest.SetupHome(t)
	repo := filepath.Join(home, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	registrytest.FailWritesTo(t, "project-souls")

	if _, err := Memory("soul.demarkus.io", "secret", false, repo); err == nil {
		t.Fatal("Memory succeeded despite binding failure")
	}
	for _, name := range []string{"souls", "project-souls", "soul-soul.token"} {
		path := filepath.Join(home, ".demarkus", name)
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("partial state remains at %s: %v", path, err)
		}
	}
}

func TestMemoryJoinRestoresExistingStateOnBindingFailure(t *testing.T) {
	home := registrytest.SetupHome(t)
	repo := filepath.Join(home, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Memory("soul.demarkus.io", "old-token", false, repo); err != nil {
		t.Fatal(err)
	}
	paths := []string{
		filepath.Join(home, ".demarkus", "souls"),
		filepath.Join(home, ".demarkus", "project-souls"),
		filepath.Join(home, ".demarkus", "soul-soul.token"),
	}
	before := make(map[string]string, len(paths))
	beforeMode := make(map[string]os.FileMode, len(paths))
	for _, path := range paths {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		before[path] = string(body)
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		beforeMode[path] = info.Mode().Perm()
	}

	registrytest.FailWritesTo(t, "project-souls")
	if _, err := Memory("soul.demarkus.io", "new-token", true, repo); err == nil {
		t.Fatal("Memory succeeded despite binding failure")
	}
	for _, path := range paths {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(body) != before[path] {
			t.Errorf("state changed at %s: got %q, want %q", path, body, before[path])
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != beforeMode[path] {
			t.Errorf("mode changed at %s: got %o, want %o", path, info.Mode().Perm(), beforeMode[path])
		}
	}
}

func TestMemoryJoinWithoutTokenRemovesManagedToken(t *testing.T) {
	home := registrytest.SetupHome(t)
	if _, err := Memory("soul.demarkus.io", "old-token", false, ""); err != nil {
		t.Fatal(err)
	}
	tokenPath := filepath.Join(home, ".demarkus", "soul-soul.token")
	if _, err := Memory("soul.demarkus.io", "", false, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(tokenPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("managed token remains after tokenless rejoin: %v", err)
	}
	row, ok, err := catalog.RemoteRow("soul")
	if err != nil || !ok || row.TokenFile != "-" {
		t.Fatalf("tokenless row = %+v, ok=%v, err=%v", row, ok, err)
	}
}

func TestMemoryJoinWithoutTokenRemovesOrphanManagedToken(t *testing.T) {
	home := registrytest.SetupHome(t)
	tokenPath := filepath.Join(home, ".demarkus", "soul-soul.token")
	if err := os.MkdirAll(filepath.Dir(tokenPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokenPath, []byte("orphan-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Memory("soul.demarkus.io", "", false, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(tokenPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("orphan managed token remains after tokenless join: %v", err)
	}
}

func TestMemoryJoinWithoutTokenPreservesExternalTokenReference(t *testing.T) {
	home := registrytest.SetupHome(t)
	tokenPath := filepath.Join(home, "external.token")
	if err := os.WriteFile(tokenPath, []byte("external-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	registrytest.RegisterRow(t, config.MemoryRow{Slug: "soul", Host: "mark://soul.demarkus.io", TokenFile: tokenPath})
	if _, err := Memory("soul.demarkus.io", "", false, ""); err == nil || !strings.Contains(err.Error(), "externally managed token file") {
		t.Fatalf("tokenless rejoin error = %v", err)
	}
	row, ok, err := catalog.RemoteRow("soul")
	if err != nil || !ok || row.TokenFile != tokenPath {
		t.Fatalf("external token row = %+v, ok=%v, err=%v", row, ok, err)
	}
	body, err := os.ReadFile(tokenPath)
	if err != nil || string(body) != "external-token" {
		t.Fatalf("external token changed: body=%q err=%v", body, err)
	}
}

func TestMemoryJoinWithTokenReportsReplacedExternalTokenReference(t *testing.T) {
	home := registrytest.SetupHome(t)
	externalPath := filepath.Join(home, "external.token")
	if err := os.WriteFile(externalPath, []byte("external-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	registrytest.RegisterRow(t, config.MemoryRow{Slug: "soul", Host: "mark://soul.demarkus.io", TokenFile: externalPath})
	warning, joinErr := registrytest.CaptureStderr(t, func() error {
		_, err := Memory("soul.demarkus.io", "managed-token", false, "")
		return err
	})
	if joinErr != nil {
		t.Fatal(joinErr)
	}
	if !strings.Contains(warning, externalPath) || !strings.Contains(warning, "old external token file still exists") {
		t.Fatalf("warning = %q", warning)
	}
	if body, err := os.ReadFile(externalPath); err != nil || string(body) != "external-token" {
		t.Fatalf("external token changed: body=%q err=%v", body, err)
	}
}

func TestMemoryJoinDoesNotReportExternalTokenReplacementAfterRollback(t *testing.T) {
	home := registrytest.SetupHome(t)
	externalPath := filepath.Join(home, "external.token")
	if err := os.WriteFile(externalPath, []byte("external-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	registrytest.RegisterRow(t, config.MemoryRow{Slug: "soul", Host: "mark://soul.demarkus.io", TokenFile: externalPath})
	registrytest.FailWritesTo(t, "project-souls")
	warning, joinErr := registrytest.CaptureStderr(t, func() error {
		_, err := Memory("soul.demarkus.io", "managed-token", false, filepath.Join(home, "repo"))
		return err
	})
	if joinErr == nil {
		t.Fatal("Memory succeeded despite binding failure")
	}
	if warning != "" {
		t.Fatalf("warning after rollback = %q", warning)
	}
	row, ok, err := catalog.RemoteRow("soul")
	if err != nil || !ok || row.TokenFile != externalPath {
		t.Fatalf("external token row after rollback = %+v, ok=%v, err=%v", row, ok, err)
	}
}

func TestMemoryJoinRestoresDeletedTokenOnLaterFailure(t *testing.T) {
	home := registrytest.SetupHome(t)
	if _, err := Memory("soul.demarkus.io", "old-token", false, ""); err != nil {
		t.Fatal(err)
	}
	tokenPath := filepath.Join(home, ".demarkus", "soul-soul.token")
	registrytest.FailWritesTo(t, "project-souls")
	if _, err := Memory("soul.demarkus.io", "", false, filepath.Join(home, "repo")); err == nil {
		t.Fatal("Memory succeeded despite binding failure")
	}
	body, err := os.ReadFile(tokenPath)
	if err != nil || string(body) != "old-token" {
		t.Fatalf("token rollback: body=%q err=%v", body, err)
	}
	info, err := os.Stat(tokenPath)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("token rollback mode: info=%v err=%v", info, err)
	}
}

func TestMemoryJoinURLWithFragment(t *testing.T) {
	home := registrytest.SetupHome(t)
	repo := filepath.Join(home, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}

	res, err := Memory("mark://kb.example.com#token=fragtok", "", false, repo)
	if err != nil {
		t.Fatal(err)
	}
	if res.Slug != "kb" || res.Host != "mark://kb.example.com" {
		t.Fatalf("slug/host: got %s %s", res.Slug, res.Host)
	}
	b, err := os.ReadFile(res.TokenFile)
	if err != nil || string(b) != "fragtok" {
		t.Fatalf("token file: %v %q", err, b)
	}

	row, ok, err := catalog.RemoteRow("kb")
	if err != nil || !ok {
		t.Fatalf("RemoteRow: %v ok=%v", err, ok)
	}
	if row.Host != "mark://kb.example.com" || row.Insecure {
		t.Fatalf("row round trip: %+v", row)
	}

	// Conflicting explicit --token and fragment token is rejected.
	if _, err := Memory("mark://kb2.example.com#token=a", "b", false, ""); err == nil {
		t.Error("expected conflict error for --token + fragment token")
	}
	// Bad fragment key fails loudly.
	if _, err := Memory("mark://kb3.example.com#tokn=a", "", false, ""); err == nil {
		t.Error("expected error for unknown fragment key")
	}
	// Bracketed IPv6 would derive a garbage slug; the join-URL path rejects it.
	if _, err := Memory("mark://[2001:db8::1]:6309#token=a", "", false, ""); err == nil {
		t.Error("expected error for bracketed IPv6 join URL")
	}
	// Malformed hosts on the legacy (no-fragment) path fail loudly too.
	for _, bad := range []string{
		"mark://kb.example.com?x=1",
		"mark://user@kb.example.com",
		"mark://kb.example.com/docs",
	} {
		if _, err := Memory(bad, "t", false, ""); err == nil {
			t.Errorf("expected error for malformed host %q", bad)
		}
	}
}

func TestMemoryJoinBroker(t *testing.T) {
	t.Setenv("DEMARKUS_KNOWLEDGE_JOIN_ALLOW_HTTP", "1")
	registrytest.SetupHome(t)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/.well-known/oauth-protected-resource" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ts.Close()

	res, err := Memory(ts.URL, "", false, "")
	if err != nil {
		t.Fatalf("Memory(broker): %v", err)
	}
	if !res.Broker || res.McpURL != res.Host+"/mcp" {
		t.Errorf("broker result = %+v", res)
	}
	if res.TokenFile != "-" {
		t.Errorf("broker memory must not reference a token file, got %q", res.TokenFile)
	}
	// Catalog row lands in SOULS (destination gate + binding apply).
	row, ok, err := catalog.RemoteRow(res.Slug)
	if err != nil || !ok {
		t.Fatalf("catalog row missing after broker join: ok=%v err=%v", ok, err)
	}
	if row.Host != res.Host {
		t.Errorf("catalog host = %q, want %q", row.Host, res.Host)
	}

	// A token alongside an HTTPS broker URL is rejected: OAuth owns auth.
	if _, err := Memory(ts.URL, "sekret", false, ""); err == nil {
		t.Error("broker join accepted a token")
	}
}

func TestValidateBrokerEndpointRejectsUserinfo(t *testing.T) {
	t.Setenv("DEMARKUS_KNOWLEDGE_JOIN_ALLOW_HTTP", "1")
	registrytest.SetupHome(t)

	requests := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	withUser := strings.Replace(ts.URL, "http://", "http://user:password@", 1)
	if _, err := Memory(withUser, "", false, ""); err == nil || !strings.Contains(err.Error(), "userinfo") {
		t.Fatalf("userinfo URL err = %v, want userinfo rejection", err)
	}
	// The rejection must happen before any request: credentials in the
	// URL must never reach the wire as a Basic Authorization header.
	if requests != 0 {
		t.Fatalf("userinfo URL produced %d requests, want 0", requests)
	}
	// No catalog row may exist for the bogus "user" slug.
	if _, ok, err := catalog.RemoteRow("user"); err != nil || ok {
		t.Fatalf("catalog row after rejected join: ok=%v err=%v", ok, err)
	}
}
