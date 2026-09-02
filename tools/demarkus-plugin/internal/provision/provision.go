// Package provision ports the demarkus-memory server-lifecycle logic from the
// plugins' bash (lib.sh + setup.sh + session-start.sh + detect-memory.sh) into Go.
// It owns the per-session managed-server lifecycle: download/verify/install the
// pinned binaries, generate the plugin token, spawn (or adopt) a demarkus-server,
// and seed the memory docs. Every readiness/progress line goes to STDERR so a
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
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/token"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/config"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/lockdir"
)

// Version pins for the SEPARATE server/client modules (their own release
// cadence; the bump workflow tracks their latest existing releases).
const (
	serverVersion = "0.32.0"
	clientVersion = "0.31.4"
	// fallbackToolsVersion is used ONLY by dev builds (Version == "dev"), where
	// there's no real release to derive the tools version from. A real release
	// uses its own ldflags Version — see toolsRef.
	fallbackToolsVersion = "0.25.2"
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

// tokenPaths is the scope minted for the plugin token. legacyTokenPaths is the
// pre-fix scope: path.Match's "*" does not cross "/", so it blocked writes to
// nested docs; entries still carrying it are revoked and reminted.
const (
	tokenPaths       = "/**"
	legacyTokenPaths = "/*"
)

//go:embed seed/index.md
var seedFS embed.FS

// pristineTemplateHashes are the sha256 of every project-template.md the old
// filesystem seeding ever shipped. The layout now lives in the memory-memory
// skill; a flat copy matching one of these is an untouched seed, safe to delete.
var pristineTemplateHashes = map[string]bool{
	"2573bd505e8ed7f3573a2fd24e903b2dbf65df4dc03a933a8f5a10f63610c7a6": true,
	"e62bf567bf066d421ce7808cc7ee5077a2a6179b52b5b521fc25721b2960e109": true,
	"1e28177e16d4e580b34566a62d804809fe90c1ca188424ca51b20f4f9c351e93": true,
}

// log writes a progress line to stderr, matching the bash "[demarkus-memory] …".
func logf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "[demarkus-memory] "+format+"\n", a...)
}

func warnf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "[demarkus-memory] warning: "+format+"\n", a...)
}

// --- path helpers (lib.sh readonly PLUGIN_* paths) ---------------------------

