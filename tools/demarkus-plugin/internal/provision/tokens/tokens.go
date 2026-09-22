// Package tokens mints and verifies the plugin's write token: the raw token
// file, its entry in the server's tokens.toml, the reload that activates it,
// and the auth probe that proves it works end to end.
package tokens

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/provision/procscan"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/registry/statefile"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/config"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/provision/progress"
	"github.com/latebit-io/demarkus/tools/internal/token"
)

const tokenLabel = "claude-code-plugin"

// tokenPaths is the scope minted for the plugin token. legacyTokenPaths is the
// pre-fix scope: path.Match's "*" does not cross "/", so it blocked writes to
// nested docs; entries still carrying it are revoked and reminted.
const (
	tokenPaths       = "/**"
	legacyTokenPaths = "/*"
)

// PathFor resolves the live tokens.toml for memoryDir: the out-of-root
// path once migrated, a legacy <memoryDir>/tokens.toml until then (the running
// server's -tokens flag still points there), the new path on fresh installs.
func PathFor(memoryDir string) (string, error) {
	newPath, err := config.ManagedTokensPath(memoryDir)
	if err != nil {
		return "", err
	}
	if config.FileExists(newPath) {
		return newPath, nil
	}
	// TODO: drop the legacy branch once released installs have migrated.
	if legacy := filepath.Join(memoryDir, "tokens.toml"); config.FileExists(legacy) {
		return legacy, nil
	}
	return newPath, nil
}

// MigrateManaged moves a legacy <memoryDir>/tokens.toml to the out-of-root
// path and returns the final path. Only call with the managed server stopped:
// a live server reads the legacy path.
func MigrateManaged(memoryDir string) (string, error) {
	newPath, err := config.ManagedTokensPath(memoryDir)
	if err != nil {
		return "", err
	}
	legacy := filepath.Join(memoryDir, "tokens.toml")
	if config.FileExists(newPath) {
		// Retry a legacy cleanup that failed after a successful copy on a
		// prior run; a leftover copy keeps token material in the content root.
		if config.FileExists(legacy) {
			if err := os.Remove(legacy); err != nil {
				return "", fmt.Errorf("remove leftover legacy tokens.toml: %w", err)
			}
		}
		return newPath, nil
	}
	if !config.FileExists(legacy) {
		return newPath, nil // fresh install: Ensure populates it
	}
	if err := os.MkdirAll(filepath.Dir(newPath), 0o700); err != nil {
		return "", err
	}
	if err := os.Rename(legacy, newPath); err != nil {
		// Cross-device fallback: project memories can sit on another volume.
		body, err := os.ReadFile(legacy)
		if err != nil {
			return "", fmt.Errorf("migrate tokens.toml out of memory root: %w", err)
		}
		if err := statefile.WriteFile(newPath, body, 0o600); err != nil {
			return "", fmt.Errorf("migrate tokens.toml out of memory root: %w", err)
		}
		if err := os.Remove(legacy); err != nil {
			return "", fmt.Errorf("remove legacy tokens.toml after copy: %w", err)
		}
	}
	progress.Logf("moved tokens.toml out of the memory root (%s)", newPath)
	return newPath, nil
}

