// Package provision ports the demarkus-memory server-lifecycle logic from the
// plugins' bash (lib.sh + setup.sh + session-start.sh + detect-soul.sh) into Go.
// It owns the per-session managed-server lifecycle: download/verify/install the
// pinned binaries, generate the plugin token, spawn (or adopt) a demarkus-server,
// and seed the soul docs. Every readiness/progress line goes to STDERR so a
// caller can run Provision purely for its side effects with a clean stdout.
//
// Faithful port note: the bash carries hard-won safety — notably the PID-at-root
// ownership check that gates ever killing a recorded .pid (a stale .pid whose
// number has been reused by an unrelated process must never be killed). That
// safety is preserved here verbatim in pidIsServerAtRoot / ensureManagedServer.
package provision

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/config"
)

// Version pins for the SEPARATE server/client modules (their own release
// cadence; the bump workflow tracks their latest existing releases).
const (
	serverVersion = "0.21.0"
	clientVersion = "0.21.1"
	// fallbackToolsVersion is used ONLY by dev builds (Version == "dev"), where
	// there's no real release to derive the tools version from. A real release
	// uses its own ldflags Version — see toolsRef.
	fallbackToolsVersion = "0.12.1"
)

// Version is the binary's own release version, injected from main's ldflags
// value. demarkus-plugin ships INSIDE the tools release, so the running binary's
// version is exactly the tools release to fetch demarkus-token from. provision
// never re-downloads demarkus-plugin itself (bootstrap.sh owns that), which is
// what avoids the self-referential version skew where a binary would download an
// older copy of itself.
var Version = "dev"

var semverRe = regexp.MustCompile(`^\d+\.\d+\.\d+$`)

// toolsRef is the tools release to pull demarkus-token from: the binary's own
// version when it's a real release, else the dev fallback.
func toolsRef() string {
	if semverRe.MatchString(Version) {
		return Version
	}
	return fallbackToolsVersion
}

// Port/range constants (lib.sh).
const (
	demarkusProtocolPort = 6309  // demarkus-server's built-in default
	defaultPort          = 6310  // plugin's first-choice managed port
	isolatedPortStart    = 16310 // isolated range start
	isolatedPortEnd      = 16509 // isolated range end
)

const tokenLabel = "claude-code-plugin"

//go:embed seed/index.md seed/project-template.md
var seedFS embed.FS

// log writes a progress line to stderr, matching the bash "[demarkus-memory] …".
func logf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "[demarkus-memory] "+format+"\n", a...)
}

func warnf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "[demarkus-memory] warning: "+format+"\n", a...)
}

// --- path helpers (lib.sh readonly PLUGIN_* paths) ---------------------------

func pluginBinDir() (string, error)  { return underHome("bin") }
func pluginConfig() (string, error)  { return underHome("plugin-memory.conf") }
func pluginToken() (string, error)   { return underHome("plugin-memory.token") }
func sharedSoulDir() (string, error) { return underHome("soul") }
func isolatedSoulDir() (string, error) {
	return underHome("plugin-soul")
}

func underHome(name string) (string, error) {
	h, err := config.Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(h, name), nil
}

func binPath(name string) (string, error) {
	d, err := pluginBinDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, name), nil
}

// --- detectPlatform (lib.sh detect_platform) ---------------------------------

// detectPlatform maps GOOS/GOARCH onto the release artifact suffix, e.g.
// "darwin_arm64". Errors on an unsupported OS/arch (the bash `die`). The bash
// reads `uname -s`/`uname -m`; GOOS/GOARCH carry the same information in Go's
// canonical form (darwin/linux, arm64/amd64), so the mapping is direct.
func detectPlatform() (string, error) {
	var osName string
	switch runtime.GOOS {
	case "darwin":
		osName = "darwin"
	case "linux":
		osName = "linux"
	default:
		return "", fmt.Errorf("unsupported OS: %s", runtime.GOOS)
	}
	var arch string
	switch runtime.GOARCH {
	case "arm64":
		arch = "arm64"
	case "amd64":
		arch = "amd64"
	default:
		return "", fmt.Errorf("unsupported arch: %s", runtime.GOARCH)
	}
	return osName + "_" + arch, nil
}

// --- sha256Verify (lib.sh sha256_verify) -------------------------------------

// sha256Verify checks that archive's SHA-256 matches the entry for its basename
// in a GNU-coreutils-style checksums file ("<hex>  <name>"). Mirrors the bash
// grep " name$" → first column.
func sha256Verify(checksumsFile, archive string) error {
	cb, err := os.ReadFile(checksumsFile)
	if err != nil {
		return err
	}
	name := filepath.Base(archive)
	expected := ""
	for ln := range strings.SplitSeq(string(cb), "\n") {
		fields := strings.Fields(ln)
		if len(fields) >= 2 && fields[len(fields)-1] == name {
			expected = fields[0]
			break
		}
	}
	if expected == "" {
		return fmt.Errorf("no checksum entry for %s", name)
	}
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	actual := hex.EncodeToString(h.Sum(nil))
	if !strings.EqualFold(actual, expected) {
		return fmt.Errorf("checksum mismatch for %s (expected %s, got %s)", name, expected, actual)
	}
	return nil
}

// --- config load/save (lib.sh load_config/save_config) -----------------------

