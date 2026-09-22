// Package provision orchestrates the plugin's managed-server lifecycle per
// session (config, binaries, server, token, seed) and reports its health.
package provision

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/provision/procscan"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/provision/release"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/provision/seed"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/provision/server"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/provision/tokens"

	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/config"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/lockdir"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/provision/progress"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/semver"
)

// Managed port choices: the first-choice port, then the isolated range.
// Variables so tests can pick free ports.
var (
	defaultPort       = 6310
	isolatedPortStart = 16310
)

// restartLocalServerOnUpgrade moves the local server onto a freshly swapped
// binary. Ownership is the MODE, not a .pid: reuse-mode servers are the user's,
// so only a down one is started. Restart problems are warned, never propagated.
func restartLocalServerOnUpgrade(replaced bool) {
	if !replaced {
		return
	}
	cfg, err := config.LoadConfig()
	if err != nil || cfg == nil {
		return
	}
	port, err := strconv.Atoi(strings.TrimSpace(cfg.Port))
	if err != nil {
		progress.Warnf("could not parse recorded PORT %q for upgrade restart; run /soul-init to recover", cfg.Port)
		return
	}

	// Our own managed server — safe to restart onto the new binary. Gated on MODE
	// (not just .pid) so a stale .pid under a reuse config can't trigger this.
	if cfg.Mode != "reuse" && config.FileExists(filepath.Join(cfg.MemoryDir, ".pid")) {
		progress.Logf("binary upgraded; restarting managed server (mode=%s, memory=%s, port=%d) on the new binary", cfg.Mode, cfg.MemoryDir, port)
		if err := server.Ensure(cfg.MemoryDir, port); err != nil {
			progress.Warnf("could not (re)start the local server after a binary upgrade; run /soul-init to recover: %v", err)
		}
		return
	}

	// Reuse mode, or a managed server that's down: don't assume it's ours.
	if extPID := procscan.PIDAtRoot(cfg.MemoryDir); extPID > 0 {
		progress.Warnf("binary upgraded, but the server at %s (pid=%d, mode=%s) is user-managed and still running the old binary; restart it yourself to pick up the new one", cfg.MemoryDir, extPID, cfg.Mode)
		return
	}

	progress.Logf("binary upgraded; starting down server (mode=%s, memory=%s, port=%d) on the new binary", cfg.Mode, cfg.MemoryDir, port)
	if err := server.Ensure(cfg.MemoryDir, port); err != nil {
		progress.Warnf("could not (re)start the local server after a binary upgrade; run /soul-init to recover: %v", err)
	}
}

// withProvisionLock serializes provisioning across processes (concurrent
// session starts would otherwise interleave binary installs). Held across
// downloads; ~180s bound covers a slow first-run download.
func withProvisionLock(fn func() error) error {
	lockDir, err := config.StatePath(".provision.lock")
	if err != nil {
		return err
	}
	return lockdir.WithLock(lockDir, 900, 200*time.Millisecond, fn)
}

// Provision runs the per-session sequence: config, binaries, server, seed.
// Progress goes to STDERR. Seeding runs after the lock: create-only publish
// is race-safe, and a wedged server must not serialize session starts.
func Provision() error {
	seedPort := 0
	if err := withProvisionLock(func() error {
		var err error
		seedPort, err = provisionLocked()
		return err
	}); err != nil {
		return err
	}
	if seedPort > 0 {
		seedMemoryDocs(seedPort)
	}
	return nil
}

// seedMemoryDocs seeds the local server; a missing token only skips seeding.
func seedMemoryDocs(port int) {
	tok, err := tokens.Read()
	if err != nil {
		progress.Warnf("could not read plugin token; skipping doc seeding: %v", err)
		return
	}
	cl := tokens.LocalClient()
	defer cl.Close()
	// No caller context: provisioning is bounded by the client's own timeouts.
	seed.MemoryDocs(context.Background(), cl, "localhost:"+strconv.Itoa(port), tok)
}