// Ensure mints the plugin token (raw to plugin-memory.token, entry
// to tokensTOML, which may live outside root, the server root used to find a
// live server to reload). Idempotent when current; stale state is reminted.
func Ensure(root, tokensTOML string) error {
	tokenFile, err := config.TokenPath()
	if err != nil {
		return err
	}
	tokenBin, err := config.BinPath("demarkus-token")
	if err != nil {
		return err
	}

	entry, present, err := pluginEntry(tokensTOML)
	if err != nil {
		return err
	}
	if present && !scopeStale(&entry) && rawMatchesEntry(tokenFile, &entry) {
		// Reassert 0600 on the idempotent path: an existing token left
		// world/group-readable (e.g. from an older install) would otherwise stay
		// exposed forever since we return without regenerating.
		if err := os.Chmod(tokenFile, 0o600); err != nil {
			return fmt.Errorf("restrict token file %s: %w", tokenFile, err)
		}
		return nil
	}

	// Stale state — revoke any prior entry with our label before regenerating.
	// A failed revoke would make the generate below fail on a label collision,
	// so surface it here where the cause is clear.
	if present {
		revoke := exec.Command(tokenBin, "revoke", "-label", tokenLabel, "-tokens", tokensTOML)
		var stderr bytes.Buffer
		revoke.Stdout = io.Discard
		revoke.Stderr = &stderr
		if err := revoke.Run(); err != nil {
			return fmt.Errorf("revoke stale plugin token (label=%s, tokens.toml: %s): %w: %s", tokenLabel, tokensTOML, err, strings.TrimSpace(stderr.String()))
		}
	}

	if err := os.MkdirAll(filepath.Dir(tokensTOML), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(tokenFile), 0o755); err != nil {
		return err
	}

	// Generate under umask 077 so the raw-token temp file (and tokens.toml) are
	// created mode 600 from the start, never exposing the token to other local
	// users for the window before the chmod below.
	tmpTok := tokenFile + ".tmp"
	genErr := withUmask(0o077, func() error {
		tf, err := os.OpenFile(tmpTok, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		defer func() { _ = tf.Close() }()
		gen := exec.Command(tokenBin, "generate",
			"-label", tokenLabel,
			"-paths", tokenPaths,
			"-ops", "publish,archive",
			"-tokens", tokensTOML,
		)
		gen.Stdout = tf
		gen.Stderr = io.Discard
		return gen.Run()
	})
	if genErr != nil {
		_ = os.Remove(tmpTok)
		return fmt.Errorf("demarkus-token generate failed (tokens.toml: %s): %w", tokensTOML, genErr)
	}

	// Reload before committing the raw token: a matching token file must imply
	// the server loaded it, so a failed reload leaves no file and the next run
	// remints.
	if pid := findServerAtRoot(root); pid > 0 {
		if err := reloadServer(pid); err != nil {
			_ = os.Remove(tmpTok) // best-effort; a leftover .tmp never satisfies the gate
			return fmt.Errorf("reload reminted token: %w", err)
		}
	}

	if err := os.Rename(tmpTok, tokenFile); err != nil {
		return err
	}
	if err := os.Chmod(tokenFile, 0o600); err != nil {
		return err
	}
	progress.Logf("generated plugin token (label=%s)", tokenLabel)
	return nil
}

// Test seams: reload-path coverage stubs these without a live server.
var (
	findServerAtRoot = procscan.PIDAtRoot
	reloadServer     = SignalReload
)

// rawMatchesEntry reports whether the raw token on disk hashes to the entry's
// hash. False on a missing or unreadable file: an inconsistent pair (e.g.
// leftovers of a failed run) must fall through to a remint.
func rawMatchesEntry(tokenFile string, entry *token.Entry) bool {
	raw, err := os.ReadFile(tokenFile)
	if err != nil {
		return false
	}
	return protocol.HashToken(strings.TrimSpace(string(raw))) == entry.Hash
}

// SignalReload SIGHUPs a server so it reloads tokens.toml. A vanished process
// is fine: the respawn path loads the fresh table.
func SignalReload(pid int) error {
	err := procscan.Signal(pid, syscall.SIGHUP)
	switch {
	case err == nil:
		progress.Logf("sent SIGHUP to server (pid=%d) to reload tokens", pid)
		return nil
	case procscan.AlreadyExited(err):
		return nil
	default:
		return fmt.Errorf("SIGHUP to server pid %d: %w", pid, err)
	}
}

// pluginEntry parses tokensTOML and returns the plugin's entry and whether it
// exists. A missing file is absence, not an error. Parse errors surface: an
// uninterpretable tokens.toml must not pass as current (or be clobbered).
func pluginEntry(tokensTOML string) (token.Entry, bool, error) {
	f, err := token.ReadFile(tokensTOML)
	if errors.Is(err, os.ErrNotExist) {
		return token.Entry{}, false, nil
	}
	if err != nil {
		return token.Entry{}, false, fmt.Errorf("inspect plugin token entry: %w", err)
	}
	entry, ok := f.Tokens[tokenLabel]
	return entry, ok, nil
}

// scopeStale reports whether an entry still carries the legacy scope without
// the current one. A manually widened entry listing both is left alone.
func scopeStale(entry *token.Entry) bool {
	return slices.Contains(entry.Paths, legacyTokenPaths) && !slices.Contains(entry.Paths, tokenPaths)
}

// withUmask runs fn with the process umask set to mask, restoring it after.
// syscall.Umask is process-global; provisioning is single-threaded per session
// so this is safe here.
func withUmask(mask int, fn func() error) error {
	old := syscall.Umask(mask)
	defer syscall.Umask(old)
	return fn()
}

// Read loads and trims the plugin's raw token.
func Read() (string, error) {
	tokenFile, err := config.TokenPath()
	if err != nil {
		return "", err
	}
	raw, err := os.ReadFile(tokenFile)
	if err != nil {
		return "", fmt.Errorf("read plugin token: %w", err)
	}
	return strings.TrimSpace(string(raw)), nil
}

// LocalClient builds the client for localhost session-start calls. Tight
// timeouts: the client retries transient errors 5x, so a down server still
// costs ~5s of dial timeouts (vs ~50s on the defaults); a wedged one ~25s/op.
func LocalClient() *fetch.Client {
	return fetch.NewClient(fetch.Options{Insecure: true, DialTimeout: time.Second, RequestTimeout: 5 * time.Second})
}

// AuthProbeClient is the one-verb surface the auth probe needs;
// *fetch.Client satisfies it.
type AuthProbeClient interface {
	Append(ctx context.Context, r fetch.WriteRequest) (fetch.Result, error)
}

// DialProbe opens the auth probe's client and returns its close; tests
// substitute a canned server.
var DialProbe = func() (AuthProbeClient, func()) {
	cl := LocalClient()
	return cl, cl.Close
}

// probeExpectedVersion can never match a real document, so the probe APPEND
// always ends in not-found or a version conflict, never a write.
const probeExpectedVersion = 1 << 30

// Verify probes auth with an APPEND that cannot mutate anything
// (auth is checked before the store is touched). Unauthorized retries briefly:
// the server reloads tokens asynchronously after SIGHUP, so a remint can race.
func Verify(port int) error {
	tok, err := Read()
	if err != nil {
		return err
	}
	cl, done := DialProbe()
	defer done()
	host := "localhost:" + strconv.Itoa(port)
	for attempt := 0; ; attempt++ {
		// No caller context: the probe is bounded by the client's own timeouts.
		err = verifyTokenWith(context.Background(), cl, host, tok)
		if !errors.Is(err, ErrUnauthorized) || attempt >= 3 {
			return err
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// ErrUnauthorized distinguishes a definitive auth rejection from transport
// failures, which callers must not report as token drift.
var ErrUnauthorized = errors.New("auth probe returned unauthorized")

func verifyTokenWith(ctx context.Context, cl AuthProbeClient, host, tok string) error {
	res, err := cl.Append(ctx, fetch.WriteRequest{
		Host: host, Path: "/.demarkus-plugin-auth-probe.md", Token: tok,
		Body: "probe", ExpectedVersion: probeExpectedVersion,
	})
	if err != nil {
		return fmt.Errorf("auth probe: %w", err)
	}
	if res.Response.Status == protocol.StatusUnauthorized {
		return ErrUnauthorized
	}
	return nil
}