// loadConfig returns the saved SOUL_DIR/PORT/MODE, or nil when the config file
// is absent. config.LoadConfig already parses the shell-style file (handling
// printf %q quoting); reuse it rather than reimplement.
func loadConfig() (*config.PluginConfig, error) {
	return config.LoadConfig()
}

// saveConfig atomically writes plugin-memory.conf. Values are backslash-escaped
// (shellQuote), equivalent to bash printf '%q' for the simple path/word values we
// store, and reversed by config.unquoteShell on load.
func saveConfig(soul string, port int, mode string) error {
	h, err := config.Home()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(h, 0o755); err != nil {
		return err
	}
	cfg, err := pluginConfig()
	if err != nil {
		return err
	}
	content := fmt.Sprintf(
		"# demarkus-memory plugin config: managed by demarkus-plugin provision\nSOUL_DIR=%s\nPORT=%d\nMODE=%s\n",
		shellQuote(soul), port, shellQuote(mode),
	)
	tmp := cfg + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, cfg)
}

// shellQuote backslash-escapes the shell-special characters in s, matching what
// bash printf '%q' emits for the simple path/word values we store (and what
// config.unquoteShell reverses: a plain backslash-unescape of the un-wrapped
// value). A value with no special characters is left bare. We deliberately use
// the backslash form rather than '…' wrapping because config.unquoteShell does a
// naive backslash-unescape and does not understand the '\” single-quote idiom.
func shellQuote(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\n'\"\\$`*?[]{}()&;|<>#~!") {
		return s
	}
	var b strings.Builder
	for _, r := range s {
		if strings.ContainsRune(" \t\n'\"\\$`*?[]{}()&;|<>#~!", r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// --- version drift (lib.sh _installed_versions/_desired_versions) ------------

// desiredVersions is the pin string provision expects, in the bash shape. It
// covers only the binaries provision manages — server, mcp, token. demarkus-plugin
// is deliberately excluded: bootstrap.sh installs/updates it, and having provision
// re-fetch it (against a pin that can't match the auto-assigned release tag)
// caused a self-downgrade cascade.
func desiredVersions() string {
	return fmt.Sprintf("server=%s,client=%s,tools=%s",
		serverVersion, clientVersion, toolsRef())
}

// installedVersions queries each on-disk binary via --version and assembles the
// same shape as desiredVersions. A missing/unexecutable/too-old binary yields an
// empty field, which never equals a desired version, so ensureBinaries
// re-downloads. demarkus-token accepts --version as well as its `version`
// subcommand.
func installedVersions() string {
	return fmt.Sprintf("server=%s,client=%s,tools=%s",
		binaryVersion("demarkus-server"),
		binaryVersion("demarkus-mcp"),
		binaryVersion("demarkus-token"),
	)
}

// binaryVersion echoes the version BIN reports via --version, or "" if BIN is
// missing, not executable, or errors (a pre-version binary errors on the unknown
// flag). stderr is discarded so an old binary's noise can't leak in.
func binaryVersion(name string) string {
	p, err := binPath(name)
	if err != nil {
		return ""
	}
	info, err := os.Stat(p)
	if err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
		return ""
	}
	cmd := exec.Command(p, "--version")
	cmd.Stderr = io.Discard
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// --- binary download/install (lib.sh ensure_binaries) ------------------------

const (
	serverReleaseBase = "https://github.com/latebit-io/demarkus/releases/download/server%2Fv" + serverVersion
	clientReleaseBase = "https://github.com/latebit-io/demarkus/releases/download/client%2Fv" + clientVersion
)

// toolsReleaseBase is the release that ships demarkus-token (the same one this
// binary came from), resolved at runtime from toolsRef.
func toolsReleaseBase() string {
	return "https://github.com/latebit-io/demarkus/releases/download/tools%2Fv" + toolsRef()
}

// ensureBinaries downloads + installs the pinned binaries to ~/.demarkus/bin
// whenever the installed binaries don't report exactly the pinned versions
// (queried live via --version). Returns whether it actually swapped a binary
// this run (the bash DEMARKUS_BINARIES_REPLACED), which restartLocalServerOnUpgrade
// reads to decide whether to restart the running server onto the new binary.
func ensureBinaries() (replaced bool, err error) {
	desired := desiredVersions()
	installed := installedVersions()
	if installed == desired {
		return false, nil
	}

	binDir, err := pluginBinDir()
	if err != nil {
		return false, err
	}
	// Distinguish a stale install from a fresh one for the log line only.
	if anyBinaryPresent() {
		logf("binary version drift detected (installed=%s, desired=%s); re-downloading", installed, desired)
	}
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return false, err
	}

	plat, err := detectPlatform()
	if err != nil {
		return false, err
	}

	tmp, err := os.MkdirTemp("", "demarkus-binaries-")
	if err != nil {
		return false, err
	}
	defer func() { _ = os.RemoveAll(tmp) }()

	logf("downloading demarkus binaries (server v%s, client v%s, tools v%s, %s)",
		serverVersion, clientVersion, toolsRef(), plat)

	// server archive + checksums.
	serverArchive := fmt.Sprintf("demarkus-server_%s_%s.tar.gz", serverVersion, plat)
	if err := download(serverReleaseBase+"/"+serverArchive, filepath.Join(tmp, serverArchive)); err != nil {
		return false, fmt.Errorf("download %s: %w", serverArchive, err)
	}
	if err := download(serverReleaseBase+"/demarkus-server_checksums.txt", filepath.Join(tmp, "server_checksums.txt")); err != nil {
		return false, fmt.Errorf("download server checksums: %w", err)
	}
	if err := sha256Verify(filepath.Join(tmp, "server_checksums.txt"), filepath.Join(tmp, serverArchive)); err != nil {
		return false, err
	}
	if err := extractTarGz(filepath.Join(tmp, serverArchive), tmp); err != nil {
		return false, err
	}

	// mcp archive + checksums.
	mcpArchive := fmt.Sprintf("demarkus-mcp_%s_%s.tar.gz", clientVersion, plat)
	if err := download(clientReleaseBase+"/"+mcpArchive, filepath.Join(tmp, mcpArchive)); err != nil {
		return false, fmt.Errorf("download %s: %w", mcpArchive, err)
	}
	if err := download(clientReleaseBase+"/demarkus-client_checksums.txt", filepath.Join(tmp, "client_checksums.txt")); err != nil {
		return false, fmt.Errorf("download client checksums: %w", err)
	}
	if err := sha256Verify(filepath.Join(tmp, "client_checksums.txt"), filepath.Join(tmp, mcpArchive)); err != nil {
		return false, err
	}
	if err := extractTarGz(filepath.Join(tmp, mcpArchive), tmp); err != nil {
		return false, err
	}

	// token archive (tools release) + tools checksums. demarkus-token ships in the
	// same tools release as this binary, so toolsRef() (the running binary's own
	// version) is exactly the release that has a matching token. demarkus-plugin is
	// NOT fetched here — bootstrap.sh installs it; provision re-fetching itself is
	// the self-downgrade bug this fix removes.
	base := toolsReleaseBase()
	tokenArchive := fmt.Sprintf("demarkus-token_%s_%s.tar.gz", toolsRef(), plat)
	if err := download(base+"/"+tokenArchive, filepath.Join(tmp, tokenArchive)); err != nil {
		return false, fmt.Errorf("download %s: %w", tokenArchive, err)
	}
	if err := download(base+"/demarkus-tools_checksums.txt", filepath.Join(tmp, "tools_checksums.txt")); err != nil {
		return false, fmt.Errorf("download tools checksums: %w", err)
	}
	if err := sha256Verify(filepath.Join(tmp, "tools_checksums.txt"), filepath.Join(tmp, tokenArchive)); err != nil {
		return false, err
	}
	if err := extractTarGz(filepath.Join(tmp, tokenArchive), tmp); err != nil {
		return false, err
	}

	// Install 0755 to the bin dir (bash install -m 0755).
	for _, name := range []string{"demarkus-server", "demarkus-token", "demarkus-mcp"} {
		if err := installFile(filepath.Join(tmp, name), filepath.Join(binDir, name)); err != nil {
			return false, fmt.Errorf("install %s: %w", name, err)
		}
	}

	logf("binaries installed to %s", binDir)
	return true, nil
}

func anyBinaryPresent() bool {
	for _, name := range []string{"demarkus-server", "demarkus-mcp", "demarkus-token"} {
		if p, err := binPath(name); err == nil {
			if info, e := os.Stat(p); e == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
				return true
			}
		}
	}
	return false
}