// provisionLocked does the lock-held provisioning work and returns the port to
// seed against, or 0 when seeding should be skipped.
func provisionLocked() (int, error) {
	cfg, err := config.LoadConfig()
	if err != nil {
		return 0, err
	}
	if cfg == nil {
		progress.Logf("no plugin config; running default setup")
		if err := initLocked("default", 0, ""); err != nil { // already holding the lock
			return 0, err
		}
		cfg, err = config.LoadConfig()
		if err != nil {
			return 0, err
		}
		if cfg == nil {
			return 0, fmt.Errorf("default setup completed but plugin-memory.conf is missing or invalid")
		}
	}

	replaced, err := release.EnsureBinaries()
	if err != nil {
		return 0, err
	}
	restartLocalServerOnUpgrade(replaced)

	port, perr := strconv.Atoi(strings.TrimSpace(cfg.Port))
	seedAfter := true
	switch cfg.Mode {
	case "default", "isolated":
		if perr != nil {
			return 0, fmt.Errorf("malformed PORT %q in plugin-memory.conf", cfg.Port)
		}
		if err := server.Ensure(cfg.MemoryDir, port); err != nil {
			return 0, err
		}
	case "reuse":
		if perr != nil {
			progress.Warnf("malformed PORT %q in plugin-memory.conf; skipping doc seeding", cfg.Port)
			seedAfter = false
		}
		if procscan.PIDAtRoot(cfg.MemoryDir) == 0 {
			progress.Warnf("configured to reuse server at %s but none is running; run /soul-init to reconfigure", cfg.MemoryDir)
			seedAfter = false // known down; don't pay the seeding dial retries
		}
	default:
		return 0, fmt.Errorf("unknown MODE in plugin-memory.conf: %s", cfg.Mode)
	}

	seed.CleanupLegacyTemplate(cfg.MemoryDir) // local-only; runs even when seeding is skipped
	progress.Logf("ready (mode=%s, memory=%s, port=%s)", cfg.Mode, cfg.MemoryDir, cfg.Port)
	if !seedAfter {
		port = 0
	}
	return port, nil
}

// Init runs setup for the given mode: "default" | "reuse" | "isolated".
// For reuse, port and root are required. For default/isolated they are ignored.
func Init(mode string, port int, root string) error {
	return withProvisionLock(func() error { return initLocked(mode, port, root) })
}

func initLocked(mode string, port int, root string) error {
	switch mode {
	case "default":
		return doDefault()
	case "isolated":
		return doIsolated()
	case "reuse":
		return doReuse(port, root)
	default:
		return fmt.Errorf("usage: init default | reuse --port N --root PATH | isolated")
	}
}

func doDefault() error {
	if _, err := release.EnsureBinaries(); err != nil {
		return err
	}
	if !procscan.PortIsFree(defaultPort) {
		progress.Logf("port %d is in use; falling back to isolated mode", defaultPort)
		return doIsolated()
	}
	memory, err := config.SharedMemoryDir()
	if err != nil {
		return err
	}
	return setupManaged("default", memory, defaultPort)
}

func doIsolated() error {
	if _, err := release.EnsureBinaries(); err != nil {
		return err
	}
	port, err := procscan.FindFreePortFrom(isolatedPortStart)
	if err != nil {
		return err
	}
	memory, err := config.IsolatedMemoryDir()
	if err != nil {
		return err
	}
	return setupManaged("isolated", memory, port)
}

// setupManaged mints the token, spawns or adopts the managed server for memory
// on port, proves the token authenticates end to end (setup success must mean
// writes work, not that files were written), then records the config.
func setupManaged(mode, memory string, port int) error {
	tokensTOML, err := tokens.PathFor(memory)
	if err != nil {
		return err
	}
	if err := tokens.Ensure(memory, tokensTOML); err != nil {
		return err
	}
	if err := server.Ensure(memory, port); err != nil {
		return err
	}
	if err := tokens.Verify(port); err != nil {
		return fmt.Errorf("plugin token does not authenticate against the managed server: %w; re-run /soul-init", err)
	}
	if err := config.SaveConfig(memory, port, mode, ""); err != nil {
		return err
	}
	progress.Logf("%s setup complete (memory=%s, port=%d)", mode, memory, port)
	return nil
}