func pluginBinDir() (string, error)    { return underHome("bin") }
func pluginConfig() (string, error)    { return underHome("plugin-memory.conf") }
func pluginToken() (string, error)     { return underHome("plugin-memory.token") }
func sharedMemoryDir() (string, error) { return underHome("soul") }
func isolatedMemoryDir() (string, error) {
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

// memoryID is a stable per-memory identifier: basename for readability plus a
// path hash for uniqueness across memories.
func memoryID(memoryDir string) string {
	sum := sha256.Sum256([]byte(memoryDir))
	return filepath.Base(memoryDir) + "-" + hex.EncodeToString(sum[:4])
}

// managedServerLogPath places the managed server's log under ~/.demarkus/logs,
// never inside memoryDir, which the server watches for tokens.toml changes (#289).
func managedServerLogPath(memoryDir string) (string, error) {
	return underHome(filepath.Join("logs", "server-"+memoryID(memoryDir)+".log"))
}

// managedTokensPath is the out-of-root tokens.toml for memoryDir's managed
// server: token data does not belong inside the served content root (#289
// follow-up). Managed modes only; reuse mode keeps <root>/tokens.toml.
func managedTokensPath(memoryDir string) (string, error) {
	return underHome(filepath.Join("tokens", memoryID(memoryDir), "tokens.toml"))
}

// tokensPathFor resolves the live tokens.toml for memoryDir: the out-of-root
// path once migrated, a legacy <memoryDir>/tokens.toml until then (the running
// server's -tokens flag still points there), the new path on fresh installs.
func tokensPathFor(memoryDir string) (string, error) {
	newPath, err := managedTokensPath(memoryDir)
	if err != nil {
		return "", err
	}
	if fileExists(newPath) {
		return newPath, nil
	}
	// TODO: drop the legacy branch once released installs have migrated.
	if legacy := filepath.Join(memoryDir, "tokens.toml"); fileExists(legacy) {
		return legacy, nil
	}
	return newPath, nil
}

// migrateManagedTokens moves a legacy <memoryDir>/tokens.toml to the out-of-root
// path and returns the final path. Only call with the managed server stopped:
// a live server reads the legacy path.
func migrateManagedTokens(memoryDir string) (string, error) {
	newPath, err := managedTokensPath(memoryDir)
	if err != nil {
		return "", err
	}
	legacy := filepath.Join(memoryDir, "tokens.toml")
	if fileExists(newPath) {
		// Retry a legacy cleanup that failed after a successful copy on a
		// prior run; a leftover copy keeps token material in the content root.
		if fileExists(legacy) {
			if err := os.Remove(legacy); err != nil {
				return "", fmt.Errorf("remove leftover legacy tokens.toml: %w", err)
			}
		}
		return newPath, nil
	}
	if !fileExists(legacy) {
		return newPath, nil // fresh install: ensureTokenEntry populates it
	}
	if err := os.MkdirAll(filepath.Dir(newPath), 0o700); err != nil {
		return "", err
	}
	if err := os.Rename(legacy, newPath); err != nil {
		// Cross-device fallback: project memories can sit on another volume.
		if err := installFile(legacy, newPath, 0o600); err != nil {
			return "", fmt.Errorf("migrate tokens.toml out of memory root: %w", err)
		}
		if err := os.Remove(legacy); err != nil {
			return "", fmt.Errorf("remove legacy tokens.toml after copy: %w", err)
		}
	}
	logf("moved tokens.toml out of the memory root (%s)", newPath)
	return newPath, nil
}

// ensureManagedTokenEntry mints the plugin token against the resolved managed
// tokens path for memory.
func ensureManagedTokenEntry(memory string) error {
	tokensTOML, err := tokensPathFor(memory)
	if err != nil {
		return err
	}
	return ensureTokenEntry(memory, tokensTOML)
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
func saveConfig(memory string, port int, mode, tokensTOML string) error {
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
		shellQuote(memory), port, shellQuote(mode),
	)
	// Persisted so a later run (or VerifyAuth) with the server down still
	// targets the registry resolved at setup, not a conventional guess.
	if tokensTOML != "" {
		content += fmt.Sprintf("TOKENS=%s\n", shellQuote(tokensTOML))
	}
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
		if err := installFile(filepath.Join(tmp, name), filepath.Join(binDir, name), 0o755); err != nil {
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

// installFile copies src to dst with the given mode (atomic via temp + rename).
func installFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() {
		if err := in.Close(); err != nil {
			warnf("close %s after copy: %v", src, err)
		}
	}()
	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
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
	if err := os.Chmod(tmp, mode); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

// --- token (lib.sh ensure_token_entry) ---------------------------------------

// ensureTokenEntry mints the plugin token (raw to plugin-memory.token, entry
// to tokensTOML, which may live outside root, the server root used to find a
// live server to reload). Idempotent when current; stale state is reminted.
func ensureTokenEntry(root, tokensTOML string) error {
	tokenFile, err := pluginToken()
	if err != nil {
		return err
	}
	tokenBin, err := binPath("demarkus-token")
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
		_ = os.Chmod(tokenFile, 0o600)
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

	// Reload BEFORE committing the raw token: a running server holds the
	// pre-remint table, and committing only after a successful reload keeps the
	// gate's invariant — a matching token file implies the server loaded it, so
	// any failure here self-heals via a remint on the next run.
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
	logf("generated plugin token (label=%s)", tokenLabel)
	return nil
}

// Test seams: reload-path coverage stubs these without a live server.
var (
	findServerAtRoot = pidOfServerAtRoot
	reloadServer     = signalReload
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

// signalReload SIGHUPs a server so it reloads tokens.toml. A vanished process
// is fine: the respawn path loads the fresh table.
func signalReload(pid int) error {
	err := signalPID(pid, syscall.SIGHUP)
	switch {
	case err == nil:
		logf("sent SIGHUP to server (pid=%d) to reload tokens", pid)
		return nil
	case alreadyExited(err):
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

// Anchored at an argument boundary so a path merely containing "-root=" or
// "-tokens=" is never parsed as a server flag.
var (
	rootFlagRe   = regexp.MustCompile(`(?:^|\s)--?root[= ]+(\S+)`)
	portFlagRe   = regexp.MustCompile(`(?:^|\s)--?port[= ]+(\d+)`)
	tokensFlagRe = regexp.MustCompile(`(?:^|\s)--?tokens[= ]+(\S+)`)
)

// flagValue extracts a flag's value from a ps args string, "" when absent.
func flagValue(args string, re *regexp.Regexp) string {
	if m := re.FindStringSubmatch(args); m != nil {
		return m[1]
	}
	return ""
}

// tokensPathOfServer returns the tokens.toml the server actually reads
// (-tokens flag, else DEMARKUS_TOKENS env), or "" when undiscoverable. Exact
// /proc argv wins over the flattened ps string, which truncates on whitespace.
func tokensPathOfServer(args string, pid int) string {
	if p := tokensFromArgv(procArgv(pid)); p != "" {
		return p
	}
	if p := flagValue(args, tokensFlagRe); p != "" {
		return p
	}
	return procEnv(pid, "DEMARKUS_TOKENS")
}

// procArgv returns the process's exact argument vector from /proc, nil where
// /proc is unavailable (darwin) or unreadable.
func procArgv(pid int) []string {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil || len(b) == 0 {
		return nil
	}
	return strings.Split(strings.TrimRight(string(b), "\x00"), "\x00")
}

func tokensFromArgv(argv []string) string {
	for i, a := range argv {
		if a == "-tokens" || a == "--tokens" {
			if i+1 < len(argv) {
				return argv[i+1]
			}
			return ""
		}
		for _, prefix := range []string{"-tokens=", "--tokens="} {
			if v, ok := strings.CutPrefix(a, prefix); ok {
				return v
			}
		}
	}
	return ""
}

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
	if !lockdir.PidAlive(pid) {
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
	for _, form := range []string{" -root " + target, " -root=" + target, " --root " + target, " --root=" + target} {
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
		port := portOfServer(args, pid)
		root := flagValue(args, rootFlagRe)
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

// portOfServer returns the port the server listens on (-port flag in args,
// else DEMARKUS_PORT env, else the protocol default).
func portOfServer(args string, pid int) string {
	if p := flagValue(args, portFlagRe); p != "" {
		return p
	}
	if p := procEnv(pid, "DEMARKUS_PORT"); p != "" {
		return p
	}
	return strconv.Itoa(demarkusProtocolPort)
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

// stopStaleManagedServer stops a live-but-stale managed server and confirms it
// exited: the caller's token migration moves the file the old server reads. A
// live PID that is not our server for memoryDir is left untouched (kill safety).
func stopStaleManagedServer(runningPID int, memoryDir string) error {
	if !pidIsServerAtRoot(runningPID, memoryDir) {
		if runningPID > 0 && lockdir.PidAlive(runningPID) {
			warnf("recorded pid %d is live but is not the demarkus-server for %s (stale .pid, reused PID); leaving it alone and clearing our bookkeeping", runningPID, memoryDir)
		}
		return nil
	}
	logf("restarting managed demarkus-server onto upgraded binary (target=%s)", serverVersion)
	if err := signalPID(runningPID, syscall.SIGTERM); err != nil && !alreadyExited(err) {
		warnf("SIGTERM to managed server pid %d: %v", runningPID, err)
	}
	// Bounded wait for exit (~3s), then escalate to SIGKILL so the respawn
	// can bind the freed UDP port.
	for waited := 0; waited < 30 && lockdir.PidAlive(runningPID); waited++ {
		time.Sleep(100 * time.Millisecond)
	}
	if lockdir.PidAlive(runningPID) {
		if err := signalPID(runningPID, syscall.SIGKILL); err != nil && !alreadyExited(err) {
			warnf("SIGKILL to managed server pid %d: %v", runningPID, err)
		}
		for waited := 0; waited < 10 && lockdir.PidAlive(runningPID); waited++ {
			time.Sleep(100 * time.Millisecond)
		}
	}
	if lockdir.PidAlive(runningPID) {
		return fmt.Errorf("prior managed server (pid %d) still alive after SIGKILL; aborting respawn", runningPID)
	}
	return nil
}

// ensureManagedServer spawns a demarkus-server for memoryDir unless ours is
// already running and current (default/isolated modes only). A recorded PID is
// killed only once confirmed to be our server for this root; a stale PID is left alone.
func ensureManagedServer(memoryDir string, port int) error {
	pidFile := filepath.Join(memoryDir, ".pid")
	versionFile := filepath.Join(memoryDir, ".server-version")

	if managedServerCurrent(pidFile, versionFile, memoryDir) {
		return nil
	}

	// Not reusable. Stop a live-but-stale managed server before respawning — but
	// only after confirming it is genuinely ours for this root.
	if err := stopStaleManagedServer(readPID(pidFile), memoryDir); err != nil {
		return err
	}
	_ = os.Remove(pidFile)
	_ = os.Remove(versionFile)
	// Pre-#289 the log sat at <memoryDir>/.log, inside the server's tokens-watch
	// directory, feeding the watcher its own output. Remove it on migration.
	legacyLog := filepath.Join(memoryDir, ".log")
	if err := os.Remove(legacyLog); err != nil && !os.IsNotExist(err) {
		warnf("remove legacy server log %s: %v", legacyLog, err)
	}

	if err := os.MkdirAll(memoryDir, 0o755); err != nil {
		return err
	}
	tokensPath, err := migrateManagedTokens(memoryDir)
	if err != nil {
		return err
	}

	serverBin, err := binPath("demarkus-server")
	if err != nil {
		return err
	}
	logFile, err := managedServerLogPath(memoryDir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(logFile), 0o755); err != nil {
		return err
	}
	lf, err := os.OpenFile(logFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer func() { _ = lf.Close() }()

	cmd := exec.Command(serverBin,
		"-root", memoryDir,
		"-port", strconv.Itoa(port),
		"-tokens", tokensPath,
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
		if !lockdir.PidAlive(pid) {
			_ = os.Remove(pidFile)
			_ = os.Remove(versionFile)
			tailInfo := ""
			if t := tailFile(logFile, 5); t != "" {
				tailInfo = "\nrecent log:\n" + t
			}
			return fmt.Errorf("demarkus-server failed to start (port %d may be in use; re-run /memory-init)%s", port, tailInfo)
		}
		if !portIsFree(port) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	logf("spawned demarkus-server (pid=%d, port=%d, root=%s)", pid, port, memoryDir)
	return nil
}

// restartLocalServerOnUpgrade moves the local server onto a freshly swapped
// binary. Ownership is the MODE, not a .pid: reuse-mode servers are the user's,
// so only a down one is started. Restart problems are warned, never propagated.
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
		warnf("could not parse recorded PORT %q for upgrade restart; run /memory-init to recover", cfg.Port)
		return
	}

	// Our own managed server — safe to restart onto the new binary. Gated on MODE
	// (not just .pid) so a stale .pid under a reuse config can't trigger this.
	if cfg.Mode != "reuse" && fileExists(filepath.Join(cfg.MemoryDir, ".pid")) {
		logf("binary upgraded; restarting managed server (mode=%s, memory=%s, port=%d) on the new binary", cfg.Mode, cfg.MemoryDir, port)
		if err := ensureManagedServer(cfg.MemoryDir, port); err != nil {
			warnf("could not (re)start the local server after a binary upgrade; run /memory-init to recover: %v", err)
		}
		return
	}

	// Reuse mode, or a managed server that's down: don't assume it's ours.
	if extPID := pidOfServerAtRoot(cfg.MemoryDir); extPID > 0 {
		warnf("binary upgraded, but the server at %s (pid=%d, mode=%s) is user-managed and still running the old binary; restart it yourself to pick up the new one", cfg.MemoryDir, extPID, cfg.Mode)
		return
	}

	logf("binary upgraded; starting down server (mode=%s, memory=%s, port=%d) on the new binary", cfg.Mode, cfg.MemoryDir, port)
	if err := ensureManagedServer(cfg.MemoryDir, port); err != nil {
		warnf("could not (re)start the local server after a binary upgrade; run /memory-init to recover: %v", err)
	}
}

// --- seed (lib.sh seed_doc + session-start.sh seed_memorydocs) -----------------

// seedClient is the protocol surface seeding needs; *fetch.Client satisfies it.
type seedClient interface {
	Fetch(host, path, token string) (fetch.Result, error)
	Publish(host, path, body, token string, expectedVersion int, meta map[string]string) (fetch.Result, error)
}

// readPluginToken loads and trims the plugin's raw token.
func readPluginToken() (string, error) {
	tokenFile, err := pluginToken()
	if err != nil {
		return "", err
	}
	raw, err := os.ReadFile(tokenFile)
	if err != nil {
		return "", fmt.Errorf("read plugin token: %w", err)
	}
	return strings.TrimSpace(string(raw)), nil
}

// localClient builds the client for localhost session-start calls. Tight
// timeouts: the client retries transient errors 5x, so a down server still
// costs ~5s of dial timeouts (vs ~50s on the defaults); a wedged one ~25s/op.
func localClient() *fetch.Client {
	return fetch.NewClient(fetch.Options{Insecure: true, DialTimeout: time.Second, RequestTimeout: 5 * time.Second})
}

// seedMemoryDocs seeds index.md via PUBLISH so it gets version history; a
// flat file on disk is not FETCHable (issue #288). Best-effort: failures
// warn and provision continues.
func seedMemoryDocs(port int) {
	tok, err := readPluginToken()
	if err != nil {
		warnf("could not read plugin token; skipping doc seeding: %v", err)
		return
	}
	cl := localClient()
	defer cl.Close()
	seedDoc(cl, "localhost:"+strconv.Itoa(port), tok, "index.md", map[string]string{
		"tags":       "index,hub,projects,navigation",
		"importance": "0.9",
		// no OKF type: hubs stay untyped
	})
}

// cleanupLegacyTemplate deletes the old seeding's flat project-template.md
// (never FETCHable, issue #288) when pristine; the layout now ships in the
// memory-memory skill. A customized copy stays until its owner republishes.
func cleanupLegacyTemplate(memoryDir string) {
	const name = "project-template.md"
	flatPath := filepath.Join(memoryDir, name)
	fi, err := os.Lstat(flatPath)
	if err != nil {
		if !os.IsNotExist(err) {
			warnf("could not stat %s: %v", flatPath, err)
		}
		return
	}
	if !fi.Mode().IsRegular() {
		return // a symlink is an already-versioned doc
	}
	flat, err := os.ReadFile(flatPath)
	if err != nil {
		warnf("could not read flat %s: %v", name, err)
		return
	}
	sum := sha256.Sum256(flat)
	if !pristineTemplateHashes[hex.EncodeToString(sum[:])] {
		return
	}
	if err := os.Remove(flatPath); err != nil {
		warnf("could not remove legacy flat %s: %v", name, err)
		return
	}
	logf("removed legacy flat %s (the layout now ships in the memory-memory skill)", name)
}

// seedDoc publishes the embedded seed for name at the memory root unless the
// server already serves it. Create-only: over a legacy flat file this relies
// on the store's flat-to-v1 migration, whose conflict preserves user content.
func seedDoc(cl seedClient, host, tok, name string, meta map[string]string) {
	docPath := "/" + name
	res, err := cl.Fetch(host, docPath, tok)
	if err != nil {
		warnf("could not check %s before seeding (server unreachable?): %v", docPath, err)
		return
	}
	switch res.Response.Status {
	case protocol.StatusNotFound:
		// not served; seed below
	case protocol.StatusOK, protocol.StatusArchived:
		return // already versioned, or deliberately archived
	default:
		warnf("unexpected status %q checking %s; not seeding", res.Response.Status, docPath)
		return
	}

	content, err := seedFS.ReadFile("seed/" + name)
	if err != nil {
		warnf("embedded seed %s unreadable: %v", name, err)
		return
	}
	// expected-version 0 = create-only: a concurrent writer winning the race is fine.
	pres, err := cl.Publish(host, docPath, string(content), tok, 0, meta)
	if err != nil {
		warnf("could not seed %s: %v", docPath, err)
		return
	}
	switch pres.Response.Status {
	case protocol.StatusCreated, protocol.StatusOK:
		logf("seeded %s", docPath)
	case protocol.StatusConflict:
		// a concurrent writer created it; nothing to do
	default:
		warnf("could not seed %s: server returned %q", docPath, pres.Response.Status)
	}
}

// --- exported API ------------------------------------------------------------

// withProvisionLock serializes provisioning across processes (concurrent
// session starts would otherwise interleave binary installs). Held across
// downloads; ~180s bound covers a slow first-run download.
func withProvisionLock(fn func() error) error {
	lockDir, err := underHome(".provision.lock")
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

// provisionLocked does the lock-held provisioning work and returns the port to
// seed against, or 0 when seeding should be skipped.
func provisionLocked() (int, error) {
	cfg, err := loadConfig()
	if err != nil {
		return 0, err
	}
	if cfg == nil {
		logf("no plugin config; running default setup")
		if err := initLocked("default", 0, ""); err != nil { // already holding the lock
			return 0, err
		}
		cfg, err = loadConfig()
		if err != nil {
			return 0, err
		}
		if cfg == nil {
			return 0, fmt.Errorf("default setup completed but plugin-memory.conf is missing or invalid")
		}
	}

	replaced, err := ensureBinaries()
	if err != nil {
		return 0, err
	}
	restartLocalServerOnUpgrade(replaced)

	port, perr := strconv.Atoi(strings.TrimSpace(cfg.Port))
	seed := true
	switch cfg.Mode {
	case "default", "isolated":
		if perr != nil {
			return 0, fmt.Errorf("malformed PORT %q in plugin-memory.conf", cfg.Port)
		}
		if err := ensureManagedServer(cfg.MemoryDir, port); err != nil {
			return 0, err
		}
	case "reuse":
		if perr != nil {
			warnf("malformed PORT %q in plugin-memory.conf; skipping doc seeding", cfg.Port)
			seed = false
		}
		if pidOfServerAtRoot(cfg.MemoryDir) == 0 {
			warnf("configured to reuse server at %s but none is running; run /memory-init to reconfigure", cfg.MemoryDir)
			seed = false // known down; don't pay the seeding dial retries
		}
	default:
		return 0, fmt.Errorf("unknown MODE in plugin-memory.conf: %s", cfg.Mode)
	}

	cleanupLegacyTemplate(cfg.MemoryDir) // local-only; runs even when seeding is skipped
	logf("ready (mode=%s, memory=%s, port=%s)", cfg.Mode, cfg.MemoryDir, cfg.Port)
	if !seed {
		port = 0
	}
	return port, nil
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
	memory, err := sharedMemoryDir()
	if err != nil {
		return err
	}
	if err := ensureManagedTokenEntry(memory); err != nil {
		return err
	}
	if err := ensureManagedServer(memory, defaultPort); err != nil {
		return err
	}
	// Same end-to-end auth guarantee as reuse mode: setup success must mean
	// writes work, not just that local files were written.
	if err := verifyPluginToken(defaultPort); err != nil {
		return fmt.Errorf("plugin token does not authenticate against the managed server: %w; re-run /memory-init", err)
	}
	if err := saveConfig(memory, defaultPort, "default", ""); err != nil {
		return err
	}
	logf("default setup complete (memory=%s, port=%d)", memory, defaultPort)
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
	memory, err := isolatedMemoryDir()
	if err != nil {
		return err
	}
	if err := ensureManagedTokenEntry(memory); err != nil {
		return err
	}
	if err := ensureManagedServer(memory, port); err != nil {
		return err
	}
	if err := verifyPluginToken(port); err != nil {
		return fmt.Errorf("plugin token does not authenticate against the managed server: %w; re-run /memory-init", err)
	}
	if err := saveConfig(memory, port, "isolated", ""); err != nil {
		return err
	}
	logf("isolated setup complete (memory=%s, port=%d)", memory, port)
	return nil
}

func doReuse(port int, root string) error {
	if port <= 0 || root == "" {
		return fmt.Errorf("reuse requires --port N --root PATH")
	}
	if _, err := ensureBinaries(); err != nil {
		return err
	}
	// Adopted external server: prefer the tokens.toml it actually reads over the
	// <root>/tokens.toml convention (divergence broke every write, 2026-08-27),
	// and validate everything BEFORE ensureTokenEntry mutates any registry.
	tokensTOML := filepath.Join(root, "tokens.toml")
	prior, err := loadConfig()
	if err != nil {
		return fmt.Errorf("load plugin config: %w", err)
	}
	if prior != nil && prior.Mode == "reuse" && prior.MemoryDir == root && prior.TokensTOML != "" {
		// A prior adoption resolved a custom registry; keep it when the server
		// is down rather than falling back to the convention.
		tokensTOML = prior.TokensTOML
	}
	targetPID := pidOfServerAtRoot(root)
	if targetPID > 0 {
		targetArgs := psArgs(targetPID)
		if targetArgs != "" {
			// Compare parsed values, not text: "06309" and "6309" are the same port.
			raw := portOfServer(targetArgs, targetPID)
			if actual, err := strconv.Atoi(raw); err != nil || actual != port {
				return fmt.Errorf("the demarkus-server at root %s (pid %d) is listening on port %s, not %d; re-run with --port %s", root, targetPID, raw, port, raw)
			}
		}
		if discovered := tokensPathOfServer(targetArgs, targetPID); discovered != "" && discovered != tokensTOML {
			// An explicit path that does not resolve is a hard stop: writing the
			// conventional file instead would mutate a registry the server never
			// reads (a flattened ps string can truncate paths with whitespace).
			if !fileExists(discovered) {
				return fmt.Errorf("the adopted server (pid %d) reads tokens from %s, but no such file exists; check its -tokens flag or DEMARKUS_TOKENS and re-run /memory-init", targetPID, discovered)
			}
			logf("adopted server (pid %d) reads tokens from %s, not the conventional %s; using it", targetPID, discovered, tokensTOML)
			tokensTOML = discovered
		}
	}
	if err := ensureTokenEntry(root, tokensTOML); err != nil {
		return err
	}

	if targetPID > 0 {
		// Ask the adopted server to reload tokens. It may not be ours, but SIGHUP
		// is the documented mechanism and has no effect on other signal handlers.
		if err := signalReload(targetPID); err != nil {
			warnf("%v; tokens may not be active until the server restarts", err)
		}
		// Prove the token authenticates end to end. A hash written to a file the
		// server never reads passes every local check and fails only at first
		// write; refuse to report success on that state.
		if err := verifyPluginToken(port); err != nil {
			return fmt.Errorf("plugin token does not authenticate against the adopted server (tokens file: %s): %w; check the server's -tokens flag or DEMARKUS_TOKENS and re-run /memory-init", tokensTOML, err)
		}
	} else {
		warnf("no running server found with -root %s; proceeding with config, but /memory-init may need a rerun once it's up", root)
	}

	if err := saveConfig(root, port, "reuse", tokensTOML); err != nil {
		return err
	}
	logf("reuse setup complete (memory=%s, port=%d)", root, port)
	return nil
}

// authProbeClient is the one-verb surface the auth probe needs;
// *fetch.Client satisfies it.
type authProbeClient interface {
	Append(host, path, body, token string, expectedVersion int, meta map[string]string) (fetch.Result, error)
}

// probeExpectedVersion can never match a real document, so the probe APPEND
// always ends in not-found or a version conflict, never a write.
const probeExpectedVersion = 1 << 30

// verifyPluginToken probes auth with an APPEND that cannot mutate anything
// (auth is checked before the store is touched). Unauthorized retries briefly:
// the server reloads tokens asynchronously after SIGHUP, so a remint can race.
func verifyPluginToken(port int) error {
	tok, err := readPluginToken()
	if err != nil {
		return err
	}
	cl := localClient()
	defer cl.Close()
	host := "localhost:" + strconv.Itoa(port)
	for attempt := 0; ; attempt++ {
		err = verifyTokenWith(cl, host, tok)
		if !errors.Is(err, errUnauthorized) || attempt >= 3 {
			return err
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// errUnauthorized distinguishes a definitive auth rejection from transport
// failures, which callers must not report as token drift.
var errUnauthorized = errors.New("auth probe returned unauthorized")

func verifyTokenWith(cl authProbeClient, host, tok string) error {
	res, err := cl.Append(host, "/.demarkus-plugin-auth-probe.md", "probe", tok, probeExpectedVersion, nil)
	if err != nil {
		return fmt.Errorf("auth probe: %w", err)
	}
	if res.Response.Status == protocol.StatusUnauthorized {
		return errUnauthorized
	}
	return nil
}

// VerifyAuth reports whether the plugin token authenticates against the
// configured local server, naming the token registry the server actually reads.
// Read-only apart from the non-mutating ARCHIVE probe. (memory-doctor drift check.)
func VerifyAuth() (string, error) {
	cfg, err := loadConfig()
	if err != nil {
		return "", err
	}
	if cfg == nil {
		return "no demarkus-memory memory configured (run /memory-init)", nil
	}
	port, err := strconv.Atoi(cfg.Port)
	if err != nil {
		return "", fmt.Errorf("invalid PORT %q in plugin-memory.conf", cfg.Port)
	}
	// Name the registry for the report: the running server's own -tokens or
	// DEMARKUS_TOKENS wins; otherwise the path provisioning would use.
	tokensTOML := ""
	pid := pidOfServerAtRoot(cfg.MemoryDir)
	serverPort := ""
	if pid > 0 {
		args := psArgs(pid)
		tokensTOML = tokensPathOfServer(args, pid)
		serverPort = portOfServer(args, pid)
	}
	if tokensTOML == "" {
		tokensTOML = cfg.TokensTOML // registry resolved at setup, if recorded
	}
	if tokensTOML == "" {
		if cfg.Mode == "reuse" {
			tokensTOML = filepath.Join(cfg.MemoryDir, "tokens.toml")
		} else {
			p, err := tokensPathFor(cfg.MemoryDir)
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
		return fmt.Sprintf("cannot verify: server pid %d listens on port %s but the config says %s; re-run /memory-init", pid, serverPort, cfg.Port), nil
	}
	if err := verifyPluginToken(port); err != nil {
		// Only a definitive unauthorized is drift; a transport failure means the
		// probe never reached a server and proves nothing about the token.
		if !errors.Is(err, errUnauthorized) {
			return fmt.Sprintf("cannot verify: %v (server pid %d, port %s)", err, pid, cfg.Port), nil
		}
		return fmt.Sprintf("token drift: plugin token does not authenticate (server pid %d, token registry %s); re-run /memory-init or update the registry hash", pid, tokensTOML), nil
	}
	return fmt.Sprintf("write auth healthy (server pid %d, token registry %s)", pid, tokensTOML), nil
}

// Status returns a human-readable one-line status of the configured server, or a
// note that no memory is configured.
func Status() (string, error) {
	cfg, err := loadConfig()
	if err != nil {
		return "", err
	}
	if cfg == nil {
		return "no demarkus-memory memory configured (run /memory-init)", nil
	}
	w, err := HealthWarning()
	if err != nil {
		return "", err
	}
	if w != "" {
		return fmt.Sprintf("mode=%s memory=%s port=%s: %s", cfg.Mode, cfg.MemoryDir, cfg.Port, w), nil
	}
	return fmt.Sprintf("mode=%s memory=%s port=%s : healthy", cfg.Mode, cfg.MemoryDir, cfg.Port), nil
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
		pid := readPID(filepath.Join(cfg.MemoryDir, ".pid"))
		if pid <= 0 || !pidIsServerAtRoot(pid, cfg.MemoryDir) {
			return fmt.Sprintf("the demarkus-memory server is not running (no live process for %s). Memory tools (mark_fetch/mark_publish/mark_lookup/...) will fail until it restarts; run /memory-init to restart, or /memory-status to diagnose.", cfg.MemoryDir), nil
		}
	case "reuse":
		if pidOfServerAtRoot(cfg.MemoryDir) == 0 {
			return fmt.Sprintf("demarkus-memory is configured to reuse a server rooted at %s, but none is running. Memory tools will fail until it is started; start that server or run /memory-init to reconfigure.", cfg.MemoryDir), nil
		}
	}
	return "", nil
}

// DetectServers reports running demarkus-server processes as "NO_SERVER" or
// "SERVERS\n<PID PORT ROOT>..." lines, matching the old detect script.
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

// alreadyExited reports a signal failure that just means the process is gone,
// which is the outcome the stop sequence wants, not a warning.
func alreadyExited(err error) bool {
	return errors.Is(err, os.ErrProcessDone) || errors.Is(err, syscall.ESRCH)
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