// download fetches url to dest with connect/total timeouts equivalent to the
// bash curl --connect-timeout 10 --max-time 300, following redirects (GitHub
// release downloads redirect to a CDN). Writes via a temp file + rename.
func download(url, dest string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 300 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: status %s", url, resp.Status)
	}
	tmp := dest + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dest)
}

// extractTarGz extracts a .tar.gz into destDir. Only regular files are written
// (the archives are flat single binaries); paths are sanitized against traversal.
func extractTarGz(archive, destDir string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		clean := filepath.Clean(hdr.Name)
		if strings.HasPrefix(clean, "..") || filepath.IsAbs(clean) {
			return fmt.Errorf("unsafe path in archive: %s", hdr.Name)
		}
		target := filepath.Join(destDir, clean)
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(hdr.Mode)&0o777)
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, tr); err != nil { //nolint:gosec // trusted, sha256-verified archive
				_ = out.Close()
				return err
			}
			if err := out.Close(); err != nil {
				return err
			}
		}
	}
	return nil
}

// installFile copies src to dst with mode 0755 (atomic via temp + rename).
func installFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

// --- token (lib.sh ensure_token_entry) ---------------------------------------

// ensureTokenEntry generates a plugin-scoped token, writes the raw token to
// ~/.demarkus/plugin-memory.token (mode 0600), and appends an entry for
// tokenLabel to tokensTOML. Idempotent: a no-op when the token file and a
// matching tokens.toml entry already exist. Stale state (token file gone but the
// label still present) is revoked before regenerating.
func ensureTokenEntry(tokensTOML string) error {
	tokenFile, err := pluginToken()
	if err != nil {
		return err
	}
	tokenBin, err := binPath("demarkus-token")
	if err != nil {
		return err
	}

	labelPresent := tokensHasLabel(tokensTOML)
	if fileExists(tokenFile) && fileExists(tokensTOML) && labelPresent {
		// Reassert 0600 on the idempotent path: an existing token left
		// world/group-readable (e.g. from an older install) would otherwise stay
		// exposed forever since we return without regenerating.
		_ = os.Chmod(tokenFile, 0o600)
		return nil
	}

	// Stale state — revoke any prior entry with our label before regenerating.
	if labelPresent {
		revoke := exec.Command(tokenBin, "revoke", "-label", tokenLabel, "-tokens", tokensTOML)
		revoke.Stdout = io.Discard
		revoke.Stderr = io.Discard
		_ = revoke.Run() // best-effort
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
			"-paths", "/*",
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

	if err := os.Rename(tmpTok, tokenFile); err != nil {
		return err
	}
	if err := os.Chmod(tokenFile, 0o600); err != nil {
		return err
	}
	logf("generated plugin token (label=%s)", tokenLabel)
	return nil
}

// tokensHasLabel reports whether tokensTOML contains a "[tokens.<label>]"
// section (bash grep "tokens\.LABEL\]").
func tokensHasLabel(tokensTOML string) bool {
	b, err := os.ReadFile(tokensTOML)
	if err != nil {
		return false
	}
	return strings.Contains(string(b), "tokens."+tokenLabel+"]")
}

// withUmask runs fn with the process umask set to mask, restoring it after.
// syscall.Umask is process-global; provisioning is single-threaded per session
// so this is safe here.
func withUmask(mask int, fn func() error) error {
	old := syscall.Umask(mask)
	defer syscall.Umask(old)
	return fn()
}

// --- port checks (lib.sh port_is_free/find_free_port_from) -------------------

// portIsFree reports whether nothing appears to own UDP port. demarkus is QUIC
// (UDP); a successful bind of the UDP port (then immediate close) is the probe.
// A bind that fails with "address in use" means taken; any other bind failure is
// treated as free (permissive, matching the bash degrade-to-permissive behavior)
// — the server spawn's post-spawn probe catches a genuine conflict loudly.
func portIsFree(port int) bool {
	conn, err := net.ListenPacket("udp", net.JoinHostPort("", strconv.Itoa(port)))
	if err != nil {
		if errIsAddrInUse(err) {
			return false
		}
		return true // permissive on any other probe failure
	}
	_ = conn.Close()
	return true
}

// errIsAddrInUse reports whether err is an EADDRINUSE bind failure.
func errIsAddrInUse(err error) bool {
	return errors.Is(err, syscall.EADDRINUSE)
}

// findFreePortFrom returns the first apparently-free UDP port in [start, start+199].
func findFreePortFrom(start int) (int, error) {
	end := start + 199
	for p := start; p <= end; p++ {
		if portIsFree(p) {
			return p, nil
		}
	}
	return 0, fmt.Errorf("no free UDP port in %d..%d", start, end)
}

// --- process scan (lib.sh pid_of_server_at_root / _pid_is_server_at_root / find_running_demarkus) ---

var (
	rootFlagRe = regexp.MustCompile(`-root[= ]+([^ ]+)`)
	portFlagRe = regexp.MustCompile(`-port[= ]+(\d+)`)
)

// pidOfServerAtRoot returns the PID of a demarkus-server whose -root flag (or
// DEMARKUS_ROOT env) equals root (literal match), or 0 when none match.
func pidOfServerAtRoot(root string) int {
	for _, pid := range pgrepDemarkusServer() {
		args := psArgs(pid)
		if argsRootMatches(args, root) {
			return pid
		}
		if procEnv(pid, "DEMARKUS_ROOT") == root {
			return pid
		}
	}
	return 0
}

// pidIsServerAtRoot reports whether pid is a LIVE demarkus-server whose -root
// flag (or DEMARKUS_ROOT env) equals root. This is the ownership check that makes
// reusing/killing a recorded .pid safe: a stale .pid whose number has been reused
// by an unrelated process must never be treated as our managed server.
func pidIsServerAtRoot(pid int, root string) bool {
	if pid <= 0 {
		return false
	}
	if !pidAlive(pid) {
		return false
	}
	args := psArgs(pid)
	if !strings.Contains(args, "demarkus-server") {
		return false
	}
	if argsRootMatches(args, root) {
		return true
	}
	return procEnv(pid, "DEMARKUS_ROOT") == root
}

// argsRootMatches mirrors the bash literal substring match for "-root TARGET" /
// "-root=TARGET" at a word boundary, handling paths with regex metacharacters or
// spaces safely (literal compare, not regex).
func argsRootMatches(args, target string) bool {
	for _, form := range []string{" -root " + target, " -root=" + target} {
		if strings.Contains(args, form+" ") || strings.HasSuffix(args, form) {
			return true
		}
	}
	return false
}

// findRunningDemarkus emits "PID PORT ROOT" per running demarkus-server, one per
// line (empty when none). Args take precedence over env; falls back to
// DEMARKUS_PORT/DEMARKUS_ROOT env, then the protocol default port / "(unknown)" root.
func findRunningDemarkus() string {
	var lines []string
	for _, pid := range pgrepDemarkusServer() {
		args := psArgs(pid)
		if args == "" {
			continue
		}
		port := ""
		if m := portFlagRe.FindStringSubmatch(args); m != nil {
			port = m[1]
		}
		if port == "" {
			port = procEnv(pid, "DEMARKUS_PORT")
		}
		if port == "" {
			port = strconv.Itoa(demarkusProtocolPort)
		}
		root := ""
		if m := rootFlagRe.FindStringSubmatch(args); m != nil {
			root = m[1]
		}
		if root == "" {
			root = procEnv(pid, "DEMARKUS_ROOT")
		}
		if root == "" {
			root = "(unknown)"
		}
		lines = append(lines, fmt.Sprintf("%d %s %s", pid, port, root))
	}
	return strings.Join(lines, "\n")
}

// portOfRunningServer returns the port reported by findRunningDemarkus for pid,
// or "" if not found. Used by reuse to validate the adopted server's actual port.
func portOfRunningServer(pid int) string {
	for ln := range strings.SplitSeq(findRunningDemarkus(), "\n") {
		fields := strings.Fields(ln)
		if len(fields) >= 2 && fields[0] == strconv.Itoa(pid) {
			return fields[1]
		}
	}
	return ""
}

// --- managed server (lib.sh managed_server_current / ensure_managed_server) --

// managedServerCurrent reports whether the managed server recorded in pidFile is
// alive, is genuinely OUR demarkus-server for root (not a reused PID), AND the
// binary version stamped in versionFile matches the current serverVersion pin.
func managedServerCurrent(pidFile, versionFile, root string) bool {
	pid := readPID(pidFile)
	if pid <= 0 {
		return false
	}
	if !pidIsServerAtRoot(pid, root) {
		return false
	}
	ver, _ := os.ReadFile(versionFile)
	return strings.TrimSpace(string(ver)) == serverVersion
}

// ensureManagedServer spawns a demarkus-server for soulDir on port if ours isn't
// already running and current. Writes the PID to <soulDir>/.pid and the spawned
// binary's version to <soulDir>/.server-version. For default/isolated modes only.
//
// CRITICAL PID-at-root kill safety: a recorded PID is only killed once confirmed
// to be genuinely OUR demarkus-server for this root. A dead/garbage PID just gets
// its bookkeeping files cleared; a live PID that is NOT our server (stale .pid,
// number reused by an unrelated process) is left untouched.
func ensureManagedServer(soulDir string, port int) error {
	pidFile := filepath.Join(soulDir, ".pid")
	versionFile := filepath.Join(soulDir, ".server-version")
	logFile := filepath.Join(soulDir, ".log")

	if managedServerCurrent(pidFile, versionFile, soulDir) {
		return nil
	}

	// Not reusable. Stop a live-but-stale managed server before respawning — but
	// only after confirming it is genuinely ours for this root.
	runningPID := readPID(pidFile)
	if pidIsServerAtRoot(runningPID, soulDir) {
		logf("restarting managed demarkus-server onto upgraded binary (target=%s)", serverVersion)
		_ = signalPID(runningPID, syscall.SIGTERM)
		// Bounded wait for exit (~3s), then escalate to SIGKILL so the respawn
		// can bind the freed UDP port.
		for waited := 0; waited < 30 && pidAlive(runningPID); waited++ {
			time.Sleep(100 * time.Millisecond)
		}
		if pidAlive(runningPID) {
			_ = signalPID(runningPID, syscall.SIGKILL)
		}
	} else if runningPID > 0 && pidAlive(runningPID) {
		warnf("recorded pid %d is live but is not the demarkus-server for %s (stale .pid, reused PID); leaving it alone and clearing our bookkeeping", runningPID, soulDir)
	}
	_ = os.Remove(pidFile)
	_ = os.Remove(versionFile)

	if err := os.MkdirAll(soulDir, 0o755); err != nil {
		return err
	}

	serverBin, err := binPath("demarkus-server")
	if err != nil {
		return err
	}
	lf, err := os.OpenFile(logFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer func() { _ = lf.Close() }()

	cmd := exec.Command(serverBin,
		"-root", soulDir,
		"-port", strconv.Itoa(port),
		"-tokens", filepath.Join(soulDir, "tokens.toml"),
	)
	cmd.Stdout = lf
	cmd.Stderr = lf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // detach into its own session
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("spawn demarkus-server: %w", err)
	}
	pid := cmd.Process.Pid
	// If we can't record ownership bookkeeping, kill the just-spawned server
	// rather than leave a detached process with no valid .pid/.server-version
	// (which a later run can neither recognize nor safely manage).
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(pid)+"\n"), 0o644); err != nil {
		_ = cmd.Process.Kill()
		_ = os.Remove(pidFile)
		return err
	}
	if err := os.WriteFile(versionFile, []byte(serverVersion+"\n"), 0o644); err != nil {
		_ = cmd.Process.Kill()
		_ = os.Remove(pidFile)
		_ = os.Remove(versionFile)
		return err
	}
	_ = cmd.Process.Release() // fully detach; don't reap

	// Bounded poll: fail fast if the process died (bind/startup error), succeed
	// once the port is observed bound, or accept once the attempt cap is reached
	// with the process still alive (permissive when the port probe is unavailable).
	const maxAttempts = 20 // ~2s at 100ms
	for range maxAttempts {
		if !pidAlive(pid) {
			_ = os.Remove(pidFile)
			_ = os.Remove(versionFile)
			tailInfo := ""
			if t := tailFile(logFile, 5); t != "" {
				tailInfo = "\nrecent log:\n" + t
			}
			return fmt.Errorf("demarkus-server failed to start (port %d may be in use; re-run /soul-init)%s", port, tailInfo)
		}
		if !portIsFree(port) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	logf("spawned demarkus-server (pid=%d, port=%d, root=%s)", pid, port, soulDir)
	return nil
}

