package tokens

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/provision/provisiontest"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/config"
	"github.com/latebit-io/demarkus/tools/internal/token"
)

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

// TestEnsureTokenEntryScope pins the minted scope to the recursive "/**" and
// the remint-on-stale-scope behavior, via a fake demarkus-token binary that
// logs its argv and mirrors generate's tokens.toml append.
func TestEnsureTokenEntryScope(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	argsLog := filepath.Join(home, "args.log")
	provisiontest.WriteFakeTokenBin(t, home, argsLog)

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
		if err := Ensure(memory, tokensTOML); err != nil {
			t.Fatalf("fresh mint: %v", err)
		}
	})
	if !strings.Contains(log, "-paths /**") {
		t.Fatalf("fresh mint did not use /**; argv:\n%s", log)
	}

	// Provisioned-and-current is a no-op: no further binary invocations.
	log = phaseLog(func() {
		if err := Ensure(memory, tokensTOML); err != nil {
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
		if err := Ensure(memory, tokensTOML); err != nil {
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
	if err := Ensure(memory, tokensTOML); err == nil {
		t.Error("revoke failure did not surface an error")
	}
	t.Setenv("FAKE_REVOKE_FAIL", "")

	// A tokens.toml we cannot parse fails provisioning rather than being
	// silently treated as current (or clobbered).
	malformed := "[tokens." + tokenLabel + "]\nnot toml [[["
	if err := os.WriteFile(tokensTOML, []byte(malformed), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Ensure(memory, tokensTOML); err == nil {
		t.Error("malformed tokens.toml did not surface an error")
	}

	// A raw token that no longer matches the entry's hash (leftovers of a
	// failed run) fails the gate and is reminted.
	if err := os.Remove(tokensTOML); err != nil { // clear the malformed file
		t.Fatal(err)
	}
	if err := Ensure(memory, tokensTOML); err != nil {
		t.Fatalf("re-mint after malformed: %v", err)
	}
	tokenFile, err := config.TokenPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokenFile, []byte("not-the-minted-raw"), 0o600); err != nil {
		t.Fatal(err)
	}
	log = phaseLog(func() {
		if err := Ensure(memory, tokensTOML); err != nil {
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
	provisiontest.WriteFakeTokenBin(t, home, "")

	memory := filepath.Join(home, "root")
	if err := os.MkdirAll(memory, 0o755); err != nil {
		t.Fatal(err)
	}
	tokensTOML := filepath.Join(memory, "tokens.toml")
	tokenFile, err := config.TokenPath()
	if err != nil {
		t.Fatal(err)
	}

	origFind, origReload := findServerAtRoot, reloadServer
	t.Cleanup(func() { findServerAtRoot, reloadServer = origFind, origReload })
	findServerAtRoot = func(string) int { return 4242 }
	reloads := 0
	reloadServer = func(int) error { reloads++; return errors.New("injected reload failure") }

	if err := Ensure(memory, tokensTOML); err == nil {
		t.Fatal("failed reload did not surface an error")
	}
	if reloads != 1 {
		t.Fatalf("reloads = %d, want 1", reloads)
	}
	if config.FileExists(tokenFile) {
		t.Error("token file committed despite failed reload; gate would skip the retry")
	}

	// The next run retries: mint again, reload succeeds, token file commits.
	reloadServer = func(int) error { reloads++; return nil }
	if err := Ensure(memory, tokensTOML); err != nil {
		t.Fatalf("retry after failed reload: %v", err)
	}
	if reloads != 2 {
		t.Fatalf("reloads = %d, want 2", reloads)
	}
	if !config.FileExists(tokenFile) {
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

	newPath, err := config.ManagedTokensPath(memory)
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(newPath, memory+string(os.PathSeparator)) {
		t.Fatalf("managed tokens path %s is inside the memory root", newPath)
	}

	// Fresh install: nothing on disk resolves to the new path.
	if got, err := PathFor(memory); err != nil || got != newPath {
		t.Fatalf("fresh tokensPathFor = %q, %v; want %q", got, err, newPath)
	}

	// A legacy file wins resolution until migrated: the running server's
	// -tokens flag still points there.
	legacy := filepath.Join(memory, "tokens.toml")
	if err := os.WriteFile(legacy, []byte("[tokens]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := PathFor(memory); err != nil || got != legacy {
		t.Fatalf("unmigrated tokensPathFor = %q, %v; want %q", got, err, legacy)
	}

	// Migration moves the file, preserving content; resolution flips.
	migrated, err := MigrateManaged(memory)
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
	if config.FileExists(legacy) {
		t.Error("legacy tokens.toml still present after migration")
	}
	if got, err := PathFor(memory); err != nil || got != newPath {
		t.Fatalf("post-migration tokensPathFor = %q, %v; want %q", got, err, newPath)
	}

	// Idempotent rerun leaves the migrated file alone.
	if again, err := MigrateManaged(memory); err != nil || again != newPath {
		t.Fatalf("rerun migrateManagedTokens = %q, %v; want %q", again, err, newPath)
	}

	// A leftover legacy file (failed removal after a prior copy) is cleaned up
	// on the next run without touching the migrated file.
	if err := os.WriteFile(legacy, []byte("leftover"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := MigrateManaged(memory); err != nil || got != newPath {
		t.Fatalf("leftover-cleanup migrateManagedTokens = %q, %v; want %q", got, err, newPath)
	}
	if config.FileExists(legacy) {
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

	if err := SignalReload(pid); err != nil {
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

	if err := SignalReload(pid); err != nil {
		t.Errorf("signalReload(dead pid) = %v, want nil", err)
	}

	// Non-positive pids are rejected before any signal: kill(0)/kill(-1)
	// broadcast to the process group / all reachable processes.
	for _, bad := range []int{0, -1} {
		if err := SignalReload(bad); err == nil {
			t.Errorf("signalReload(%d) = nil, want error", bad)
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
			err := verifyTokenWith(t.Context(), c.mock, "localhost:6309", "tok")
			if (err != nil) != c.wantErr {
				t.Errorf("verifyTokenWith error = %v, wantErr %v", err, c.wantErr)
			}
			// Drift classification hangs on this: only a real unauthorized may
			// match the sentinel, transport errors must not.
			if gotUnauth := errors.Is(err, ErrUnauthorized); gotUnauth != (c.mock.status == protocol.StatusUnauthorized) {
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

func (m *mockAppendClient) Append(_ context.Context, r fetch.WriteRequest) (fetch.Result, error) {
	m.path = r.Path
	m.expectedVersion = r.ExpectedVersion
	if m.err != nil {
		return fetch.Result{}, m.err
	}
	return fetch.Result{Response: protocol.Response{Status: m.status}}, nil
}
