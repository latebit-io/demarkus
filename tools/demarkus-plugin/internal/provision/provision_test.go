package provision

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/token"
)

// TestDetectPlatform checks the OS_arch mapping produces a sane release suffix on
// the host (we can't easily override runtime.GOOS/GOARCH, so just assert shape).
func TestDetectPlatform(t *testing.T) {
	plat, err := detectPlatform()
	if err != nil {
		t.Fatalf("detectPlatform: %v", err)
	}
	parts := strings.Split(plat, "_")
	if len(parts) != 2 {
		t.Fatalf("platform %q is not OS_arch", plat)
	}
	switch parts[0] {
	case "darwin", "linux":
	default:
		t.Fatalf("unexpected OS %q", parts[0])
	}
	switch parts[1] {
	case "arm64", "amd64":
	default:
		t.Fatalf("unexpected arch %q", parts[1])
	}
}

func TestConfigRoundTrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	memory := filepath.Join(t.TempDir(), "memory dir") // a space to exercise quoting
	if err := saveConfig(memory, 16312, "isolated", ""); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg == nil {
		t.Fatal("loadConfig returned nil after save")
	}
	if cfg.MemoryDir != memory {
		t.Errorf("MemoryDir = %q, want %q", cfg.MemoryDir, memory)
	}
	if cfg.Port != "16312" {
		t.Errorf("Port = %q, want 16312", cfg.Port)
	}
	if cfg.Mode != "isolated" {
		t.Errorf("Mode = %q, want isolated", cfg.Mode)
	}
}

func TestConfigPlainPathRoundTrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	memory := "/Users/x/.demarkus/memory"
	if err := saveConfig(memory, 6310, "default", ""); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg == nil || cfg.MemoryDir != memory || cfg.Port != "6310" || cfg.Mode != "default" {
		t.Fatalf("round-trip mismatch: %+v", cfg)
	}
}

func TestLoadConfigAbsent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig on absent: %v", err)
	}
	if cfg != nil {
		t.Fatalf("expected nil config when absent, got %+v", cfg)
	}
}

func TestPortIsFreeAndFindFreePort(t *testing.T) {
	// Bind a UDP port ourselves, then assert portIsFree reports it taken and
	// find_free_port skips it.
	conn, err := net.ListenPacket("udp", ":0")
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	defer func() { _ = conn.Close() }()
	taken := conn.LocalAddr().(*net.UDPAddr).Port

	if portIsFree(taken) {
		t.Errorf("port %d is bound but portIsFree returned true", taken)
	}

	// A high port we did not bind should read free (best effort; if the host has
	// it taken the test is still valid — just pick one likely free).
	free := taken + 1
	if free > 65000 {
		free = taken - 1
	}
	if !portIsFree(free) {
		t.Skipf("probe port %d unexpectedly busy on this host; skipping free-path assertion", free)
	}

	got, err := findFreePortFrom(free)
	if err != nil {
		t.Fatalf("findFreePortFrom: %v", err)
	}
	if got < free || got > free+199 {
		t.Errorf("findFreePortFrom(%d) = %d, out of range", free, got)
	}
}

func TestPidIsServerAtRootRejectsNonsense(t *testing.T) {
	// PID 0 and a clearly-dead high PID are never our server.
	if pidIsServerAtRoot(0, "/whatever") {
		t.Error("pid 0 should never match")
	}
	if pidIsServerAtRoot(-5, "/whatever") {
		t.Error("negative pid should never match")
	}
	// Our own test process is alive but is not demarkus-server.
	if pidIsServerAtRoot(os.Getpid(), "/whatever") {
		t.Error("the test process is not a demarkus-server at any root")
	}
}

func TestArgsRootMatches(t *testing.T) {
	cases := []struct {
		args, target string
		want         bool
	}{
		{" demarkus-server -root /home/x/memory -port 6310", "/home/x/memory", true},
		{" demarkus-server -root=/home/x/memory -port 6310", "/home/x/memory", true},
		{" demarkus-server -port 6310 -root /home/x/memory", "/home/x/memory", true},   // at end
		{" demarkus-server -port 6310 -root=/home/x/memory", "/home/x/memory", true},   // at end, = form
		{" demarkus-server -root /home/x/memory2 -port 6310", "/home/x/memory", false}, // prefix only
		{" demarkus-server -root /other -port 6310", "/home/x/memory", false},
		{" demarkus-server --root /home/x/memory -port 6310", "/home/x/memory", true}, // long form
		{" demarkus-server --root=/home/x/memory", "/home/x/memory", true},            // long form, = at end
	}
	for _, c := range cases {
		if got := argsRootMatches(c.args, c.target); got != c.want {
			t.Errorf("argsRootMatches(%q, %q) = %v, want %v", c.args, c.target, got, c.want)
		}
	}
}

func TestVersionDriftFormatting(t *testing.T) {
	old := Version
	t.Cleanup(func() { Version = old })

	// A dev build (no real ldflags version) falls back to fallbackToolsVersion.
	Version = "dev"
	want := "server=" + serverVersion + ",client=" + clientVersion + ",tools=" + fallbackToolsVersion
	if got := desiredVersions(); got != want {
		t.Errorf("desiredVersions() [dev] = %q, want %q", got, want)
	}

	// A real release derives the tools pin from the binary's own version.
	Version = "9.9.9"
	want = "server=" + serverVersion + ",client=" + clientVersion + ",tools=9.9.9"
	if got := desiredVersions(); got != want {
		t.Errorf("desiredVersions() [release] = %q, want %q", got, want)
	}

	// With no binaries installed, installedVersions reports all-empty fields,
	// which never equals desired → ensureBinaries would re-download. (No plugin
	// field: provision doesn't manage demarkus-plugin — bootstrap.sh does.)
	t.Setenv("HOME", t.TempDir())
	got := installedVersions()
	if got != "server=,client=,tools=" {
		t.Errorf("installedVersions() with no bins = %q", got)
	}
	if got == desiredVersions() {
		t.Error("empty installed must not equal desired")
	}
}