// restartLocalServerOnUpgrade, when ensureBinaries swapped the binary this run,
// makes the configured local server run on the new binary using its OWN recorded
// config. No-op when no binary changed or no local soul is configured.
//
// Ownership is the MODE, not a .pid file:
//   - default/isolated: the server is ours → ensureManagedServer restarts it.
//   - reuse: the server is the user's, ALWAYS. A .pid is not proof of ownership
//     (a reuse config can carry a stale .pid), so reuse never takes the managed
//     path: a running server is left alone (warned it's on the old binary), and
//     only a DOWN one is started on the new binary with the recorded config.
//
// Never fails the caller: a restart problem is warned, not propagated.
func restartLocalServerOnUpgrade(replaced bool) {
	if !replaced {
		return
	}
	cfg, err := loadConfig()
	if err != nil || cfg == nil {
		return
	}
	port, err := strconv.Atoi(strings.TrimSpace(cfg.Port))
	if err != nil {
		warnf("could not parse recorded PORT %q for upgrade restart; run /soul-init to recover", cfg.Port)
		return
	}

	// Our own managed server — safe to restart onto the new binary. Gated on MODE
	// (not just .pid) so a stale .pid under a reuse config can't trigger this.
	if cfg.Mode != "reuse" && fileExists(filepath.Join(cfg.SoulDir, ".pid")) {
		logf("binary upgraded; restarting managed server (mode=%s, soul=%s, port=%d) on the new binary", cfg.Mode, cfg.SoulDir, port)
		if err := ensureManagedServer(cfg.SoulDir, port); err != nil {
			warnf("could not (re)start the local server after a binary upgrade; run /soul-init to recover: %v", err)
		}
		return
	}

	// Reuse mode, or a managed server that's down: don't assume it's ours.
	if extPID := pidOfServerAtRoot(cfg.SoulDir); extPID > 0 {
		warnf("binary upgraded, but the server at %s (pid=%d, mode=%s) is user-managed and still running the old binary; restart it yourself to pick up the new one", cfg.SoulDir, extPID, cfg.Mode)
		return
	}

	logf("binary upgraded; starting down server (mode=%s, soul=%s, port=%d) on the new binary", cfg.Mode, cfg.SoulDir, port)
	if err := ensureManagedServer(cfg.SoulDir, port); err != nil {
		warnf("could not (re)start the local server after a binary upgrade; run /soul-init to recover: %v", err)
	}
}

