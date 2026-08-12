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
	"strings"
	"syscall"
	"testing"

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

	soul := filepath.Join(t.TempDir(), "soul dir") // a space to exercise quoting
	if err := saveConfig(soul, 16312, "isolated"); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg == nil {
		t.Fatal("loadConfig returned nil after save")
	}
	if cfg.SoulDir != soul {
		t.Errorf("SoulDir = %q, want %q", cfg.SoulDir, soul)
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
	soul := "/Users/x/.demarkus/soul"
	if err := saveConfig(soul, 6310, "default"); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg == nil || cfg.SoulDir != soul || cfg.Port != "6310" || cfg.Mode != "default" {
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
		{" demarkus-server -root /home/x/soul -port 6310", "/home/x/soul", true},
		{" demarkus-server -root=/home/x/soul -port 6310", "/home/x/soul", true},
		{" demarkus-server -port 6310 -root /home/x/soul", "/home/x/soul", true},   // at end
		{" demarkus-server -port 6310 -root=/home/x/soul", "/home/x/soul", true},   // at end, = form
		{" demarkus-server -root /home/x/soul2 -port 6310", "/home/x/soul", false}, // prefix only
		{" demarkus-server -root /other -port 6310", "/home/x/soul", false},
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
	for _, soul := range []string{
		"/plain/path",
		"/path with space",
		"/path/with'quote",
	} {
		if err := saveConfig(soul, 6310, "default"); err != nil {
			t.Fatalf("saveConfig(%q): %v", soul, err)
		}
		cfg, err := loadConfig()
		if err != nil || cfg == nil {
			t.Fatalf("loadConfig(%q): %v", soul, err)
		}
		if cfg.SoulDir != soul {
			t.Errorf("round-trip %q -> %q", soul, cfg.SoulDir)
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

	soul := filepath.Join(home, "root")
	if err := os.MkdirAll(soul, 0o755); err != nil {
		t.Fatal(err)
	}
	tokensTOML := filepath.Join(soul, "tokens.toml")
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
		if err := ensureTokenEntry(soul, tokensTOML); err != nil {
			t.Fatalf("fresh mint: %v", err)
		}
	})
	if !strings.Contains(log, "-paths /**") {
		t.Fatalf("fresh mint did not use /**; argv:\n%s", log)
	}

	// Provisioned-and-current is a no-op: no further binary invocations.
	log = phaseLog(func() {
		if err := ensureTokenEntry(soul, tokensTOML); err != nil {
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
		if err := ensureTokenEntry(soul, tokensTOML); err != nil {
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
	if err := ensureTokenEntry(soul, tokensTOML); err == nil {
		t.Error("revoke failure did not surface an error")
	}
	t.Setenv("FAKE_REVOKE_FAIL", "")

	// A tokens.toml we cannot parse fails provisioning rather than being
	// silently treated as current (or clobbered).
	malformed := "[tokens." + tokenLabel + "]\nnot toml [[["
	if err := os.WriteFile(tokensTOML, []byte(malformed), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ensureTokenEntry(soul, tokensTOML); err == nil {
		t.Error("malformed tokens.toml did not surface an error")
	}

	// A raw token that no longer matches the entry's hash (leftovers of a
	// failed run) fails the gate and is reminted.
	if err := os.Remove(tokensTOML); err != nil { // clear the malformed file
		t.Fatal(err)
	}
	if err := ensureTokenEntry(soul, tokensTOML); err != nil {
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
		if err := ensureTokenEntry(soul, tokensTOML); err != nil {
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

	soul := filepath.Join(home, "root")
	if err := os.MkdirAll(soul, 0o755); err != nil {
		t.Fatal(err)
	}
	tokensTOML := filepath.Join(soul, "tokens.toml")
	tokenFile, err := pluginToken()
	if err != nil {
		t.Fatal(err)
	}

	origFind, origReload := findServerAtRoot, reloadServer
	t.Cleanup(func() { findServerAtRoot, reloadServer = origFind, origReload })
	findServerAtRoot = func(string) int { return 4242 }
	reloads := 0
	reloadServer = func(int) error { reloads++; return errors.New("injected reload failure") }

	if err := ensureTokenEntry(soul, tokensTOML); err == nil {
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
	if err := ensureTokenEntry(soul, tokensTOML); err != nil {
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
	soul := filepath.Join(home, "soul")
	if err := os.MkdirAll(soul, 0o755); err != nil {
		t.Fatal(err)
	}

	newPath, err := managedTokensPath(soul)
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(newPath, soul+string(os.PathSeparator)) {
		t.Fatalf("managed tokens path %s is inside the soul root", newPath)
	}

	// Fresh install: nothing on disk resolves to the new path.
	if got, err := tokensPathFor(soul); err != nil || got != newPath {
		t.Fatalf("fresh tokensPathFor = %q, %v; want %q", got, err, newPath)
	}

	// A legacy file wins resolution until migrated: the running server's
	// -tokens flag still points there.
	legacy := filepath.Join(soul, "tokens.toml")
	if err := os.WriteFile(legacy, []byte("[tokens]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := tokensPathFor(soul); err != nil || got != legacy {
		t.Fatalf("unmigrated tokensPathFor = %q, %v; want %q", got, err, legacy)
	}

	// Migration moves the file, preserving content; resolution flips.
	migrated, err := migrateManagedTokens(soul)
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
	if got, err := tokensPathFor(soul); err != nil || got != newPath {
		t.Fatalf("post-migration tokensPathFor = %q, %v; want %q", got, err, newPath)
	}

	// Idempotent rerun leaves the migrated file alone.
	if again, err := migrateManagedTokens(soul); err != nil || again != newPath {
		t.Fatalf("rerun migrateManagedTokens = %q, %v; want %q", again, err, newPath)
	}

	// A leftover legacy file (failed removal after a prior copy) is cleaned up
	// on the next run without touching the migrated file.
	if err := os.WriteFile(legacy, []byte("leftover"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := migrateManagedTokens(soul); err != nil || got != newPath {
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