func doReuse(port int, root string) error {
	if port <= 0 || root == "" {
		return fmt.Errorf("reuse requires --port N --root PATH")
	}
	if _, err := release.EnsureBinaries(); err != nil {
		return err
	}
	// Adopted external server: prefer the tokens.toml it actually reads over the
	// <root>/tokens.toml convention (divergence broke every write, 2026-08-27),
	// and validate everything BEFORE tokens.Ensure mutates any registry.
	tokensTOML := filepath.Join(root, "tokens.toml")
	prior, err := config.LoadConfig()
	if err != nil {
		return fmt.Errorf("load plugin config: %w", err)
	}
	if prior != nil && prior.Mode == "reuse" && prior.MemoryDir == root && prior.TokensTOML != "" {
		// A prior adoption resolved a custom registry; keep it when the server
		// is down rather than falling back to the convention.
		tokensTOML = prior.TokensTOML
	}
	targetPID, ran := procscan.PIDAtRootProbed(root)
	if !ran {
		return fmt.Errorf("cannot tell whether a demarkus-server is running at root %s: process discovery did not complete; re-run /soul-init", root)
	}
	if targetPID > 0 {
		targetArgs, argsRan := procscan.Args(targetPID)
		if !argsRan {
			return fmt.Errorf("cannot inspect the demarkus-server at root %s (pid %d): process discovery did not complete; re-run /soul-init", root, targetPID)
		}
		if targetArgs != "" {
			// Compare parsed values, not text: "06309" and "6309" are the same port.
			raw := procscan.PortOf(targetArgs, targetPID)
			if actual, err := strconv.Atoi(raw); err != nil || actual != port {
				return fmt.Errorf("the demarkus-server at root %s (pid %d) is listening on port %s, not %d; re-run with --port %s", root, targetPID, raw, port, raw)
			}
		}
		// An unidentified registry must stop here: tokens.Ensure would
		// otherwise write the conventional file the server never reads.
		discovered, known := procscan.TokensPathOf(targetArgs, targetPID)
		if !known {
			return fmt.Errorf("cannot tell which token registry the demarkus-server at root %s (pid %d) reads: environment discovery did not complete; re-run /soul-init", root, targetPID)
		}
		if discovered != "" && discovered != tokensTOML {
			// An explicit path that does not resolve is a hard stop: writing the
			// conventional file instead would mutate a registry the server never
			// reads (a flattened ps string can truncate paths with whitespace).
			if !config.FileExists(discovered) {
				return fmt.Errorf("the adopted server (pid %d) reads tokens from %s, but no such file exists; check its -tokens flag or DEMARKUS_TOKENS and re-run /soul-init", targetPID, discovered)
			}
			progress.Logf("adopted server (pid %d) reads tokens from %s, not the conventional %s; using it", targetPID, discovered, tokensTOML)
			tokensTOML = discovered
		}
	}
	if err := tokens.Ensure(root, tokensTOML); err != nil {
		return err
	}

	if targetPID > 0 {
		// Ask the adopted server to reload tokens. It may not be ours, but SIGHUP
		// is the documented mechanism and has no effect on other signal handlers.
		if err := tokens.SignalReload(targetPID); err != nil {
			progress.Warnf("%v; tokens may not be active until the server restarts", err)
		}
		// Prove the token authenticates end to end. A hash written to a file the
		// server never reads passes every local check and fails only at first
		// write; refuse to report success on that state.
		if err := tokens.Verify(port); err != nil {
			return fmt.Errorf("plugin token does not authenticate against the adopted server (tokens file: %s): %w; check the server's -tokens flag or DEMARKUS_TOKENS and re-run /soul-init", tokensTOML, err)
		}
	} else {
		progress.Warnf("no running server found with -root %s; proceeding with config, but /soul-init may need a rerun once it's up", root)
	}

	if err := config.SaveConfig(root, port, "reuse", tokensTOML); err != nil {
		return err
	}
	progress.Logf("reuse setup complete (memory=%s, port=%d)", root, port)
	return nil
}