// --- seed (lib.sh seed_doc + session-start.sh seed_souldocs) -----------------

// seedSoulDocs copies the embedded index.md and project-template.md into soulDir
// when absent. Best-effort; never clobbers existing content (a user's customized
// doc survives across sessions).
func seedSoulDocs(soulDir string) {
	seedDoc("index.md", filepath.Join(soulDir, "index.md"))
	seedDoc("project-template.md", filepath.Join(soulDir, "project-template.md"))
}

// seedDoc copies the embedded seed seedName to target if target's directory is
// writable and target is absent. A no-op (best-effort) otherwise; logs only on
// an actual copy.
func seedDoc(seedName, target string) {
	dir := filepath.Dir(target)
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() || !dirWritable(dir) {
		return
	}
	if fileExists(target) {
		return
	}
	data, err := seedFS.ReadFile("seed/" + seedName)
	if err != nil {
		return
	}
	if err := os.WriteFile(target, data, 0o644); err != nil {
		warnf("could not seed %s (write failed); continuing", target)
		return
	}
	logf("seeded %s", target)
}

// --- exported API ------------------------------------------------------------

// runProvisionLocked runs fn and releases the provision lock dir afterward, even
// if fn panics. Split out of withProvisionLock's loop so the cleanup defer isn't
// registered per iteration (it fires once, on the acquiring iteration's return).
func runProvisionLocked(lockDir string, fn func() error) error {
	defer func() { _ = os.RemoveAll(lockDir) }()
	return fn()
}

