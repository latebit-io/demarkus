package registry

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/config"
)

func setupHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".demarkus"), 0o755); err != nil {
		t.Fatal(err)
	}
	return home
}

func failWritesTo(t *testing.T, name string) {
	t.Helper()
	originalWriter := writeStateFile
	t.Cleanup(func() { writeStateFile = originalWriter })
	writeStateFile = func(path string, data []byte, mode os.FileMode) error {
		if filepath.Base(path) == name {
			return errors.New("injected write failure")
		}
		return atomicWritePerm(path, data, mode)
	}
}

func captureStderr(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	stderr, err := os.CreateTemp(t.TempDir(), "stderr-")
	if err != nil {
		t.Fatal(err)
	}
	originalStderr := os.Stderr
	os.Stderr = stderr
	callErr := fn()
	os.Stderr = originalStderr
	if err := stderr.Close(); err != nil {
		t.Fatal(err)
	}
	warning, err := os.ReadFile(stderr.Name())
	if err != nil {
		t.Fatal(err)
	}
	return string(warning), callErr
}

func TestAtomicWritePermConcurrent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state")
	const writers = 32
	start := make(chan struct{})
	errs := make(chan error, writers)
	var wg sync.WaitGroup
	for i := range writers {
		wg.Go(func() {
			<-start
			errs <- atomicWritePerm(path, []byte("writer-"+strconv.Itoa(i)), 0o600)
		})
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	body, err := os.ReadFile(path)
	if err != nil || !strings.HasPrefix(string(body), "writer-") {
		t.Fatalf("final state: body=%q err=%v", body, err)
	}
	temps, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".state.*.tmp"))
	if err != nil || len(temps) != 0 {
		t.Fatalf("temporary files: %v err=%v", temps, err)
	}
}

func TestProjectBindSetUsesTransactionWriter(t *testing.T) {
	home := setupHome(t)
	if err := SoulRegister("remote", "mark://remote.example", false, "-"); err != nil {
		t.Fatal(err)
	}
	failWritesTo(t, "project-souls")
	if err := ProjectBindSet(filepath.Join(home, "repo"), "remote"); err == nil {
		t.Fatal("ProjectBindSet succeeded despite injected write failure")
	}
	path, err := config.StatePath("project-souls")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("binding file exists after failed write: %v", err)
	}
}

func TestDeriveSlug(t *testing.T) {
	cases := map[string]string{
		"mcp.broker.acme.com": "acme",
		"MCP.BROKER.Acme.com": "acme",
		"broker.acme.com":     "acme",
		"acme-broker.example": "acme-broker",
		"soul.demarkus.io":    "soul",
	}
	for host, want := range cases {
		if got := deriveSlug(host); got != want {
			t.Errorf("deriveSlug(%q) = %q, want %q", host, got, want)
		}
	}
}

func TestMcpAddRemoveList(t *testing.T) {
	setupHome(t)
	if err := McpAdd("foo", "bash", []string{"/x.sh"}); err != nil {
		t.Fatal(err)
	}
	if err := McpAddHTTP("kb", "https://b/mcp"); err != nil {
		t.Fatal(err)
	}
	names, _ := McpList()
	if len(names) != 2 {
		t.Fatalf("want 2 servers, got %v", names)
	}
	existed, _ := McpRemove("foo")
	if !existed {
		t.Error("foo should have existed")
	}
	existed, _ = McpRemove("nope")
	if existed {
		t.Error("nope should not exist")
	}
}

func TestMcpRejectsArrayConfig(t *testing.T) {
	home := setupHome(t)
	cfgDir := filepath.Join(home, ".config", "mcp")
	_ = os.MkdirAll(cfgDir, 0o755)
	cfg := filepath.Join(cfgDir, "mcp.json")
	_ = os.WriteFile(cfg, []byte("[]"), 0o644)
	// A malformed (array) config must error, NOT be silently reset + written back.
	if err := McpAdd("foo", "bash", nil); err == nil {
		t.Fatal("array-shaped config should error, not silently reset")
	}
	if b, _ := os.ReadFile(cfg); strings.TrimSpace(string(b)) != "[]" {
		t.Fatalf("malformed config must be left untouched, got %q", string(b))
	}
}