func TestSha256Verify(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "demarkus-server_0.18.0_darwin_arm64.tar.gz")
	payload := []byte("not really a tarball, just bytes")
	if err := os.WriteFile(archive, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(payload)
	hexsum := hex.EncodeToString(sum[:])

	checksums := filepath.Join(dir, "checksums.txt")
	body := hexsum + "  " + filepath.Base(archive) + "\n" +
		"deadbeef  some-other-file.tar.gz\n"
	if err := os.WriteFile(checksums, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := sha256Verify(checksums, archive); err != nil {
		t.Fatalf("sha256Verify (valid): %v", err)
	}

	// Tamper with the archive — verification must fail.
	if err := os.WriteFile(archive, append(payload, '!'), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := sha256Verify(checksums, archive); err == nil {
		t.Fatal("sha256Verify accepted a tampered archive")
	}

	// Missing checksum entry → error.
	noEntry := filepath.Join(dir, "no-entry.tar.gz")
	if err := os.WriteFile(noEntry, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := sha256Verify(checksums, noEntry); err == nil {
		t.Fatal("sha256Verify accepted a file with no checksum entry")
	}
}

func TestShellQuoteRoundTrip(t *testing.T) {
	// Quote values then save/load to confirm config.unquoteShell reverses it.
	t.Setenv("HOME", t.TempDir())
	for _, memory := range []string{
		"/plain/path",
		"/path with space",
		"/path/with'quote",
	} {
		if err := saveConfig(memory, 6310, "default", ""); err != nil {
			t.Fatalf("saveConfig(%q): %v", memory, err)
		}
		cfg, err := loadConfig()
		if err != nil || cfg == nil {
			t.Fatalf("loadConfig(%q): %v", memory, err)
		}
		if cfg.MemoryDir != memory {
			t.Errorf("round-trip %q -> %q", memory, cfg.MemoryDir)
		}
	}
}

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

func (m *mockSeedClient) Fetch(_, _, _ string) (fetch.Result, error) {
	if m.fetchErr != nil {
		return fetch.Result{}, m.fetchErr
	}
	return fetch.Result{Response: protocol.Response{Status: m.fetchStatus}}, nil
}

func (m *mockSeedClient) Publish(_, path, body, _ string, expectedVersion int, _ map[string]string) (fetch.Result, error) {
	m.published = append(m.published, publishCall{path, body, expectedVersion})
	st := m.publishStatus
	if st == "" {
		st = protocol.StatusCreated
	}
	return fetch.Result{Response: protocol.Response{Status: st}}, nil
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
			seedDoc(tt.mock, "localhost:1", "tok", "index.md", nil)
			if len(tt.mock.published) != 0 {
				t.Errorf("published %d docs, want 0", len(tt.mock.published))
			}
		})
	}

	// Not found: publishes the embedded seed, create-only.
	m := &mockSeedClient{fetchStatus: protocol.StatusNotFound}
	seedDoc(m, "localhost:1", "tok", "index.md", nil)
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
	cleanupLegacyTemplate(dir)
	if fileExists(filepath.Join(dir, "project-template.md")) {
		t.Error("pristine flat template not deleted")
	}

	// Customized: left in place for the store's flat-file migration.
	custom := []byte("# My layout\n\ncustomized\n")
	dir = write(t, custom)
	cleanupLegacyTemplate(dir)
	got, err := os.ReadFile(filepath.Join(dir, "project-template.md"))
	if err != nil || !bytes.Equal(got, custom) {
		t.Errorf("customized template altered: %q (err %v)", got, err)
	}

	// Absent: nothing happens.
	cleanupLegacyTemplate(t.TempDir())
}

func TestScopeStale(t *testing.T) {
	tests := []struct {
		name  string
		paths []string
		stale bool
	}{
		{"legacy single-segment scope", []string{"/*"}, true},
		{"recursive scope", []string{"/**"}, false},
		{"both patterns present", []string{"/*", "/**"}, false},
		{"unrelated scope", []string{"/docs/*"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := scopeStale(&token.Entry{Paths: tt.paths}); got != tt.stale {
				t.Errorf("scopeStale(%v) = %v, want %v", tt.paths, got, tt.stale)
			}
		})
	}
}