// withProvisionLock serializes provisioning across processes (two session starts
// could otherwise interleave binary installs or both pass the port check before
// spawning). Atomic mkdir mutex with PID-stamped stale-lock recovery; bounded so
// a crashed holder can't wedge startup. Held across downloads, so a concurrent
// first-run waits for the in-flight install rather than racing it.
func withProvisionLock(fn func() error) error {
	lockDir, err := underHome(".provision.lock")
	if err != nil {
		return err
	}
	lockPid := filepath.Join(lockDir, "pid")
	if err := os.MkdirAll(filepath.Dir(lockDir), 0o755); err != nil {
		return err
	}
	for range 900 { // ~180s: enough for a slow first-run download
		if err := os.Mkdir(lockDir, 0o755); err == nil {
			// Record our PID before entering the critical section. If the write
			// fails, the lock dir has no owner stamp and stale-lock recovery
			// (which reads this pid to prove the holder is dead) can never reclaim
			// it — a poison lock that wedges every waiter. So release and fail.
			if werr := os.WriteFile(lockPid, []byte(strconv.Itoa(os.Getpid())), 0o644); werr != nil {
				_ = os.RemoveAll(lockDir)
				return fmt.Errorf("write provision lock pid: %w", werr)
			}
			return runProvisionLocked(lockDir, fn)
		} else if !os.IsExist(err) {
			return err
		}
		if b, e := os.ReadFile(lockPid); e == nil {
			if pid, e2 := strconv.Atoi(strings.TrimSpace(string(b))); e2 == nil && !pidAlive(pid) {
				// Stale-lock recovery, ownership-safe: atomically rename the lock
				// dir aside (only one racer's rename wins), then RE-READ the
				// moved-aside pid and confirm it's still dead before deleting. A
				// blind RemoveAll here could nuke a lock another process freshly
				// acquired between the read above and the delete; if the rename
				// turns out to have grabbed such a live lock, we put it back.
				aside := lockDir + ".stale." + strconv.Itoa(os.Getpid())
				if os.Rename(lockDir, aside) == nil {
					restore := false
					if b2, e3 := os.ReadFile(filepath.Join(aside, "pid")); e3 == nil {
						if p2, e4 := strconv.Atoi(strings.TrimSpace(string(b2))); e4 == nil && pidAlive(p2) {
							restore = true // moved a freshly-acquired live lock — undo
						}
					}
					if restore {
						_ = os.Rename(aside, lockDir)
					} else {
						_ = os.RemoveAll(aside)
					}
				}
				continue
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("could not acquire provision lock %s (another session provisioning?)", lockDir)
}

// Provision runs the per-session sequence: load-or-default config, ensure
// binaries, restart-on-upgrade, ensure/verify the server, seed soul docs. All
// progress goes to STDERR. (session-start.sh / pi provision.sh main.)
func Provision() error { return withProvisionLock(provisionLocked) }

func provisionLocked() error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	if cfg == nil {
		logf("no plugin config; running default setup")
		if err := initLocked("default", 0, ""); err != nil { // already holding the lock
			return err
		}
		cfg, err = loadConfig()
		if err != nil {
			return err
		}
		if cfg == nil {
			return fmt.Errorf("default setup completed but plugin-memory.conf is missing or invalid")
		}
	}

	replaced, err := ensureBinaries()
	if err != nil {
		return err
	}
	restartLocalServerOnUpgrade(replaced)

	port, perr := strconv.Atoi(strings.TrimSpace(cfg.Port))
	switch cfg.Mode {
	case "default", "isolated":
		if perr != nil {
			return fmt.Errorf("malformed PORT %q in plugin-memory.conf", cfg.Port)
		}
		if err := ensureManagedServer(cfg.SoulDir, port); err != nil {
			return err
		}
	case "reuse":
		if pidOfServerAtRoot(cfg.SoulDir) == 0 {
			warnf("configured to reuse server at %s but none is running; run /soul-init to reconfigure", cfg.SoulDir)
		}
	default:
		return fmt.Errorf("unknown MODE in plugin-memory.conf: %s", cfg.Mode)
	}

	seedSoulDocs(cfg.SoulDir)
	logf("ready (mode=%s, soul=%s, port=%s)", cfg.Mode, cfg.SoulDir, cfg.Port)
	return nil
}

// Init runs setup for the given mode: "default" | "reuse" | "isolated".
// For reuse, port and root are required. For default/isolated they are ignored.
// (setup.sh main → do_default/do_reuse/do_isolated.)
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
	if _, err := ensureBinaries(); err != nil {
		return err
	}
	if !portIsFree(defaultPort) {
		logf("port %d is in use; falling back to isolated mode", defaultPort)
		return doIsolated()
	}
	soul, err := sharedSoulDir()
	if err != nil {
		return err
	}
	if err := ensureTokenEntry(filepath.Join(soul, "tokens.toml")); err != nil {
		return err
	}
	if err := ensureManagedServer(soul, defaultPort); err != nil {
		return err
	}
	if err := saveConfig(soul, defaultPort, "default"); err != nil {
		return err
	}
	logf("default setup complete (soul=%s, port=%d)", soul, defaultPort)
	return nil
}

func doIsolated() error {
	if _, err := ensureBinaries(); err != nil {
		return err
	}
	port, err := findFreePortFrom(isolatedPortStart)
	if err != nil {
		return err
	}
	soul, err := isolatedSoulDir()
	if err != nil {
		return err
	}
	if err := ensureTokenEntry(filepath.Join(soul, "tokens.toml")); err != nil {
		return err
	}
	if err := ensureManagedServer(soul, port); err != nil {
		return err
	}
	if err := saveConfig(soul, port, "isolated"); err != nil {
		return err
	}
	logf("isolated setup complete (soul=%s, port=%d)", soul, port)
	return nil
}

func doReuse(port int, root string) error {
	if port <= 0 || root == "" {
		return fmt.Errorf("reuse requires --port N --root PATH")
	}
	if _, err := ensureBinaries(); err != nil {
		return err
	}
	if err := ensureTokenEntry(filepath.Join(root, "tokens.toml")); err != nil {
		return err
	}

	// Ask the adopted server to reload tokens. It may not be ours, but SIGHUP is
	// the documented mechanism and has no effect on other signal handlers.
	targetPID := pidOfServerAtRoot(root)
	if targetPID > 0 {
		// Validate the adopted server is actually listening on --port. Every MCP
		// call dials mark://localhost:PORT from the saved config, so a mistyped or
		// stale --port would leave the plugin "configured" while every call hits
		// the wrong endpoint. Refuse a mismatch.
		actualPort := portOfRunningServer(targetPID)
		if actualPort != "" && actualPort != strconv.Itoa(port) {
			return fmt.Errorf("the demarkus-server at root %s (pid %d) is listening on port %s, not %d; re-run with --port %s", root, targetPID, actualPort, port, actualPort)
		}
		if err := signalPID(targetPID, syscall.SIGHUP); err == nil {
			logf("sent SIGHUP to adopted server (pid=%d) to reload tokens", targetPID)
		} else {
			warnf("SIGHUP to pid %d failed; tokens may not be active until the server restarts", targetPID)
		}
	} else {
		warnf("no running server found with -root %s; proceeding with config, but /soul-init may need a rerun once it's up", root)
	}

	if err := saveConfig(root, port, "reuse"); err != nil {
		return err
	}
	logf("reuse setup complete (soul=%s, port=%d)", root, port)
	return nil
}

// Status returns a human-readable one-line status of the configured server, or a
// note that no soul is configured.
func Status() (string, error) {
	cfg, err := loadConfig()
	if err != nil {
		return "", err
	}
	if cfg == nil {
		return "no demarkus-memory soul configured (run /soul-init)", nil
	}
	w, err := HealthWarning()
	if err != nil {
		return "", err
	}
	if w != "" {
		return fmt.Sprintf("mode=%s soul=%s port=%s: %s", cfg.Mode, cfg.SoulDir, cfg.Port, w), nil
	}
	return fmt.Sprintf("mode=%s soul=%s port=%s : healthy", cfg.Mode, cfg.SoulDir, cfg.Port), nil
}

// HealthWarning echoes a one-line human warning when the configured memory server
// does not appear to be up, empty when it looks healthy. Best-effort, probe-only
// (no QUIC round-trip): for managed modes it trusts the .pid liveness;
// for reuse it scans for the adopted process. (lib.sh server_health_warning.)
func HealthWarning() (string, error) {
	cfg, err := loadConfig()
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
		pid := readPID(filepath.Join(cfg.SoulDir, ".pid"))
		if pid <= 0 || !pidIsServerAtRoot(pid, cfg.SoulDir) {
			return fmt.Sprintf("the demarkus-memory server is not running (no live process for %s). Memory tools (mark_fetch/mark_publish/mark_lookup/...) will fail until it restarts; run /soul-init to restart, or /soul-status to diagnose.", cfg.SoulDir), nil
		}
	case "reuse":
		if pidOfServerAtRoot(cfg.SoulDir) == 0 {
			return fmt.Sprintf("demarkus-memory is configured to reuse a server rooted at %s, but none is running. Memory tools will fail until it is started; start that server or run /soul-init to reconfigure.", cfg.SoulDir), nil
		}
	}
	return "", nil
}