func TestSoulJoinAndCollision(t *testing.T) {
	home := setupHome(t)
	repo := filepath.Join(home, "repo")
	_ = os.MkdirAll(repo, 0o755)
	res, err := SoulJoin("soul.demarkus.io", "tok", true, repo)
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
	if b, _ := IsCatalogSoul("soul"); !b {
		t.Error("soul should be a catalog soul")
	}
	// collision: a different host under the same slug is rejected
	if _, err := SoulJoin("soul.other.net", "t2", false, ""); err == nil {
		t.Error("expected slug collision error for a different host")
	}
	// re-join same host is fine (idempotent upsert)
	if _, err := SoulJoin("soul.demarkus.io", "tok2", true, ""); err != nil {
		t.Errorf("re-join same host should succeed: %v", err)
	}
	// reserved slug rejected
	if _, err := SoulJoin("demarkus-memory.example.com", "", false, ""); err == nil {
		t.Error("expected reserved-slug rejection")
	}
}

func TestSoulJoinRollsBackBindingFailure(t *testing.T) {
	home := setupHome(t)
	repo := filepath.Join(home, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	failWritesTo(t, "project-souls")

	if _, err := SoulJoin("soul.demarkus.io", "secret", false, repo); err == nil {
		t.Fatal("SoulJoin succeeded despite binding failure")
	}
	for _, name := range []string{"souls", "project-souls", "soul-soul.token"} {
		path := filepath.Join(home, ".demarkus", name)
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("partial state remains at %s: %v", path, err)
		}
	}
}