// VerifyAuth reports whether the plugin token authenticates against the
// configured local server, naming the token registry the server actually reads.
// Read-only apart from the non-mutating ARCHIVE probe. (memory-doctor drift check.)
func VerifyAuth() (string, error) {
	cfg, err := config.LoadConfig()
	if err != nil {
		return "", err
	}
	if cfg == nil {
		return "no local memory configured (run /soul-init)", nil
	}
	port, err := strconv.Atoi(cfg.Port)
	if err != nil {
		return "", fmt.Errorf("invalid PORT %q in plugin-memory.conf", cfg.Port)
	}
	// Name the registry for the report: the running server's own -tokens or
	// DEMARKUS_TOKENS wins; otherwise the path provisioning would use.
	tokensTOML := ""
	pid, ran := procscan.PIDAtRootProbed(cfg.MemoryDir)
	if !ran {
		return fmt.Sprintf("cannot verify: process discovery did not complete for %s; re-run /soul-status", cfg.MemoryDir), nil
	}
	serverPort := ""
	if pid > 0 {
		args, argsRan := procscan.Args(pid)
		if !argsRan {
			return fmt.Sprintf("cannot verify: cannot inspect server pid %d; process discovery did not complete", pid), nil
		}
		// Falling through to cfg.TokensTOML or the default would name a registry
		// we could not read; say so instead.
		discovered, known := procscan.TokensPathOf(args, pid)
		if !known {
			return fmt.Sprintf("cannot verify: cannot tell which token registry server pid %d reads; environment discovery did not complete", pid), nil
		}
		tokensTOML = discovered
		serverPort = procscan.PortOf(args, pid)
	}
	if tokensTOML == "" {
		tokensTOML = cfg.TokensTOML // registry resolved at setup, if recorded
	}
	if tokensTOML == "" {
		if cfg.Mode == "reuse" {
			tokensTOML = filepath.Join(cfg.MemoryDir, "tokens.toml")
		} else {
			p, err := tokens.PathFor(cfg.MemoryDir)
			if err != nil {
				return "", fmt.Errorf("resolve token registry for %s: %w", cfg.MemoryDir, err)
			}
			tokensTOML = p
		}
	}
	if pid <= 0 {
		return fmt.Sprintf("cannot verify: no running demarkus-server for %s (expected token registry: %s)", cfg.MemoryDir, tokensTOML), nil
	}
	// A port mismatch means the probe would hit some other process, so any
	// verdict from it would describe the wrong server. Parsed comparison:
	// "06309" and "6309" are the same port.
	if sp, err := strconv.Atoi(serverPort); err != nil || sp != port {
		return fmt.Sprintf("cannot verify: server pid %d listens on port %s but the config says %s; re-run /soul-init", pid, serverPort, cfg.Port), nil
	}
	if err := tokens.Verify(port); err != nil {
		// Only a definitive unauthorized is drift; a transport failure means the
		// probe never reached a server and proves nothing about the token.
		if !errors.Is(err, tokens.ErrUnauthorized) {
			return fmt.Sprintf("cannot verify: %v (server pid %d, port %s)", err, pid, cfg.Port), nil
		}
		return fmt.Sprintf("token drift: plugin token does not authenticate (server pid %d, token registry %s); re-run /soul-init or update the registry hash", pid, tokensTOML), nil
	}
	return fmt.Sprintf("write auth healthy (server pid %d, token registry %s)", pid, tokensTOML), nil
}

// Status returns a human-readable one-line status of the configured server, or a
// note that no memory is configured.
func Status() (string, error) {
	cfg, err := config.LoadConfig()
	if err != nil {
		return "", err
	}
	if cfg == nil {
		return "no local memory configured (run /soul-init)", nil
	}
	w, err := HealthWarning()
	if err != nil {
		return "", err
	}
	verdict := "healthy"
	if w != "" {
		verdict = w
	}
	return fmt.Sprintf("mode=%s memory=%s port=%s: %s\ndesired=%s", cfg.Mode, cfg.MemoryDir, cfg.Port, verdict, release.DesiredVersions()), nil
}