// DetectServers reports running demarkus-server processes, matching detect-soul.sh:
//
//	"NO_SERVER"                    — nothing detected
//	"SERVERS\n<PID PORT ROOT>..."  — one or more running servers
func DetectServers() (string, error) {
	results := findRunningDemarkus()
	if results == "" {
		return "NO_SERVER", nil
	}
	return "SERVERS\n" + results, nil
}

// --- small helpers -----------------------------------------------------------

func fileExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir()
}

func dirWritable(dir string) bool {
	// Probe with a temp file; the bash uses `-w`. A failed create means not writable.
	f, err := os.CreateTemp(dir, ".demarkus-wtest-")
	if err != nil {
		return false
	}
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)
	return true
}

// readPID reads and validates the bare positive integer in a .pid file, or 0.
func readPID(pidFile string) int {
	b, err := os.ReadFile(pidFile)
	if err != nil {
		return 0
	}
	s := strings.TrimSpace(string(b))
	if s == "" {
		return 0
	}
	pid, err := strconv.Atoi(s)
	if err != nil || pid <= 0 {
		return 0
	}
	return pid
}

// pidAlive reports whether pid is a live process (kill -0).
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = p.Signal(syscall.Signal(0))
	return err == nil || err == syscall.EPERM
}

func signalPID(pid int, sig syscall.Signal) error {
	if pid <= 0 {
		return fmt.Errorf("invalid pid %d", pid)
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Signal(sig)
}

// tailFile returns the last n lines of a file, or "" on error.
func tailFile(path string, n int) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// pgrepDemarkusServer returns the PIDs of processes whose command line contains
// "demarkus-server", sorted ascending (deterministic, matching pgrep order).
func pgrepDemarkusServer() []int {
	out, err := exec.Command("pgrep", "-f", "demarkus-server").Output()
	if err != nil {
		return nil
	}
	var pids []int
	self := os.Getpid()
	for ln := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" {
			continue
		}
		pid, err := strconv.Atoi(ln)
		if err != nil || pid == self {
			continue
		}
		pids = append(pids, pid)
	}
	sort.Ints(pids)
	return pids
}

// psArgs returns the full command line (args) of pid, or "". The leading/trailing
// space wrapping matches the bash " -root … " word-boundary matching done by
// argsRootMatches (ps -o args= gives no leading space, so we add one).
func psArgs(pid int) string {
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "args=").Output()
	if err != nil {
		return ""
	}
	args := strings.TrimSpace(string(out))
	if args == "" {
		return ""
	}
	return " " + args
}

// procEnv returns the value of env var name for pid, or "". Portable across
// Linux (/proc/<pid>/environ) and macOS (ps eww).
func procEnv(pid int, name string) string {
	if b, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid)); err == nil {
		for kv := range bytes.SplitSeq(b, []byte{0}) {
			s := string(kv)
			if v, ok := strings.CutPrefix(s, name+"="); ok {
				return v
			}
		}
		return ""
	}
	out, err := exec.Command("ps", "eww", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return ""
	}
	for tok := range strings.FieldsSeq(string(out)) {
		if v, ok := strings.CutPrefix(tok, name+"="); ok {
			return v
		}
	}
	return ""
}
