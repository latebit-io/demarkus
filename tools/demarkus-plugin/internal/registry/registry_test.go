package registry

import (
	"errors"
	"net/http"
	"net/http/httptest"
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
	if err := MemoryRegister("remote", "mark://remote.example", false, "-"); err != nil {
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

func TestProjectBindSetRejectsRecordDelimiters(t *testing.T) {
	setupHome(t)
	if err := MemoryRegister("remote", "mark://remote.example", false, "-"); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"/repo\tother", "/repo\rnext", "/repo\nnext", "/repo\x00next", " relative", "relative "} {
		if err := ProjectBindSet(dir, "remote"); err == nil {
			t.Errorf("binding directory %q should error", dir)
		}
	}
	path, err := config.StatePath("project-souls")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("binding file exists after rejected inputs: %v", err)
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

func TestMemoryJoinAndCollision(t *testing.T) {
	home := setupHome(t)
	repo := filepath.Join(home, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	res, err := MemoryJoin("soul.demarkus.io", "tok", true, repo)
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
	if b, _ := IsCatalogMemory("soul"); !b {
		t.Error("soul should be a catalog memory")
	}
	// collision: a different host under the same slug is rejected
	if _, err := MemoryJoin("soul.other.net", "t2", false, ""); err == nil {
		t.Error("expected slug collision error for a different host")
	}
	// re-join same host is fine (idempotent upsert)
	if _, err := MemoryJoin("soul.demarkus.io", "tok2", true, ""); err != nil {
		t.Errorf("re-join same host should succeed: %v", err)
	}
	// reserved slug rejected
	if _, err := MemoryJoin("demarkus-memory.example.com", "", false, ""); err == nil {
		t.Error("expected reserved-slug rejection")
	}
}

func TestMemoryJoinRollsBackBindingFailure(t *testing.T) {
	home := setupHome(t)
	repo := filepath.Join(home, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	failWritesTo(t, "project-souls")

	if _, err := MemoryJoin("soul.demarkus.io", "secret", false, repo); err == nil {
		t.Fatal("MemoryJoin succeeded despite binding failure")
	}
	for _, name := range []string{"souls", "project-souls", "soul-soul.token"} {
		path := filepath.Join(home, ".demarkus", name)
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("partial state remains at %s: %v", path, err)
		}
	}
}

func TestMemoryJoinRestoresExistingStateOnBindingFailure(t *testing.T) {
	home := setupHome(t)
	repo := filepath.Join(home, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := MemoryJoin("soul.demarkus.io", "old-token", false, repo); err != nil {
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
	if _, err := MemoryJoin("soul.demarkus.io", "new-token", true, repo); err == nil {
		t.Fatal("MemoryJoin succeeded despite binding failure")
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
	home := setupHome(t)
	if _, err := MemoryJoin("soul.demarkus.io", "old-token", false, ""); err != nil {
		t.Fatal(err)
	}
	tokenPath := filepath.Join(home, ".demarkus", "soul-soul.token")
	if _, err := MemoryJoin("soul.demarkus.io", "", false, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(tokenPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("managed token remains after tokenless rejoin: %v", err)
	}
	row, ok, err := RemoteMemoryRow("soul")
	if err != nil || !ok || row.TokenFile != "-" {
		t.Fatalf("tokenless row = %+v, ok=%v, err=%v", row, ok, err)
	}
}

func TestMemoryJoinWithoutTokenRemovesOrphanManagedToken(t *testing.T) {
	home := setupHome(t)
	tokenPath := filepath.Join(home, ".demarkus", "soul-soul.token")
	if err := os.MkdirAll(filepath.Dir(tokenPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokenPath, []byte("orphan-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := MemoryJoin("soul.demarkus.io", "", false, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(tokenPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("orphan managed token remains after tokenless join: %v", err)
	}
}

func TestMemoryJoinWithoutTokenPreservesExternalTokenReference(t *testing.T) {
	home := setupHome(t)
	tokenPath := filepath.Join(home, "external.token")
	if err := os.WriteFile(tokenPath, []byte("external-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := MemoryRegister("soul", "mark://soul.demarkus.io", false, tokenPath); err != nil {
		t.Fatal(err)
	}
	if _, err := MemoryJoin("soul.demarkus.io", "", false, ""); err == nil || !strings.Contains(err.Error(), "externally managed token file") {
		t.Fatalf("tokenless rejoin error = %v", err)
	}
	row, ok, err := RemoteMemoryRow("soul")
	if err != nil || !ok || row.TokenFile != tokenPath {
		t.Fatalf("external token row = %+v, ok=%v, err=%v", row, ok, err)
	}
	body, err := os.ReadFile(tokenPath)
	if err != nil || string(body) != "external-token" {
		t.Fatalf("external token changed: body=%q err=%v", body, err)
	}
}

func TestMemoryJoinWithTokenReportsReplacedExternalTokenReference(t *testing.T) {
	home := setupHome(t)
	externalPath := filepath.Join(home, "external.token")
	if err := os.WriteFile(externalPath, []byte("external-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := MemoryRegister("soul", "mark://soul.demarkus.io", false, externalPath); err != nil {
		t.Fatal(err)
	}
	warning, joinErr := captureStderr(t, func() error {
		_, err := MemoryJoin("soul.demarkus.io", "managed-token", false, "")
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
	home := setupHome(t)
	externalPath := filepath.Join(home, "external.token")
	if err := os.WriteFile(externalPath, []byte("external-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := MemoryRegister("soul", "mark://soul.demarkus.io", false, externalPath); err != nil {
		t.Fatal(err)
	}
	failWritesTo(t, "project-souls")
	warning, joinErr := captureStderr(t, func() error {
		_, err := MemoryJoin("soul.demarkus.io", "managed-token", false, filepath.Join(home, "repo"))
		return err
	})
	if joinErr == nil {
		t.Fatal("MemoryJoin succeeded despite binding failure")
	}
	if warning != "" {
		t.Fatalf("warning after rollback = %q", warning)
	}
	row, ok, err := RemoteMemoryRow("soul")
	if err != nil || !ok || row.TokenFile != externalPath {
		t.Fatalf("external token row after rollback = %+v, ok=%v, err=%v", row, ok, err)
	}
}

func TestMemoryJoinRestoresDeletedTokenOnLaterFailure(t *testing.T) {
	home := setupHome(t)
	if _, err := MemoryJoin("soul.demarkus.io", "old-token", false, ""); err != nil {
		t.Fatal(err)
	}
	tokenPath := filepath.Join(home, ".demarkus", "soul-soul.token")
	failWritesTo(t, "project-souls")
	if _, err := MemoryJoin("soul.demarkus.io", "", false, filepath.Join(home, "repo")); err == nil {
		t.Fatal("MemoryJoin succeeded despite binding failure")
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
	body := "strictness: block\nrequire_tags: category, team\nrequire_fields: type, authors\n" +
		"strictness: ask\nrequire_tags: ignored\nrequire_fields: ignored\n"
	if err := PolicyMirror("acme", body); err != nil {
		t.Fatal(err)
	}
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join(home, ".demarkus", name))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	if read("plugin-knowledge.strictness.acme") != "block\n" {
		t.Errorf("strictness mirror wrong: %q", read("plugin-knowledge.strictness.acme"))
	}
	if read("plugin-knowledge.require-tags.acme") != "category team\n" {
		t.Errorf("require_tags mirror wrong: %q", read("plugin-knowledge.require-tags.acme"))
	}
	if read("plugin-knowledge.require-fields.acme") != "type authors\n" {
		t.Errorf("require_fields mirror wrong: %q", read("plugin-knowledge.require-fields.acme"))
	}
	if snapshot := read("plugin-knowledge.policy.acme"); !strings.Contains(snapshot, "strictness: block\n") ||
		!strings.Contains(snapshot, "require_tags: category team\n") ||
		!strings.Contains(snapshot, "require_fields: type authors\n") {
		t.Errorf("atomic policy snapshot wrong: %q", snapshot)
	}
	// a knob absent from a later policy clears its file
	if err := PolicyMirror("acme", "strictness: warn\n"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"plugin-knowledge.require-tags.acme", "plugin-knowledge.require-fields.acme"} {
		if _, err := os.Stat(filepath.Join(home, ".demarkus", name)); !os.IsNotExist(err) {
			t.Errorf("%s should be cleared when absent from policy", name)
		}
	}
	if err := PolicyMirror("acme", "strictness: invalid\nrequire_tags: category\nrequire_fields: type\n"); err == nil {
		t.Fatal("invalid policy mirror succeeded")
	}
	if read("plugin-knowledge.strictness.acme") != "warn\n" {
		t.Errorf("failed mirror changed strictness: %q", read("plugin-knowledge.strictness.acme"))
	}
	blankFirst := "strictness:\nstrictness: block\nrequire_tags:\nrequire_tags: category\n" +
		"require_fields:\nrequire_fields: type\n"
	if err := PolicyMirror("acme", blankFirst); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		"plugin-knowledge.strictness.acme",
		"plugin-knowledge.require-tags.acme",
		"plugin-knowledge.require-fields.acme",
	} {
		if _, err := os.Stat(filepath.Join(home, ".demarkus", name)); !os.IsNotExist(err) {
			t.Errorf("%s should be cleared by a blank first directive", name)
		}
	}
	if snapshot := read("plugin-knowledge.policy.acme"); !strings.Contains(snapshot, "Atomic mirrored publish policy") {
		t.Errorf("empty atomic snapshot missing: %q", snapshot)
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

func TestPolicyMirrorRollsBack(t *testing.T) {
	setupHome(t)
	if err := KnowledgeRegister("acme"); err != nil {
		t.Fatal(err)
	}
	if err := PolicyMirror("acme", "strictness: block\nrequire_tags: category\nrequire_fields: type\n"); err != nil {
		t.Fatal(err)
	}
	failWritesTo(t, "plugin-knowledge.require-tags.acme")
	if err := PolicyMirror("acme", "strictness: ask\nrequire_tags: team\nrequire_fields: owner\n"); err == nil {
		t.Fatal("policy mirror succeeded despite injected failure")
	}
	policy, err := config.KnowledgePolicy("acme")
	if err != nil {
		t.Fatal(err)
	}
	if policy.Strictness != "block" || strings.Join(policy.RequiredTagAxes, ",") != "category" ||
		strings.Join(policy.RequiredFields, ",") != "type" {
		t.Fatalf("policy after rollback = %#v", policy)
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
	if err := PolicyMirror("acme", "strictness: block\nrequire_tags: category\nrequire_fields: type\n"); err != nil {
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
	for _, name := range []string{
		"plugin-knowledge.policy.acme",
		"plugin-knowledge.strictness.acme",
		"plugin-knowledge.require-tags.acme",
		"plugin-knowledge.require-fields.acme",
	} {
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
	home := setupHome(t)
	if _, err := PromoteTargetAdd("acme", "/shared", "Acme shared"); err != nil {
		t.Fatal(err)
	}
	if _, err := PromoteTargetAdd("acme", "bad", ""); err == nil {
		t.Error("path not starting with / should error")
	}
	for _, unsafe := range []string{"/docs/../secret", "/docs/./internal", `/docs\secret`, "/safe\nother", "/safe\rother"} {
		if _, err := PromoteTargetAdd("acme", unsafe, ""); err == nil {
			t.Errorf("unsafe path %q should error", unsafe)
		}
	}
	for _, label := range []string{"bad\tlabel", "bad\rlabel", "bad\nlabel", "bad\x00label", " leading", "trailing "} {
		if _, err := PromoteTargetAdd("acme", "/labeled", label); err == nil {
			t.Errorf("unsafe label %q should error", label)
		}
	}
	canonical, err := PromoteTargetAdd("acme", "/shared//nested/", "Nested")
	if err != nil {
		t.Fatal(err)
	}
	if canonical != "/shared/nested" {
		t.Fatalf("canonical path = %q", canonical)
	}
	rows, err := PromoteTargetList()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0] != "acme /shared Acme shared" || rows[1] != "acme /shared/nested Nested" {
		t.Fatalf("promote target rows = %v", rows)
	}
	registryPath := filepath.Join(home, ".demarkus", "promote-targets")
	legacyRows := strings.Join(append(rows, "legacy /legacy//nested/ Legacy label"), "\n") + "\n"
	if err := os.WriteFile(registryPath, []byte(legacyRows), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := PromoteTargetAdd("legacy", "/legacy/nested", "Ignored replacement"); err != nil {
		t.Fatal(err)
	}
	rows, err = PromoteTargetList()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 || rows[2] != "legacy /legacy/nested Legacy label" {
		t.Fatalf("promote target row wrong: %v", rows)
	}
}

func TestMemoryJoinURLWithFragment(t *testing.T) {
	home := setupHome(t)
	repo := filepath.Join(home, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}

	res, err := MemoryJoin("mark://kb.example.com#token=fragtok", "", false, repo)
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

	row, ok, err := RemoteMemoryRow("kb")
	if err != nil || !ok {
		t.Fatalf("RemoteMemoryRow: %v ok=%v", err, ok)
	}
	if row.Host != "mark://kb.example.com" || row.Insecure {
		t.Fatalf("row round trip: %+v", row)
	}

	// Conflicting explicit --token and fragment token is rejected.
	if _, err := MemoryJoin("mark://kb2.example.com#token=a", "b", false, ""); err == nil {
		t.Error("expected conflict error for --token + fragment token")
	}
	// Bad fragment key fails loudly.
	if _, err := MemoryJoin("mark://kb3.example.com#tokn=a", "", false, ""); err == nil {
		t.Error("expected error for unknown fragment key")
	}
	// Bracketed IPv6 would derive a garbage slug; the join-URL path rejects it.
	if _, err := MemoryJoin("mark://[2001:db8::1]:6309#token=a", "", false, ""); err == nil {
		t.Error("expected error for bracketed IPv6 join URL")
	}
	// Malformed hosts on the legacy (no-fragment) path fail loudly too.
	for _, bad := range []string{
		"mark://kb.example.com?x=1",
		"mark://user@kb.example.com",
		"mark://kb.example.com/docs",
	} {
		if _, err := MemoryJoin(bad, "t", false, ""); err == nil {
			t.Errorf("expected error for malformed host %q", bad)
		}
	}
}

func TestMemoryJoinBroker(t *testing.T) {
	t.Setenv("DEMARKUS_KNOWLEDGE_JOIN_ALLOW_HTTP", "1")
	home := t.TempDir()
	t.Setenv("HOME", home)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/.well-known/oauth-protected-resource" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ts.Close()

	res, err := MemoryJoin(ts.URL, "", false, "")
	if err != nil {
		t.Fatalf("MemoryJoin(broker): %v", err)
	}
	if !res.Broker || res.McpURL != res.Host+"/mcp" {
		t.Errorf("broker result = %+v", res)
	}
	if res.TokenFile != "-" {
		t.Errorf("broker memory must not reference a token file, got %q", res.TokenFile)
	}
	// Catalog row lands in SOULS (destination gate + binding apply).
	row, ok, err := RemoteMemoryRow(res.Slug)
	if err != nil || !ok {
		t.Fatalf("catalog row missing after broker join: ok=%v err=%v", ok, err)
	}
	if row.Host != res.Host {
		t.Errorf("catalog host = %q, want %q", row.Host, res.Host)
	}

	// A token alongside an HTTPS broker URL is rejected: OAuth owns auth.
	if _, err := MemoryJoin(ts.URL, "sekret", false, ""); err == nil {
		t.Error("broker join accepted a token")
	}
}

// Query and fragment components corrupt the appended discovery and
// /mcp paths; both are rejected before any request.
func TestValidateBrokerEndpointRejectsQueryAndFragment(t *testing.T) {
	t.Setenv("DEMARKUS_KNOWLEDGE_JOIN_ALLOW_HTTP", "1")
	for _, u := range []string{
		"http://broker.example?x=1",
		"http://broker.example/?",
		"http://broker.example/#x",
		// Empty fragment marker: Parse erases it but keeps the path
		// untrimmed, which would double the slash in appended paths.
		"http://broker.example/#",
	} {
		if _, err := ValidateBrokerEndpoint(u); err == nil || !strings.Contains(err.Error(), "query or fragment") {
			t.Errorf("ValidateBrokerEndpoint(%q) err = %v, want query/fragment rejection", u, err)
		}
	}
}

func TestValidateBrokerEndpointRejectsUserinfo(t *testing.T) {
	t.Setenv("DEMARKUS_KNOWLEDGE_JOIN_ALLOW_HTTP", "1")
	home := t.TempDir()
	t.Setenv("HOME", home)

	requests := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	withUser := strings.Replace(ts.URL, "http://", "http://user:password@", 1)
	if _, err := MemoryJoin(withUser, "", false, ""); err == nil || !strings.Contains(err.Error(), "userinfo") {
		t.Fatalf("userinfo URL err = %v, want userinfo rejection", err)
	}
	// The rejection must happen before any request: credentials in the
	// URL must never reach the wire as a Basic Authorization header.
	if requests != 0 {
		t.Fatalf("userinfo URL produced %d requests, want 0", requests)
	}
	// No catalog row may exist for the bogus "user" slug.
	if _, ok, err := RemoteMemoryRow("user"); err != nil || ok {
		t.Fatalf("catalog row after rejected join: ok=%v err=%v", ok, err)
	}
}

func TestMcpCursorHarnessPathAndShape(t *testing.T) {
	home := setupHome(t)
	if err := SetMcpHarness("cursor"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { McpHarness = "" })
	if err := McpAdd("mem", "/bin/x", []string{"mcp-serve"}); err != nil {
		t.Fatal(err)
	}
	if err := McpAddHTTP("kb", "https://b/mcp"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(home, ".cursor", "mcp.json"))
	if err != nil {
		t.Fatalf("cursor config not written: %v", err)
	}
	if strings.Contains(string(b), "\"auth\"") {
		t.Fatalf("cursor HTTP entry must not carry an auth key: %s", b)
	}
	if !strings.Contains(string(b), "\"url\": \"https://b/mcp\"") {
		t.Fatalf("cursor HTTP entry missing url: %s", b)
	}
	if _, err := os.Stat(filepath.Join(home, ".config", "mcp", "mcp.json")); !os.IsNotExist(err) {
		t.Fatalf("pi config must be untouched under the cursor harness: %v", err)
	}
	if err := SetMcpHarness("vim"); err == nil {
		t.Fatal("unknown harness should be rejected")
	}
}