func TestPluginEntry(t *testing.T) {
	dir := t.TempDir()
	pluginStanza := token.FormatEntry(tokenLabel, &token.Entry{Hash: "abc", Paths: []string{"/*"}, Operations: []string{"publish"}})
	otherStanza := token.FormatEntry("other", &token.Entry{Hash: "x", Paths: []string{"/*"}, Operations: []string{"publish"}})

	tests := []struct {
		name    string
		body    string // empty means don't write the file
		present bool
		wantErr bool
	}{
		{"plugin entry present", pluginStanza, true, false},
		{"other label only", otherStanza, false, false},
		{"unparseable file", "not toml [[[", false, true},
		{"missing file", "", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := filepath.Join(dir, strings.ReplaceAll(tt.name, " ", "-")+".toml")
			if tt.body != "" {
				if err := os.WriteFile(p, []byte(tt.body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			entry, present, err := pluginEntry(p)
			if (err != nil) != tt.wantErr {
				t.Fatalf("pluginEntry err = %v, wantErr %v", err, tt.wantErr)
			}
			if present != tt.present {
				t.Errorf("present = %v, want %v", present, tt.present)
			}
			if present && len(entry.Paths) == 0 {
				t.Error("present entry has no paths")
			}
		})
	}
}

// writeFakeTokenBin installs a fake demarkus-token under $HOME/.demarkus/bin
// that mirrors revoke (truncate; fixtures hold only the plugin's stanza, and
// FAKE_REVOKE_FAIL=1 forces failure) and generate (real sha256- hash so the
// gate's verification runs). A non-empty argsLog gets each invocation's argv.
func writeFakeTokenBin(t *testing.T, home, argsLog string) {
	t.Helper()
	binDir := filepath.Join(home, ".demarkus", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	logLine := ""
	if argsLog != "" {
		logLine = `echo "$@" >> "` + argsLog + `"`
	}
	fake := `#!/bin/bash
` + logLine + `
cmd="$1"; shift
label=""; paths=""; tokens=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    -label) label="$2"; shift 2 ;;
    -paths) paths="$2"; shift 2 ;;
    -tokens) tokens="$2"; shift 2 ;;
    *) shift ;;
  esac
done
if [[ "$cmd" == "revoke" ]]; then
  [[ -n "$FAKE_REVOKE_FAIL" ]] && exit 1
  : > "$tokens"
fi
if [[ "$cmd" == "generate" ]]; then
  raw="raw-token-value"
  # sha256sum on minimal Linux images, shasum on macOS; neither alone is portable.
  if command -v sha256sum >/dev/null 2>&1; then
    sum=$(printf %s "$raw" | sha256sum | cut -d' ' -f1)
  else
    sum=$(printf %s "$raw" | shasum -a 256 | cut -d' ' -f1)
  fi
  printf '\n[tokens.%s]\nhash = "sha256-%s"\npaths = ["%s"]\noperations = ["publish", "archive"]\n' "$label" "$sum" "$paths" >> "$tokens"
  echo "$raw"
fi
`
	if err := os.WriteFile(filepath.Join(binDir, "demarkus-token"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
}

// TestEnsureTokenEntryScope pins the minted scope to the recursive "/**" and
// the remint-on-stale-scope behavior, via a fake demarkus-token binary that
// logs its argv and mirrors generate's tokens.toml append.
func TestEnsureTokenEntryScope(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	argsLog := filepath.Join(home, "args.log")
	writeFakeTokenBin(t, home, argsLog)

	memory := filepath.Join(home, "root")
	if err := os.MkdirAll(memory, 0o755); err != nil {
		t.Fatal(err)
	}
	tokensTOML := filepath.Join(memory, "tokens.toml")
	readLog := func() string {
		t.Helper()
		b, err := os.ReadFile(argsLog)
		if errors.Is(err, os.ErrNotExist) {
			return ""
		}
		if err != nil {
			t.Fatalf("read args log: %v", err)
		}
		return string(b)
	}
	// phaseLog scopes assertions to the invocations one phase made.
	phaseLog := func(fn func()) string {
		t.Helper()
		before := len(readLog())
		fn()
		return readLog()[before:]
	}
	staleStanza := token.FormatEntry(tokenLabel, &token.Entry{Hash: "old", Paths: []string{"/*"}, Operations: []string{"publish"}})

	// Fresh install mints with the recursive scope.
	log := phaseLog(func() {
		if err := ensureTokenEntry(memory, tokensTOML); err != nil {
			t.Fatalf("fresh mint: %v", err)
		}
	})
	if !strings.Contains(log, "-paths /**") {
		t.Fatalf("fresh mint did not use /**; argv:\n%s", log)
	}

	// Provisioned-and-current is a no-op: no further binary invocations.
	log = phaseLog(func() {
		if err := ensureTokenEntry(memory, tokensTOML); err != nil {
			t.Fatalf("idempotent rerun: %v", err)
		}
	})
	if log != "" {
		t.Fatalf("idempotent rerun invoked demarkus-token; argv:\n%s", log)
	}

	// An old install with the single-segment "/*" scope is revoked and reminted.
	if err := os.WriteFile(tokensTOML, []byte(staleStanza), 0o600); err != nil {
		t.Fatal(err)
	}
	log = phaseLog(func() {
		if err := ensureTokenEntry(memory, tokensTOML); err != nil {
			t.Fatalf("stale-scope remint: %v", err)
		}
	})
	if !strings.Contains(log, "revoke -label "+tokenLabel) {
		t.Errorf("stale scope did not revoke; argv:\n%s", log)
	}
	if !strings.Contains(log, "-paths /**") {
		t.Errorf("stale scope did not remint with /**; argv:\n%s", log)
	}
	f, err := token.ReadFile(tokensTOML)
	if err != nil {
		t.Fatalf("reminted tokens.toml unparseable: %v", err)
	}
	got, ok := f.Tokens[tokenLabel]
	if !ok {
		t.Fatalf("reminted tokens.toml lacks %s entry", tokenLabel)
	}
	if len(got.Paths) != 1 || got.Paths[0] != "/**" {
		t.Errorf("reminted entry paths = %v, want [/**]", got.Paths)
	}

	// A revoke failure surfaces instead of proceeding to a colliding generate.
	if err := os.WriteFile(tokensTOML, []byte(staleStanza), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_REVOKE_FAIL", "1")
	if err := ensureTokenEntry(memory, tokensTOML); err == nil {
		t.Error("revoke failure did not surface an error")
	}
	t.Setenv("FAKE_REVOKE_FAIL", "")

	// A tokens.toml we cannot parse fails provisioning rather than being
	// silently treated as current (or clobbered).
	malformed := "[tokens." + tokenLabel + "]\nnot toml [[["
	if err := os.WriteFile(tokensTOML, []byte(malformed), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ensureTokenEntry(memory, tokensTOML); err == nil {
		t.Error("malformed tokens.toml did not surface an error")
	}

	// A raw token that no longer matches the entry's hash (leftovers of a
	// failed run) fails the gate and is reminted.
	if err := os.Remove(tokensTOML); err != nil { // clear the malformed file
		t.Fatal(err)
	}
	if err := ensureTokenEntry(memory, tokensTOML); err != nil {
		t.Fatalf("re-mint after malformed: %v", err)
	}
	tokenFile, err := pluginToken()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokenFile, []byte("not-the-minted-raw"), 0o600); err != nil {
		t.Fatal(err)
	}
	log = phaseLog(func() {
		if err := ensureTokenEntry(memory, tokensTOML); err != nil {
			t.Fatalf("mismatched-raw remint: %v", err)
		}
	})
	if !strings.Contains(log, "-paths /**") {
		t.Errorf("mismatched raw token did not trigger a remint; argv:\n%s", log)
	}
}

// TestEnsureTokenEntryReload covers the reload branch via the test seams: a
// failed reload aborts before the token file is committed, and the next run
// re-enters the mint path and re-signals.
func TestEnsureTokenEntryReload(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeFakeTokenBin(t, home, "")

	memory := filepath.Join(home, "root")
	if err := os.MkdirAll(memory, 0o755); err != nil {
		t.Fatal(err)
	}
	tokensTOML := filepath.Join(memory, "tokens.toml")
	tokenFile, err := pluginToken()
	if err != nil {
		t.Fatal(err)
	}

	origFind, origReload := findServerAtRoot, reloadServer
	t.Cleanup(func() { findServerAtRoot, reloadServer = origFind, origReload })
	findServerAtRoot = func(string) int { return 4242 }
	reloads := 0
	reloadServer = func(int) error { reloads++; return errors.New("injected reload failure") }

	if err := ensureTokenEntry(memory, tokensTOML); err == nil {
		t.Fatal("failed reload did not surface an error")
	}
	if reloads != 1 {
		t.Fatalf("reloads = %d, want 1", reloads)
	}
	if fileExists(tokenFile) {
		t.Error("token file committed despite failed reload; gate would skip the retry")
	}

	// The next run retries: mint again, reload succeeds, token file commits.
	reloadServer = func(int) error { reloads++; return nil }
	if err := ensureTokenEntry(memory, tokensTOML); err != nil {
		t.Fatalf("retry after failed reload: %v", err)
	}
	if reloads != 2 {
		t.Fatalf("reloads = %d, want 2", reloads)
	}
	if !fileExists(tokenFile) {
		t.Error("token file missing after successful retry")
	}
}

// TestManagedTokensMigration pins the #289 follow-up: tokens.toml moves out of
// the content root on respawn; resolution prefers legacy until migrated.
func TestManagedTokensMigration(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	memory := filepath.Join(home, "memory")
	if err := os.MkdirAll(memory, 0o755); err != nil {
		t.Fatal(err)
	}

	newPath, err := managedTokensPath(memory)
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(newPath, memory+string(os.PathSeparator)) {
		t.Fatalf("managed tokens path %s is inside the memory root", newPath)
	}

	// Fresh install: nothing on disk resolves to the new path.
	if got, err := tokensPathFor(memory); err != nil || got != newPath {
		t.Fatalf("fresh tokensPathFor = %q, %v; want %q", got, err, newPath)
	}

	// A legacy file wins resolution until migrated: the running server's
	// -tokens flag still points there.
	legacy := filepath.Join(memory, "tokens.toml")
	if err := os.WriteFile(legacy, []byte("[tokens]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := tokensPathFor(memory); err != nil || got != legacy {
		t.Fatalf("unmigrated tokensPathFor = %q, %v; want %q", got, err, legacy)
	}

	// Migration moves the file, preserving content; resolution flips.
	migrated, err := migrateManagedTokens(memory)
	if err != nil {
		t.Fatal(err)
	}
	if migrated != newPath {
		t.Fatalf("migrateManagedTokens = %q, want %q", migrated, newPath)
	}
	data, err := os.ReadFile(newPath)
	if err != nil || string(data) != "[tokens]\n" {
		t.Fatalf("migrated content = %q, %v; want original body", data, err)
	}
	if fileExists(legacy) {
		t.Error("legacy tokens.toml still present after migration")
	}
	if got, err := tokensPathFor(memory); err != nil || got != newPath {
		t.Fatalf("post-migration tokensPathFor = %q, %v; want %q", got, err, newPath)
	}

	// Idempotent rerun leaves the migrated file alone.
	if again, err := migrateManagedTokens(memory); err != nil || again != newPath {
		t.Fatalf("rerun migrateManagedTokens = %q, %v; want %q", again, err, newPath)
	}

	// A leftover legacy file (failed removal after a prior copy) is cleaned up
	// on the next run without touching the migrated file.
	if err := os.WriteFile(legacy, []byte("leftover"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := migrateManagedTokens(memory); err != nil || got != newPath {
		t.Fatalf("leftover-cleanup migrateManagedTokens = %q, %v; want %q", got, err, newPath)
	}
	if fileExists(legacy) {
		t.Error("leftover legacy tokens.toml not removed on rerun")
	}
	if data, err := os.ReadFile(newPath); err != nil || string(data) != "[tokens]\n" {
		t.Fatalf("migrated file changed by leftover cleanup: %q, %v", data, err)
	}
}

// TestSignalReload covers the nil paths: live owned child, then vanished pid.
// The hard-error branch (EPERM) needs a foreign-uid process; not unit-reachable.
func TestSignalReload(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// Kill on an already-reaped child returns ErrProcessDone; safe to ignore.
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	pid := cmd.Process.Pid

	if err := signalReload(pid); err != nil {
		t.Fatalf("signalReload(live child) = %v", err)
	}
	// The child must die to our SIGHUP, not run out its sleep.
	err := cmd.Wait()
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("child exit was not an ExitError: %v", err)
	}
	if sig := ee.ProcessState.Sys().(syscall.WaitStatus).Signal(); sig != syscall.SIGHUP {
		t.Fatalf("child terminated by %v, want SIGHUP", sig)
	}

	if err := signalReload(pid); err != nil {
		t.Errorf("signalReload(dead pid) = %v, want nil", err)
	}

	// Non-positive pids are rejected before any signal: kill(0)/kill(-1)
	// broadcast to the process group / all reachable processes.
	for _, bad := range []int{0, -1} {
		if err := signalReload(bad); err == nil {
			t.Errorf("signalReload(%d) = nil, want error", bad)
		}
	}
}

func TestFlagValue(t *testing.T) {
	cases := []struct {
		args string
		re   *regexp.Regexp
		want string
	}{
		{" demarkus-server -root /x -tokens /home/x/tokens.toml", tokensFlagRe, "/home/x/tokens.toml"},
		{" demarkus-server -tokens=/etc/demarkus/tokens.toml -root /x", tokensFlagRe, "/etc/demarkus/tokens.toml"},
		{" demarkus-server -root /x -port 6310", tokensFlagRe, ""},
		{" demarkus-server -root /x -port 6310", portFlagRe, "6310"},
		{" demarkus-server -root=/x -port 6310", rootFlagRe, "/x"},
	}
	for _, c := range cases {
		if got := flagValue(c.args, c.re); got != c.want {
			t.Errorf("flagValue(%q, %v) = %q, want %q", c.args, c.re, got, c.want)
		}
	}
}

// mockAppendClient cans the APPEND probe response for verifyTokenWith.
type mockAppendClient struct {
	status          string
	err             error
	path            string
	expectedVersion int
}

func (m *mockAppendClient) Append(_, path, _, _ string, expectedVersion int, _ map[string]string) (fetch.Result, error) {
	m.path = path
	m.expectedVersion = expectedVersion
	if m.err != nil {
		return fetch.Result{}, m.err
	}
	return fetch.Result{Response: protocol.Response{Status: m.status}}, nil
}

func TestVerifyTokenWith(t *testing.T) {
	cases := []struct {
		name    string
		mock    *mockAppendClient
		wantErr bool
	}{
		{"unauthorized fails", &mockAppendClient{status: protocol.StatusUnauthorized}, true},
		{"not-found passes", &mockAppendClient{status: protocol.StatusNotFound}, false},
		{"version conflict passes", &mockAppendClient{status: protocol.StatusConflict}, false},
		{"transport error surfaces", &mockAppendClient{err: errors.New("dial refused")}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := verifyTokenWith(c.mock, "localhost:6309", "tok")
			if (err != nil) != c.wantErr {
				t.Errorf("verifyTokenWith error = %v, wantErr %v", err, c.wantErr)
			}
			// Drift classification hangs on this: only a real unauthorized may
			// match the sentinel, transport errors must not.
			if gotUnauth := errors.Is(err, errUnauthorized); gotUnauth != (c.mock.status == protocol.StatusUnauthorized) {
				t.Errorf("errors.Is(err, errUnauthorized) = %v for status %q", gotUnauth, c.mock.status)
			}
			if c.mock.path != "/.demarkus-plugin-auth-probe.md" {
				t.Errorf("probe path = %q", c.mock.path)
			}
			// The probe must be non-mutating by construction: an expected
			// version no real document can hold.
			if c.mock.expectedVersion != probeExpectedVersion {
				t.Errorf("probe expected-version = %d, want %d", c.mock.expectedVersion, probeExpectedVersion)
			}
		})
	}
}

func TestTokensFromArgv(t *testing.T) {
	cases := []struct {
		name string
		argv []string
		want string
	}{
		{"separate value", []string{"demarkus-server", "-tokens", "/a/tokens.toml"}, "/a/tokens.toml"},
		{"equals form", []string{"demarkus-server", "--tokens=/b/tokens.toml"}, "/b/tokens.toml"},
		{"whitespace preserved", []string{"demarkus-server", "-tokens", "/My Tokens/tokens.toml"}, "/My Tokens/tokens.toml"},
		{"absent", []string{"demarkus-server", "-root", "/x"}, ""},
		{"trailing flag without value", []string{"demarkus-server", "-tokens"}, ""},
	}
	for _, c := range cases {
		if got := tokensFromArgv(c.argv); got != c.want {
			t.Errorf("%s: tokensFromArgv(%v) = %q, want %q", c.name, c.argv, got, c.want)
		}
	}
}

// TestSemverLess covers the ordering and the not-a-release cases where no
// ordering exists, which the drift probe must treat as "say nothing".
func TestSemverLess(t *testing.T) {
	tests := []struct {
		name     string
		a, b     string
		wantLess bool
		wantOK   bool
	}{
		{name: "patch behind", a: "0.35.0", b: "0.35.1", wantLess: true, wantOK: true},
		{name: "minor behind", a: "0.17.19", b: "0.35.0", wantLess: true, wantOK: true},
		{name: "major behind", a: "0.99.99", b: "1.0.0", wantLess: true, wantOK: true},
		{name: "equal", a: "0.35.0", b: "0.35.0", wantOK: true},
		{name: "ahead", a: "0.36.0", b: "0.35.0", wantOK: true},
		{name: "numeric not lexical", a: "0.9.0", b: "0.10.0", wantLess: true, wantOK: true},
		{name: "dev build", a: "dev", b: "0.35.0"},
		{name: "tagged build", a: "0.35.0-rc1", b: "0.35.0"},
		{name: "empty probe", a: "", b: "0.35.0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			less, ok := semverLess(tt.a, tt.b)
			if ok != tt.wantOK {
				t.Fatalf("semverLess(%q, %q) ok = %v, want %v", tt.a, tt.b, ok, tt.wantOK)
			}
			if less != tt.wantLess {
				t.Errorf("semverLess(%q, %q) less = %v, want %v", tt.a, tt.b, less, tt.wantLess)
			}
		})
	}
}

// TestBinaryVersionAt checks the path-taking split reports a version, and stays
// empty for the inputs the drift probe must not read a verdict into.
func TestBinaryVersionAt(t *testing.T) {
	dir := t.TempDir()

	good := filepath.Join(dir, "answers")
	if err := os.WriteFile(good, []byte("#!/bin/sh\necho 0.35.0\n"), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	if got := binaryVersionAt(good); got != "0.35.0" {
		t.Errorf("binaryVersionAt(executable) = %q, want %q", got, "0.35.0")
	}

	notExec := filepath.Join(dir, "not-exec")
	if err := os.WriteFile(notExec, []byte("#!/bin/sh\necho 0.35.0\n"), 0o644); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	if got := binaryVersionAt(notExec); got != "" {
		t.Errorf("binaryVersionAt(non-executable) = %q, want empty", got)
	}
	if got := binaryVersionAt(dir); got != "" {
		t.Errorf("binaryVersionAt(directory) = %q, want empty", got)
	}
	if got := binaryVersionAt(filepath.Join(dir, "absent")); got != "" {
		t.Errorf("binaryVersionAt(missing) = %q, want empty", got)
	}
}

// TestProcessStart pins the contract the drift probe relies on: a real start
// time for a live process, and ok=false rather than a zero time for a dead one.
func TestProcessStart(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	t.Cleanup(func() { stopProcess(t, cmd) })

	// Wait before the first lookup: an implementation that stamps the time of
	// inspection rather than of launch would pass without this, and would then
	// miss a binary replaced during the gap.
	const settle = 2 * time.Second
	time.Sleep(settle)

	started, ok := processStart(cmd.Process.Pid)
	if !ok {
		t.Fatalf("processStart(live pid) ok = false, want true")
	}
	if d := time.Since(started); d < settle-time.Second {
		t.Errorf("processStart(live pid) = %v, only %v ago; want the launch time, not the lookup time", started, d)
	}
	if d := time.Since(started); d > time.Minute {
		t.Errorf("processStart(live pid) = %v, %v ago; want within a minute", started, d)
	}

	// Kill it, then ask about a pid that is certainly gone.
	pid := cmd.Process.Pid
	stopProcess(t, cmd)
	if _, ok := processStart(pid); ok {
		t.Errorf("processStart(dead pid) ok = true, want false")
	}
}

// TestServerExecutable checks the resolver returns a runnable absolute path for
// a live process on whichever mechanism this platform provides.
func TestServerExecutable(t *testing.T) {
	// Launch by absolute path: ps comm= echoes how the process was invoked, so a
	// PATH-resolved name would test nothing the real adopted server does.
	sleepPath, err := exec.LookPath("sleep")
	if err != nil {
		t.Skipf("no sleep on PATH: %v", err)
	}
	cmd := exec.Command(sleepPath, "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	t.Cleanup(func() { stopProcess(t, cmd) })

	exe := serverExecutable(cmd.Process.Pid)
	if exe == "" {
		t.Skip("no executable-path mechanism on this platform")
	}
	if !filepath.IsAbs(exe) {
		t.Errorf("serverExecutable = %q, want an absolute path", exe)
	}
	if base := filepath.Base(exe); base != "sleep" {
		t.Errorf("serverExecutable basename = %q, want %q", base, "sleep")
	}
}

// TestReuseDriftWarningQuietOnHealthy guards the false-positive direction: a
// server at the pin whose binary predates it must say nothing, or every session
// start would nag.
func TestReuseDriftWarningQuietOnHealthy(t *testing.T) {
	t.Setenv("STUB_VERSION", serverVersion)
	pid := startStub(t, buildVersionStub(t))
	if w := reuseDriftWarning(pid); w != "" {
		t.Errorf("reuseDriftWarning(current server) = %q, want empty", w)
	}
}

// TestReuseDriftWarningUnreadableVersion covers the gap that let an unprobeable
// server read as healthy: no usable version is a lost check, not a clean one.
func TestReuseDriftWarningUnreadableVersion(t *testing.T) {
	t.Setenv("STUB_VERSION", "dev")
	exe := buildVersionStub(t)
	pid := startStub(t, exe)

	w := reuseDriftWarning(pid)
	if !strings.Contains(w, "cannot version-check") || !strings.Contains(w, "dev") {
		t.Errorf("reuseDriftWarning = %q, want a refusal naming the reported build", w)
	}
}

// TestReuseDriftWarningNoProcess checks an unresolvable pid stays silent rather
// than inventing a verdict.
func TestReuseDriftWarningNoProcess(t *testing.T) {
	if w := reuseDriftWarning(-1); w != "" {
		t.Errorf("reuseDriftWarning(bad pid) = %q, want empty", w)
	}
}

// TestBinaryVersionAtTimeout checks a binary that never answers is bounded
// rather than left to stall session start.
func TestBinaryVersionAtTimeout(t *testing.T) {
	hang := filepath.Join(t.TempDir(), "hangs")
	if err := os.WriteFile(hang, []byte("#!/bin/sh\nsleep 60\n"), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	start := time.Now()
	if got := binaryVersionAt(hang); got != "" {
		t.Errorf("binaryVersionAt(hanging) = %q, want empty", got)
	}
	if d := time.Since(start); d > versionProbeTimeout+time.Second {
		t.Errorf("binaryVersionAt(hanging) took %v, want within the %v budget", d, versionProbeTimeout)
	}
}

// TestBinaryVersionAtKillsForkedChild guards the probe against a --version that
// forks: cancelling has to take the whole process group, since a kill aimed at
// the direct child leaves the grandchild running.
func TestBinaryVersionAtKillsForkedChild(t *testing.T) {
	dir := t.TempDir()
	beat := filepath.Join(dir, "beat")
	forker := filepath.Join(dir, "forks")
	script := "#!/bin/sh\n" +
		"( while :; do sleep 1; printf x >> \"" + beat + "\"; done ) &\n" +
		"sleep 60\n"
	if err := os.WriteFile(forker, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	// Guarded: an ungrouped kill does not just leak the child, it never returns,
	// and a plain call would hang until the package timeout instead of failing.
	done := make(chan string, 1)
	go func() { done <- binaryVersionAt(forker) }()
	select {
	case got := <-done:
		if got != "" {
			t.Errorf("binaryVersionAt(forking) = %q, want empty", got)
		}
	case <-time.After(versionProbeTimeout + 2*time.Second):
		t.Fatal("binaryVersionAt never returned; the forked grandchild still holds the probe's stdout pipe")
	}
	beatSize := func() int64 {
		info, err := os.Stat(beat)
		if err != nil {
			return -1
		}
		return info.Size()
	}
	before := beatSize()
	if before < 0 {
		t.Skip("the forked child never wrote; nothing to prove about killing it")
	}
	time.Sleep(2 * time.Second)
	if after := beatSize(); after != before {
		t.Errorf("forked child outlived the cancelled probe (beat grew %d -> %d)", before, after)
	}
}

// stopProcess ends a helper process started by a test. Wait's error is the kill
// we just sent, so only a Kill failure is worth reporting.
func stopProcess(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		t.Errorf("kill test process %d: %v", cmd.Process.Pid, err)
	}
	_ = cmd.Wait() // reaps the process
}

// buildVersionStub compiles a real executable that answers --version from the
// environment. It has to be compiled: ps reports a shell script's interpreter
// rather than the script, so a script would never reach the probe under test.
func buildVersionStub(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module versionstub\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	src := "package main\n\n" +
		"import (\n\t\"fmt\"\n\t\"os\"\n\t\"time\"\n)\n\n" +
		"func main() {\n" +
		"\tif len(os.Args) > 1 {\n\t\tfmt.Println(os.Getenv(\"STUB_VERSION\"))\n\t\treturn\n\t}\n" +
		"\ttime.Sleep(5 * time.Minute)\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(src), 0o644); err != nil {
		t.Fatalf("write stub source: %v", err)
	}
	exe := filepath.Join(dir, "demarkus-server")
	build := exec.Command("go", "build", "-o", exe, ".")
	build.Dir = dir
	if out, err := build.CombinedOutput(); err != nil {
		t.Skipf("cannot build version stub: %v\n%s", err, out)
	}
	return exe
}

// startStub runs the stub so it stays alive and returns its pid.
func startStub(t *testing.T, exe string) int {
	t.Helper()
	cmd := exec.Command(exe)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start stub: %v", err)
	}
	t.Cleanup(func() { stopProcess(t, cmd) })
	return cmd.Process.Pid
}

// TestReuseDriftWarningBelowPin exercises the branch the whole change exists
// for: an adopted server older than the pinned floor, named in the warning.
func TestReuseDriftWarningBelowPin(t *testing.T) {
	t.Setenv("STUB_VERSION", "0.1.0")
	exe := buildVersionStub(t)
	pid := startStub(t, exe)

	w := reuseDriftWarning(pid)
	for _, want := range []string{"0.1.0", serverVersion, exe} {
		if !strings.Contains(w, want) {
			t.Errorf("reuseDriftWarning = %q, want it to name %q", w, want)
		}
	}
}

// TestReuseDriftWarningBinaryReplaced covers the second branch: a process at the
// pinned version whose binary was swapped underneath it.
func TestReuseDriftWarningBinaryReplaced(t *testing.T) {
	t.Setenv("STUB_VERSION", serverVersion) // at the pin, so only the swap can warn
	exe := buildVersionStub(t)
	pid := startStub(t, exe)

	if w := reuseDriftWarning(pid); w != "" {
		t.Fatalf("before replacement: reuseDriftWarning = %q, want empty", w)
	}

	old := binaryReplaceSkew
	binaryReplaceSkew = 0
	t.Cleanup(func() { binaryReplaceSkew = old })

	// ps lstart is second-granular, so let the clock cross a second boundary or
	// the replacement can read as simultaneous with the process start.
	time.Sleep(1100 * time.Millisecond)
	now := time.Now()
	if err := os.Chtimes(exe, now, now); err != nil {
		t.Fatalf("touch stub: %v", err)
	}

	w := reuseDriftWarning(pid)
	if !strings.Contains(w, "replaced") || !strings.Contains(w, exe) {
		t.Errorf("reuseDriftWarning = %q, want it to report %s was replaced", w, exe)
	}
}

// TestExecRefusal pins the gate that stops the probe running a file another user
// could have rewritten between the adoption and the probe.
func TestExecRefusal(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, mode os.FileMode) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("#!/bin/sh\ntrue\n"), mode); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		if err := os.Chmod(p, mode); err != nil { // WriteFile respects umask
			t.Fatalf("chmod %s: %v", name, err)
		}
		return p
	}

	tests := []struct {
		name string
		path string
		want string
	}{
		{name: "owned and not writable by others", path: write("ok", 0o755)},
		{name: "world writable", path: write("world-writable", 0o777), want: "is writable by other users"},
		{name: "group writable", path: write("group-writable", 0o775), want: "is writable by other users"},
		{name: "not executable", path: write("plain", 0o644), want: "is not a regular executable"},
		{name: "directory", path: dir, want: "is not a regular executable"},
		{name: "missing", path: filepath.Join(dir, "absent"), want: "cannot be read"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := execRefusal(tt.path); got != tt.want {
				t.Errorf("execRefusal(%s) = %q, want %q", tt.name, got, tt.want)
			}
		})
	}
}

// TestReuseDriftWarningRefusesUnsafeBinary checks a binary others can rewrite is
// reported as an unchecked server rather than passed off as healthy.
func TestReuseDriftWarningRefusesUnsafeBinary(t *testing.T) {
	t.Setenv("STUB_VERSION", "0.1.0")
	exe := buildVersionStub(t)
	pid := startStub(t, exe)
	if err := os.Chmod(exe, 0o777); err != nil {
		t.Fatalf("chmod stub: %v", err)
	}

	w := reuseDriftWarning(pid)
	if !strings.Contains(w, "cannot version-check") || !strings.Contains(w, "writable by other users") {
		t.Errorf("reuseDriftWarning = %q, want a refusal naming the permissions", w)
	}
}

// TestProbeBounded separates the two failures a discovery command can have: a
// clean non-zero exit is an answer, an unrunnable command is not.
func TestProbeBounded(t *testing.T) {
	if out, ran := probeBounded(nil, "sh", "-c", "echo hello"); out != "hello" || !ran {
		t.Errorf("probeBounded(echo) = %q, %v; want %q, true", out, ran, "hello")
	}
	// pgrep reports "no match" as exit 1, which must not read as a failed probe.
	if out, ran := probeBounded(nil, "sh", "-c", "exit 1"); out != "" || !ran {
		t.Errorf("probeBounded(exit 1) = %q, %v; want empty, true", out, ran)
	}
	if _, ran := probeBounded(nil, "definitely-not-a-real-command-9f3c"); ran {
		t.Error("probeBounded(missing command) reported the probe as having run")
	}
	if _, ran := probeBounded(nil, "sh", "-c", "sleep 60"); ran {
		t.Error("probeBounded(hanging command) reported the probe as having run")
	}
}

// TestReuseDriftWarningReplacedBelowPin covers the ordering: when the binary was
// swapped and the replacement is itself below the pin, the replacement is the
// true statement, since the below-pin text names a version nothing is serving.
func TestReuseDriftWarningReplacedBelowPin(t *testing.T) {
	t.Setenv("STUB_VERSION", "0.1.0")
	exe := buildVersionStub(t)
	pid := startStub(t, exe)

	old := binaryReplaceSkew
	binaryReplaceSkew = 0
	t.Cleanup(func() { binaryReplaceSkew = old })

	time.Sleep(1100 * time.Millisecond) // ps lstart is second-granular
	now := time.Now()
	if err := os.Chtimes(exe, now, now); err != nil {
		t.Fatalf("touch stub: %v", err)
	}

	w := reuseDriftWarning(pid)
	if !strings.Contains(w, "replaced") {
		t.Errorf("reuseDriftWarning = %q, want the replacement reported, not the on-disk version", w)
	}
}

// TestProcIsOurs pins the explicit owner check that stops a privileged plugin
// resolving a foreign process's executable through /proc/<pid>/exe.
func TestProcIsOurs(t *testing.T) {
	if _, err := os.Stat("/proc/self"); err != nil {
		// No procfs: serverExecutable takes the ps path, which carries its own
		// owner check, and procIsOurs is expected to refuse everything.
		if procIsOurs(os.Getpid()) {
			t.Error("procIsOurs reported true without procfs; the ps path must own that decision")
		}
		return
	}
	if !procIsOurs(os.Getpid()) {
		t.Error("procIsOurs(self) = false, want true")
	}
	// Not "pid 1 is root's": under a rootless container it can be ours, and
	// procIsOurs is then right to say true.
	var st syscall.Stat_t
	if err := syscall.Stat("/proc/1", &st); err == nil && int(st.Uid) != os.Getuid() && procIsOurs(1) {
		t.Error("procIsOurs(pid 1) = true for a process owned by another user, want false")
	}
}

// TestPsArgsReportsProbeRan checks the discovery probes distinguish a process
// that is gone from an inspection that never happened.
func TestPsArgsReportsProbeRan(t *testing.T) {
	args, ran := psArgs(os.Getpid())
	if !ran || args == "" {
		t.Errorf("psArgs(self) = %q, %v; want args and true", args, ran)
	}
	// A pid that cannot exist: ps exits non-zero, which is still an answer.
	if _, ran := psArgs(-1); !ran {
		t.Error("psArgs(bad pid) reported the probe as not having run")
	}
}