// reuseDriftWarning reports an adopted server whose code is behind: a binary
// below the pinned floor, or a process predating the binary it was started from.
// Empty when current or undeterminable; never restarts a server it does not own.
func reuseDriftWarning(pid int) string {
	exe := procscan.Executable(pid)
	if exe == "" {
		return ""
	}
	if why := procscan.ExecRefusal(exe); why != "" {
		return fmt.Sprintf("cannot version-check the reused demarkus server (pid %d): its binary %s %s, so this plugin will not run it. Drift goes unreported until that is fixed.", pid, exe, why)
	}
	got := procscan.BinaryVersionAt(exe)
	// Replacement first: got describes the binary on disk, so once it has been
	// swapped the below-pin text would report a version nothing is serving.
	if w := replacedBinaryWarning(pid, exe, got); w != "" {
		return w
	}
	gotVersion, ordered := semver.Parse(got)
	pinned, _ := semver.Parse(release.ServerVersion)
	if ordered && gotVersion.Less(pinned) {
		return fmt.Sprintf("the reused demarkus server (pid %d, %s) is version %s, below the %s this plugin expects; features the plugin assumes may be missing. Upgrade that server, or run /soul-init to let the plugin manage one.", pid, exe, got, release.ServerVersion)
	}
	if !ordered {
		reported := "no version"
		if got != "" {
			reported = strconv.Quote(got)
		}
		return fmt.Sprintf("cannot version-check the reused demarkus server (pid %d): its binary %s reports %s rather than a release, so drift against %s goes unreported.", pid, exe, reported, release.ServerVersion)
	}
	return ""
}

// replacedBinaryWarning reports a process still serving code from memory after
// its binary was rewritten. Empty when current or undeterminable.
func replacedBinaryWarning(pid int, exe, got string) string {
	if !procscan.BinaryReplaced(pid, exe) {
		return ""
	}
	installed := "the installed build"
	if got != "" {
		installed = got
	}
	return fmt.Sprintf("the reused demarkus server (pid %d) started before its binary %s was replaced, so it is still serving the old code rather than %s. Restart that server to pick it up.", pid, exe, installed)
}

// HealthWarning echoes a one-line warning when the configured memory server is
// down or behind, empty when healthy. Probe-only (no QUIC round-trip): managed
// modes trust .pid liveness, reuse also drift-checks the adopted process.
func HealthWarning() (string, error) {
	cfg, err := config.LoadConfig()
	if err != nil {
		return "", err
	}
	if cfg == nil {
		return "", nil
	}
	switch cfg.Mode {
	case "default", "isolated":
		// Verify the recorded PID is genuinely OUR demarkus-server for this root,
		// not just any live process that reused the number — otherwise a stale
		// .pid would falsely report healthy while memory tools fail.
		switch procscan.ProbeAtRoot(procscan.ReadPID(filepath.Join(cfg.MemoryDir, ".pid")), cfg.MemoryDir) {
		case procscan.Unknown:
			return fmt.Sprintf("cannot tell whether the demarkus-memory server for %s is running: process discovery did not complete. Run /soul-status to retry.", cfg.MemoryDir), nil
		case procscan.NotOurs:
			return fmt.Sprintf("the demarkus-memory server is not running (no live process for %s). Memory tools (mark_fetch/mark_publish/mark_lookup/...) will fail until it restarts; run /soul-init to restart, or /soul-status to diagnose.", cfg.MemoryDir), nil
		}
	case "reuse":
		pid, ran := procscan.PIDAtRootProbed(cfg.MemoryDir)
		if !ran {
			return fmt.Sprintf("cannot tell whether the reused server rooted at %s is running: process discovery did not complete. Run /soul-status to retry.", cfg.MemoryDir), nil
		}
		if pid == 0 {
			return fmt.Sprintf("demarkus-memory is configured to reuse a server rooted at %s, but none is running. Memory tools will fail until it is started; start that server or run /soul-init to reconfigure.", cfg.MemoryDir), nil
		}
		if w := reuseDriftWarning(pid); w != "" {
			return w, nil
		}
	}
	return "", nil
}

// DetectServers reports running demarkus-server processes as "NO_SERVER" or
// "SERVERS\n<PID PORT ROOT>..." lines.
func DetectServers() (string, error) {
	results, ran := procscan.ListServers()
	if !ran {
		return "", errors.New("could not list processes: the discovery probe did not run")
	}
	if results == "" {
		return "NO_SERVER", nil
	}
	return "SERVERS\n" + results, nil
}