func TestSoulJoinRestoresExistingStateOnBindingFailure(t *testing.T) {
	home := setupHome(t)
	repo := filepath.Join(home, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := SoulJoin("soul.demarkus.io", "old-token", false, repo); err != nil {
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

	failWritesTo(t, "project-souls")
	if _, err := SoulJoin("soul.demarkus.io", "new-token", true, repo); err == nil {
		t.Fatal("SoulJoin succeeded despite binding failure")
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

func TestSoulJoinWithoutTokenRemovesManagedToken(t *testing.T) {
	home := setupHome(t)
	if _, err := SoulJoin("soul.demarkus.io", "old-token", false, ""); err != nil {
		t.Fatal(err)
	}
	tokenPath := filepath.Join(home, ".demarkus", "soul-soul.token")
	if _, err := SoulJoin("soul.demarkus.io", "", false, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(tokenPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("managed token remains after tokenless rejoin: %v", err)
	}
	row, ok, err := RemoteSoulRow("soul")
	if err != nil || !ok || row.TokenFile != "-" {
		t.Fatalf("tokenless row = %+v, ok=%v, err=%v", row, ok, err)
	}
}

func TestSoulJoinWithoutTokenRemovesOrphanManagedToken(t *testing.T) {
	home := setupHome(t)
	tokenPath := filepath.Join(home, ".demarkus", "soul-soul.token")
	if err := os.MkdirAll(filepath.Dir(tokenPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokenPath, []byte("orphan-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := SoulJoin("soul.demarkus.io", "", false, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(tokenPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("orphan managed token remains after tokenless join: %v", err)
	}
}

func TestSoulJoinWithoutTokenPreservesExternalTokenReference(t *testing.T) {
	home := setupHome(t)
	tokenPath := filepath.Join(home, "external.token")
	if err := os.WriteFile(tokenPath, []byte("external-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SoulRegister("soul", "mark://soul.demarkus.io", false, tokenPath); err != nil {
		t.Fatal(err)
	}
	if _, err := SoulJoin("soul.demarkus.io", "", false, ""); err == nil || !strings.Contains(err.Error(), "externally managed token file") {
		t.Fatalf("tokenless rejoin error = %v", err)
	}
	row, ok, err := RemoteSoulRow("soul")
	if err != nil || !ok || row.TokenFile != tokenPath {
		t.Fatalf("external token row = %+v, ok=%v, err=%v", row, ok, err)
	}
	body, err := os.ReadFile(tokenPath)
	if err != nil || string(body) != "external-token" {
		t.Fatalf("external token changed: body=%q err=%v", body, err)
	}
}

func TestSoulJoinWithTokenReportsReplacedExternalTokenReference(t *testing.T) {
	home := setupHome(t)
	externalPath := filepath.Join(home, "external.token")
	if err := os.WriteFile(externalPath, []byte("external-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SoulRegister("soul", "mark://soul.demarkus.io", false, externalPath); err != nil {
		t.Fatal(err)
	}
	warning, joinErr := captureStderr(t, func() error {
		_, err := SoulJoin("soul.demarkus.io", "managed-token", false, "")
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

func TestSoulJoinDoesNotReportExternalTokenReplacementAfterRollback(t *testing.T) {
	home := setupHome(t)
	externalPath := filepath.Join(home, "external.token")
	if err := os.WriteFile(externalPath, []byte("external-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SoulRegister("soul", "mark://soul.demarkus.io", false, externalPath); err != nil {
		t.Fatal(err)
	}
	failWritesTo(t, "project-souls")
	warning, joinErr := captureStderr(t, func() error {
		_, err := SoulJoin("soul.demarkus.io", "managed-token", false, filepath.Join(home, "repo"))
		return err
	})
	if joinErr == nil {
		t.Fatal("SoulJoin succeeded despite binding failure")
	}
	if warning != "" {
		t.Fatalf("warning after rollback = %q", warning)
	}
	row, ok, err := RemoteSoulRow("soul")
	if err != nil || !ok || row.TokenFile != externalPath {
		t.Fatalf("external token row after rollback = %+v, ok=%v, err=%v", row, ok, err)
	}
}

func TestSoulJoinRestoresDeletedTokenOnLaterFailure(t *testing.T) {
	home := setupHome(t)
	if _, err := SoulJoin("soul.demarkus.io", "old-token", false, ""); err != nil {
		t.Fatal(err)
	}
	tokenPath := filepath.Join(home, ".demarkus", "soul-soul.token")
	failWritesTo(t, "project-souls")
	if _, err := SoulJoin("soul.demarkus.io", "", false, filepath.Join(home, "repo")); err == nil {
		t.Fatal("SoulJoin succeeded despite binding failure")
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

func TestPolicyMirror(t *testing.T) {
	home := setupHome(t)
	// PolicyMirror only writes for a registered slug, so register first.
	if err := KnowledgeRegister("acme"); err != nil {
		t.Fatal(err)
	}
	body := "strictness: block\nrequire_tags: category team\nrequire_fields: type\n\nsome prose strictness: ignored\n"
	if err := PolicyMirror("acme", body); err != nil {
		t.Fatal(err)
	}
	read := func(name string) string {
		b, _ := os.ReadFile(filepath.Join(home, ".demarkus", name))
		return string(b)
	}
	if read("plugin-knowledge.strictness.acme") != "block\n" {
		t.Errorf("strictness mirror wrong: %q", read("plugin-knowledge.strictness.acme"))
	}
	if read("plugin-knowledge.require-tags.acme") != "category team\n" {
		t.Errorf("require_tags mirror wrong: %q", read("plugin-knowledge.require-tags.acme"))
	}
	// a knob absent from a later policy clears its file
	if err := PolicyMirror("acme", "strictness: warn\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, ".demarkus", "plugin-knowledge.require-tags.acme")); !os.IsNotExist(err) {
		t.Error("require_tags file should be cleared when absent from policy")
	}
	// Mirroring an UNregistered slug writes nothing (a queued mirror after an
	// unregister must not resurrect policy files).
	if err := PolicyMirror("ghost", "strictness: block\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, ".demarkus", "plugin-knowledge.strictness.ghost")); !os.IsNotExist(err) {
		t.Error("policy for an unregistered slug should not be written")
	}
}

func TestKnowledgeRegisterUnregister(t *testing.T) {
	home := setupHome(t)
	if err := KnowledgeRegister("acme"); err != nil {
		t.Fatal(err)
	}
	if err := KnowledgeRegister("beta"); err != nil {
		t.Fatal(err)
	}
	// register is idempotent
	if err := KnowledgeRegister("acme"); err != nil {
		t.Fatal(err)
	}
	// mirror a policy so unregister can prove it clears the per-slug files
	if err := PolicyMirror("acme", "strictness: block\nrequire_tags: category\n"); err != nil {
		t.Fatal(err)
	}
	existed, err := KnowledgeUnregister("acme")
	if err != nil || !existed {
		t.Fatalf("unregister acme: existed=%v err=%v", existed, err)
	}
	systems, _ := config.ListKnowledgeSystems()
	if len(systems) != 1 || systems[0] != "beta" {
		t.Fatalf("after unregister want [beta], got %v", systems)
	}
	for _, name := range []string{"plugin-knowledge.strictness.acme", "plugin-knowledge.require-tags.acme"} {
		if _, err := os.Stat(filepath.Join(home, ".demarkus", name)); !os.IsNotExist(err) {
			t.Errorf("%s should be cleared on unregister", name)
		}
	}
	// unregistering an unknown slug is a no-op reporting existed=false
	existed, err = KnowledgeUnregister("ghost")
	if err != nil || existed {
		t.Fatalf("unregister ghost: existed=%v err=%v", existed, err)
	}
}

func TestPromoteTargetAdd(t *testing.T) {
	setupHome(t)
	if err := PromoteTargetAdd("acme", "/shared", "Acme shared"); err != nil {
		t.Fatal(err)
	}
	if err := PromoteTargetAdd("acme", "bad", ""); err == nil {
		t.Error("path not starting with / should error")
	}
	rows, _ := PromoteTargetList()
	if len(rows) != 1 || rows[0] != "acme /shared Acme shared" {
		t.Fatalf("promote target row wrong: %v", rows)
	}
}

func TestSoulJoinURLWithFragment(t *testing.T) {
	home := setupHome(t)
	repo := filepath.Join(home, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}

	res, err := SoulJoin("mark://kb.example.com#token=fragtok", "", false, repo)
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

	row, ok, err := RemoteSoulRow("kb")
	if err != nil || !ok {
		t.Fatalf("RemoteSoulRow: %v ok=%v", err, ok)
	}
	if row.Host != "mark://kb.example.com" || row.Insecure {
		t.Fatalf("row round trip: %+v", row)
	}

	// Conflicting explicit --token and fragment token is rejected.
	if _, err := SoulJoin("mark://kb2.example.com#token=a", "b", false, ""); err == nil {
		t.Error("expected conflict error for --token + fragment token")
	}
	// Bad fragment key fails loudly.
	if _, err := SoulJoin("mark://kb3.example.com#tokn=a", "", false, ""); err == nil {
		t.Error("expected error for unknown fragment key")
	}
	// Bracketed IPv6 would derive a garbage slug; the join-URL path rejects it.
	if _, err := SoulJoin("mark://[2001:db8::1]:6309#token=a", "", false, ""); err == nil {
		t.Error("expected error for bracketed IPv6 join URL")
	}
	// Malformed hosts on the legacy (no-fragment) path fail loudly too.
	for _, bad := range []string{
		"mark://kb.example.com?x=1",
		"mark://user@kb.example.com",
		"mark://kb.example.com/docs",
	} {
		if _, err := SoulJoin(bad, "t", false, ""); err == nil {
			t.Errorf("expected error for malformed host %q", bad)
		}
	}
}
