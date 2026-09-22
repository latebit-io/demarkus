package provision

import (
	"context"
	"fmt"
	"net"
	"path/filepath"
	"strconv"
	"syscall"

	"os"
	"strings"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/config"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/provision/procscan"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/provision/server"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/provision/tokens"
	"github.com/latebit-io/demarkus/tools/internal/token"

	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/provision/provisiontest"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/provision/release"
)

// TestReuseDriftWarningQuietOnHealthy guards the false-positive direction: a
// server at the pin whose binary predates it must say nothing, or every session
// start would nag.
func TestReuseDriftWarningQuietOnHealthy(t *testing.T) {
	t.Setenv("STUB_VERSION", release.ServerVersion)
	pid := provisiontest.StartStub(t, provisiontest.BuildStub(t))
	if w := reuseDriftWarning(pid); w != "" {
		t.Errorf("reuseDriftWarning(current server) = %q, want empty", w)
	}
}

// TestReuseDriftWarningUnreadableVersion covers the gap that let an unprobeable
// server read as healthy: no usable version is a lost check, not a clean one.
func TestReuseDriftWarningUnreadableVersion(t *testing.T) {
	t.Setenv("STUB_VERSION", "dev")
	exe := provisiontest.BuildStub(t)
	pid := provisiontest.StartStub(t, exe)

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

// TestReuseDriftWarningBelowPin exercises the branch the whole change exists
// for: an adopted server older than the pinned floor, named in the warning.
func TestReuseDriftWarningBelowPin(t *testing.T) {
	t.Setenv("STUB_VERSION", "0.1.0")
	exe := provisiontest.BuildStub(t)
	pid := provisiontest.StartStub(t, exe)

	w := reuseDriftWarning(pid)
	for _, want := range []string{"0.1.0", release.ServerVersion, exe} {
		if !strings.Contains(w, want) {
			t.Errorf("reuseDriftWarning = %q, want it to name %q", w, want)
		}
	}
}

// TestReuseDriftWarningBinaryReplaced covers the second branch: a process at the
// pinned version whose binary was swapped underneath it.
func TestReuseDriftWarningBinaryReplaced(t *testing.T) {
	t.Setenv("STUB_VERSION", release.ServerVersion) // at the pin, so only the swap can warn
	exe := provisiontest.BuildStub(t)
	pid := provisiontest.StartStub(t, exe)

	if w := reuseDriftWarning(pid); w != "" {
		t.Fatalf("before replacement: reuseDriftWarning = %q, want empty", w)
	}

	// A second past the start clears binaryReplaceSkew where mtime is the only
	// signal; procfs sees the swapped inode without waiting.
	time.Sleep(1100 * time.Millisecond)
	provisiontest.ReplaceBinary(t, exe)

	w := reuseDriftWarning(pid)
	if !strings.Contains(w, "replaced") || !strings.Contains(w, exe) {
		t.Errorf("reuseDriftWarning = %q, want it to report %s was replaced", w, exe)
	}
}

// TestReuseDriftWarningAtomicReplaceAtStart covers the window the mtime
// comparison cannot see: a swap in the same second as the launch, caught only by
// procfs inode identity.
func TestReuseDriftWarningAtomicReplaceAtStart(t *testing.T) {
	if _, err := os.Stat("/proc/self"); err != nil {
		t.Skip("no procfs: the sub-second window is irreducible without inode identity")
	}
	t.Setenv("STUB_VERSION", release.ServerVersion) // at the pin, so only the swap can warn
	exe := provisiontest.BuildStub(t)
	pid := provisiontest.StartStub(t, exe)

	provisiontest.ReplaceBinary(t, exe) // no delay: the window the mtime comparison cannot see

	w := reuseDriftWarning(pid)
	if !strings.Contains(w, "replaced") || !strings.Contains(w, exe) {
		t.Errorf("reuseDriftWarning = %q, want it to report %s was replaced", w, exe)
	}
}

// TestReuseDriftWarningRefusesUnsafeBinary checks a binary others can rewrite is
// reported as an unchecked server rather than passed off as healthy.
func TestReuseDriftWarningRefusesUnsafeBinary(t *testing.T) {
	t.Setenv("STUB_VERSION", "0.1.0")
	exe := provisiontest.BuildStub(t)
	pid := provisiontest.StartStub(t, exe)
	if err := os.Chmod(exe, 0o777); err != nil {
		t.Fatalf("chmod stub: %v", err)
	}

	w := reuseDriftWarning(pid)
	if !strings.Contains(w, "cannot version-check") || !strings.Contains(w, "writable by other users") {
		t.Errorf("reuseDriftWarning = %q, want a refusal naming the permissions", w)
	}
}

// TestReuseDriftWarningReplacedBelowPin covers the ordering: when the binary was
// swapped and the replacement is itself below the pin, the replacement is the
// true statement, since the below-pin text names a version nothing is serving.
func TestReuseDriftWarningReplacedBelowPin(t *testing.T) {
	t.Setenv("STUB_VERSION", "0.1.0")
	exe := provisiontest.BuildStub(t)
	pid := provisiontest.StartStub(t, exe)

	time.Sleep(1100 * time.Millisecond) // clears binaryReplaceSkew where mtime is the only signal
	provisiontest.ReplaceBinary(t, exe)

	w := reuseDriftWarning(pid)
	if !strings.Contains(w, "replaced") {
		t.Errorf("reuseDriftWarning = %q, want the replacement reported, not the on-disk version", w)
	}
}

// installCurrentBinaries stages every pinned binary answering --version with
// its pin, so EnsureBinaries touches no network, and the server stub serves.
func installCurrentBinaries(t *testing.T, home string) {
	t.Helper()
	t.Setenv("STUB_VERSION", release.ServerVersion)
	t.Setenv("FAKE_TOKEN_VERSION", release.ToolsRef())
	provisiontest.InstallBinary(t, home, "demarkus-server", provisiontest.BuildStub(t))
	provisiontest.WriteBinary(t, home, "demarkus-mcp", provisiontest.VersionScript(release.ClientVersion))
	provisiontest.WriteFakeTokenBin(t, home, "")
	if replaced, err := release.EnsureBinaries(); err != nil || replaced {
		t.Fatalf("stubs do not read as current: replaced=%v err=%v", replaced, err)
	}
}

// lifecycleHome is a temp home with current binaries, free managed ports and
// a canned auth probe answering status.
func lifecycleHome(t *testing.T, probeStatus string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	// A probe that misses its budget must fail here, never download a release.
	oldBase := release.BaseURL
	t.Cleanup(func() { release.BaseURL = oldBase })
	release.BaseURL = "http://127.0.0.1:1/"
	installCurrentBinaries(t, home)
	oldDefault, oldIsolated := defaultPort, isolatedPortStart
	t.Cleanup(func() { defaultPort, isolatedPortStart = oldDefault, oldIsolated })
	defaultPort, isolatedPortStart = provisiontest.FreePort(t), provisiontest.FreePort(t)
	stubProbe(t, probeStatus)
	return home
}

type cannedProbe struct{ status string }

func (c cannedProbe) Append(context.Context, fetch.WriteRequest) (fetch.Result, error) {
	return fetch.Result{Response: protocol.Response{Status: c.status}}, nil
}

func stubProbe(t *testing.T, status string) {
	t.Helper()
	old := tokens.DialProbe
	t.Cleanup(func() { tokens.DialProbe = old })
	tokens.DialProbe = func() (tokens.AuthProbeClient, func()) { return cannedProbe{status: status}, func() {} }
}

// managedPID returns the pid a managed server recorded under memory, killing
// and reaping it when the test ends (the plugin process would have exited).
func managedPID(t *testing.T, memory string) int {
	t.Helper()
	pid := procscan.ReadPID(filepath.Join(memory, ".pid"))
	if pid <= 0 {
		t.Fatalf("no managed server recorded under %s", memory)
	}
	t.Cleanup(func() { killAndReap(pid) })
	return pid
}

// killAndReap ends a spawned server and waits for its exit, so the next probe
// cannot see it as a live process.
func killAndReap(pid int) {
	_ = procscan.Signal(pid, syscall.SIGKILL)
	if p, err := os.FindProcess(pid); err == nil {
		_, _ = p.Wait()
	}
}

func writeConfig(t *testing.T, memory string, port int, mode, tokensTOML string) {
	t.Helper()
	if err := config.SaveConfig(memory, port, mode, tokensTOML); err != nil {
		t.Fatal(err)
	}
}

// TestInitManaged covers the two managed modes: default on the first-choice
// port, the isolated fallback when it is taken, and the refusal to record a
// setup whose token does not authenticate.
func TestInitManaged(t *testing.T) {
	t.Run("default", func(t *testing.T) {
		home := lifecycleHome(t, protocol.StatusNotFound)
		if err := Init("default", 0, ""); err != nil {
			t.Fatalf("Init(default): %v", err)
		}
		cfg, err := config.LoadConfig()
		if err != nil || cfg == nil {
			t.Fatalf("config after default setup = %+v, %v", cfg, err)
		}
		memory := filepath.Join(home, ".demarkus", "soul")
		if cfg.Mode != "default" || cfg.MemoryDir != memory || cfg.Port != strconv.Itoa(defaultPort) || cfg.TokensTOML != "" {
			t.Errorf("config = %+v, want default mode on port %d at %s", cfg, defaultPort, memory)
		}
		managedPID(t, memory)
		tokenFile, _ := config.TokenPath()
		if !config.FileExists(tokenFile) {
			t.Error("plugin token not minted")
		}
		tokensTOML, _ := config.ManagedTokensPath(memory)
		f, err := token.ReadFile(tokensTOML)
		if err != nil {
			t.Fatalf("managed tokens.toml: %v", err)
		}
		if _, ok := f.Tokens["claude-code-plugin"]; !ok {
			t.Errorf("plugin entry missing from %s", tokensTOML)
		}
	})
	t.Run("default port taken falls back to isolated", func(t *testing.T) {
		home := lifecycleHome(t, protocol.StatusNotFound)
		conn, err := net.ListenPacket("udp", ":"+strconv.Itoa(defaultPort))
		if err != nil {
			t.Fatalf("take port %d: %v", defaultPort, err)
		}
		defer func() { _ = conn.Close() }()
		if err := Init("default", 0, ""); err != nil {
			t.Fatalf("Init(default): %v", err)
		}
		cfg, err := config.LoadConfig()
		if err != nil || cfg == nil || cfg.Mode != "isolated" || cfg.MemoryDir != filepath.Join(home, ".demarkus", "plugin-soul") {
			t.Fatalf("config = %+v, %v; want isolated mode", cfg, err)
		}
		if p, _ := strconv.Atoi(cfg.Port); p < isolatedPortStart || p > isolatedPortStart+199 {
			t.Errorf("isolated port %s outside %d..%d", cfg.Port, isolatedPortStart, isolatedPortStart+199)
		}
		managedPID(t, cfg.MemoryDir)
	})
	t.Run("token that does not authenticate is not recorded", func(t *testing.T) {
		home := lifecycleHome(t, protocol.StatusUnauthorized)
		err := Init("isolated", 0, "")
		if err == nil || !strings.Contains(err.Error(), "does not authenticate") {
			t.Fatalf("Init(isolated) = %v, want an auth refusal", err)
		}
		if cfg, err := config.LoadConfig(); err != nil || cfg != nil {
			t.Errorf("config recorded despite the refusal: %+v, %v", cfg, err)
		}
		managedPID(t, filepath.Join(home, ".demarkus", "plugin-soul"))
	})
}

// TestInitReuse adopts a user-managed server: the token lands in the registry
// that server reads, a port that disagrees is refused, and a root with no
// server is recorded with a warning.
func TestInitReuse(t *testing.T) {
	home := lifecycleHome(t, protocol.StatusNotFound)
	exe, _ := config.BinPath("demarkus-server")
	root := filepath.Join(home, "user-root")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	// The adopted server reads a registry outside its root, which exists.
	tokensTOML := filepath.Join(root, "registry", "tokens.toml")
	if err := os.MkdirAll(filepath.Dir(tokensTOML), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokensTOML, []byte("[tokens]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	port := provisiontest.FreePort(t)
	provisiontest.StartServer(t, exe, provisiontest.ServerArgs{Root: root, Port: port, Tokens: tokensTOML})

	if err := Init("reuse", port+1, root); err == nil || !strings.Contains(err.Error(), "listening on port") {
		t.Fatalf("Init(reuse) with the wrong port = %v, want a port mismatch", err)
	}
	if err := Init("reuse", port, root); err != nil {
		t.Fatalf("Init(reuse): %v", err)
	}
	cfg, err := config.LoadConfig()
	if err != nil || cfg == nil || cfg.Mode != "reuse" || cfg.MemoryDir != root || cfg.TokensTOML != tokensTOML {
		t.Fatalf("config = %+v, %v; want reuse of %s reading %s", cfg, err, root, tokensTOML)
	}
	f, err := token.ReadFile(tokensTOML)
	if err != nil {
		t.Fatalf("adopted registry: %v", err)
	}
	if _, ok := f.Tokens["claude-code-plugin"]; !ok {
		t.Errorf("plugin entry missing from the registry the server reads")
	}
	if config.FileExists(filepath.Join(root, "tokens.toml")) {
		t.Error("the conventional tokens.toml was written although the server reads another")
	}

	down := filepath.Join(home, "down-root")
	if err := Init("reuse", port, down); err != nil {
		t.Fatalf("Init(reuse) with no server = %v, want a warning only", err)
	}
	if cfg, err := config.LoadConfig(); err != nil || cfg == nil || cfg.MemoryDir != down {
		t.Errorf("config after a down reuse = %+v, %v", cfg, err)
	}
}

// TestProvisionLocked covers the per-session sequence: first run performs the
// default setup, a managed config spawns its server, a reuse config whose
// server is down skips seeding, an unknown mode is an error.
func TestProvisionLocked(t *testing.T) {
	t.Run("first run performs the default setup", func(t *testing.T) {
		home := lifecycleHome(t, protocol.StatusNotFound)
		port, err := provisionLocked()
		if err != nil || port != defaultPort {
			t.Fatalf("provisionLocked() = %d, %v; want the default port %d", port, err, defaultPort)
		}
		managedPID(t, filepath.Join(home, ".demarkus", "soul"))
	})
	t.Run("managed config spawns its server", func(t *testing.T) {
		home := lifecycleHome(t, protocol.StatusNotFound)
		memory := filepath.Join(home, "managed")
		port := provisiontest.FreePort(t)
		writeConfig(t, memory, port, "isolated", "")
		if got, err := provisionLocked(); err != nil || got != port {
			t.Fatalf("provisionLocked() = %d, %v; want %d", got, err, port)
		}
		managedPID(t, memory)
	})
	t.Run("reuse with the server down skips seeding", func(t *testing.T) {
		home := lifecycleHome(t, protocol.StatusNotFound)
		writeConfig(t, filepath.Join(home, "nobody"), provisiontest.FreePort(t), "reuse", "")
		if port, err := provisionLocked(); err != nil || port != 0 {
			t.Fatalf("provisionLocked() = %d, %v; want 0, nil", port, err)
		}
	})
	t.Run("unknown mode", func(t *testing.T) {
		home := lifecycleHome(t, protocol.StatusNotFound)
		writeConfig(t, filepath.Join(home, "x"), 1, "weird", "")
		if _, err := provisionLocked(); err == nil || !strings.Contains(err.Error(), "unknown MODE") {
			t.Fatalf("provisionLocked() = %v, want an unknown mode error", err)
		}
	})
}

// TestVerifyAuth pins the verdict lines the doctor and /soul-status parse.
func TestVerifyAuth(t *testing.T) {
	home := lifecycleHome(t, protocol.StatusNotFound)
	exe, _ := config.BinPath("demarkus-server")
	root := filepath.Join(home, "root")
	tokensTOML := filepath.Join(root, "tokens.toml")
	port := provisiontest.FreePort(t)

	if got, err := VerifyAuth(); err != nil || got != "no local memory configured (run /soul-init)" {
		t.Errorf("VerifyAuth() without config = %q, %v", got, err)
	}
	writeConfig(t, root, port, "reuse", "")
	if got, err := VerifyAuth(); err != nil || got != fmt.Sprintf("cannot verify: no running demarkus-server for %s (expected token registry: %s)", root, tokensTOML) {
		t.Errorf("VerifyAuth() with no server = %q, %v", got, err)
	}

	tokenFile, _ := config.TokenPath()
	if err := os.WriteFile(tokenFile, []byte("tok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	pid := provisiontest.StartServer(t, exe, provisiontest.ServerArgs{Root: root, Port: port, Tokens: tokensTOML})
	if got, err := VerifyAuth(); err != nil || got != fmt.Sprintf("write auth healthy (server pid %d, token registry %s)", pid, tokensTOML) {
		t.Errorf("VerifyAuth() healthy = %q, %v", got, err)
	}
	stubProbe(t, protocol.StatusUnauthorized)
	if got, err := VerifyAuth(); err != nil || !strings.HasPrefix(got, "token drift: ") || !strings.Contains(got, tokensTOML) {
		t.Errorf("VerifyAuth() drift = %q, %v", got, err)
	}
	writeConfig(t, root, port+1, "reuse", "")
	if got, err := VerifyAuth(); err != nil || got != fmt.Sprintf("cannot verify: server pid %d listens on port %d but the config says %d; re-run /soul-init", pid, port, port+1) {
		t.Errorf("VerifyAuth() port mismatch = %q, %v", got, err)
	}
}

// TestHealthWarning covers the session-start line for every mode: a managed
// server that is live or gone, a reused server that is absent, current, or
// behind the pin.
func TestHealthWarning(t *testing.T) {
	home := lifecycleHome(t, protocol.StatusNotFound)
	exe, _ := config.BinPath("demarkus-server")

	if got, err := HealthWarning(); err != nil || got != "" {
		t.Errorf("HealthWarning() without config = %q, %v", got, err)
	}
	managed := filepath.Join(home, "managed")
	port := provisiontest.FreePort(t)
	if err := server.Ensure(managed, port); err != nil {
		t.Fatal(err)
	}
	pid := managedPID(t, managed)
	writeConfig(t, managed, port, "isolated", "")
	if got, err := HealthWarning(); err != nil || got != "" {
		t.Errorf("HealthWarning() with a live managed server = %q, %v", got, err)
	}
	killAndReap(pid)
	if got, err := HealthWarning(); err != nil || !strings.Contains(got, "is not running") {
		t.Errorf("HealthWarning() with a dead managed server = %q, %v", got, err)
	}

	root := filepath.Join(home, "root")
	writeConfig(t, root, port, "reuse", "")
	if got, err := HealthWarning(); err != nil || !strings.Contains(got, "configured to reuse a server rooted at") {
		t.Errorf("HealthWarning() with no reused server = %q, %v", got, err)
	}
	provisiontest.StartServer(t, exe, provisiontest.ServerArgs{Root: root, Port: port, Tokens: filepath.Join(root, "tokens.toml")})
	if got, err := HealthWarning(); err != nil || got != "" {
		t.Errorf("HealthWarning() with a current reused server = %q, %v", got, err)
	}

	old := filepath.Join(home, "old")
	t.Setenv("STUB_VERSION", "0.1.0")
	provisiontest.StartServer(t, exe, provisiontest.ServerArgs{Root: old, Port: provisiontest.FreePort(t), Tokens: filepath.Join(old, "tokens.toml")})
	writeConfig(t, old, port, "reuse", "")
	if got, err := HealthWarning(); err != nil || !strings.Contains(got, "below the "+release.ServerVersion) {
		t.Errorf("HealthWarning() with a reused server behind the pin = %q, %v", got, err)
	}
}
