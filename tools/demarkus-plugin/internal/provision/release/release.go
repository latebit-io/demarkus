// Package release pins the server, client and tools versions the plugin runs
// and installs them: download, sha256 verify, extract, atomic install.
package release

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/provision/procscan"

	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/config"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/provision/progress"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/semver"
)

// Pins for the server and client modules, which release on their own cadence;
// the bump workflow tracks their latest releases. fallbackToolsVersion serves
// dev builds only: a real release derives the tools pin from its own Version.
const (
	ServerVersion        = "0.44.0"
	ClientVersion        = "0.39.0"
	fallbackToolsVersion = "0.43.0"
)

// PluginVersion is the binary's own release, injected from main's ldflags. It
// ships in the tools release, so it names the release to fetch demarkus-token
// from; demarkus-plugin itself is bootstrap.sh's to install, never re-fetched here.
var PluginVersion = "dev"

// BaseURL is the release download root; tests point it at a local server.
var BaseURL = "https://github.com/latebit-io/demarkus/releases/download/"

// releaseURL is the download base of one component's tagged release.
func releaseURL(component, version string) string {
	return BaseURL + component + "%2Fv" + version
}

// ToolsRef is the tools release to pull demarkus-token from: the binary's own
// version when it's a real release, else the dev fallback.
func ToolsRef() string {
	if _, ok := semver.Parse(PluginVersion); ok {
		return PluginVersion
	}
	return fallbackToolsVersion
}

// detectPlatform maps GOOS/GOARCH onto the release artifact suffix, e.g.
// "darwin_arm64"; an unsupported OS or arch is an error.
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

// sha256Verify checks that archive's SHA-256 matches the entry for its basename
// in a GNU-coreutils-style checksums file ("<hex>  <name>").
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

// DesiredVersions is the pin string for the binaries provision manages: server,
// mcp, token. demarkus-plugin is bootstrap.sh's; re-fetching it here against a
// pin that cannot match its release tag was a self-downgrade cascade.
func DesiredVersions() string {
	return fmt.Sprintf("server=%s,client=%s,tools=%s",
		ServerVersion, ClientVersion, ToolsRef())
}

// installedVersions is DesiredVersions' shape read from the binaries on disk. A
// missing, unexecutable or too old binary reads as an empty field, which never
// equals a pin, so EnsureBinaries re-downloads.
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
	p, err := config.BinPath(name)
	if err != nil {
		return ""
	}
	return procscan.BinaryVersionAt(p)
}

// artifact is one pinned release archive: the binary it installs, the asset
// and checksums file names, and the component whose release serves them.
type artifact struct {
	binary, component, archive, checksums, base string
}

// artifacts lists the downloads for plat. demarkus-token ships in the same
// tools release as this binary, so toolsRef names a release with a matching one.
func artifacts(plat string) []artifact {
	tools := ToolsRef()
	return []artifact{
		{"demarkus-server", "server", fmt.Sprintf("demarkus-server_%s_%s.tar.gz", ServerVersion, plat), "demarkus-server_checksums.txt", releaseURL("server", ServerVersion)},
		{"demarkus-mcp", "client", fmt.Sprintf("demarkus-mcp_%s_%s.tar.gz", ClientVersion, plat), "demarkus-client_checksums.txt", releaseURL("client", ClientVersion)},
		{"demarkus-token", "tools", fmt.Sprintf("demarkus-token_%s_%s.tar.gz", tools, plat), "demarkus-tools_checksums.txt", releaseURL("tools", tools)},
	}
}

// fetch downloads the archive and its checksums into dir, verifies, and extracts.
func (a artifact) fetch(dir string) error {
	archive := filepath.Join(dir, a.archive)
	if err := download(a.base+"/"+a.archive, archive); err != nil {
		return fmt.Errorf("download %s: %w", a.archive, err)
	}
	sums := filepath.Join(dir, a.component+"_checksums.txt")
	if err := download(a.base+"/"+a.checksums, sums); err != nil {
		return fmt.Errorf("download %s checksums: %w", a.component, err)
	}
	if err := sha256Verify(sums, archive); err != nil {
		return err
	}
	return extractTarGz(archive, dir)
}

// EnsureBinaries downloads and installs the pinned binaries to ~/.demarkus/bin
// whenever the installed ones do not report exactly the pinned versions. It
// reports whether a binary was swapped, so the running server can be restarted.
func EnsureBinaries() (replaced bool, err error) {
	desired := DesiredVersions()
	installed := installedVersions()
	if installed == desired {
		return false, nil
	}

	binDir, err := config.StatePath("bin")
	if err != nil {
		return false, err
	}
	// Distinguish a stale install from a fresh one for the log line only.
	if anyBinaryPresent() {
		progress.Logf("binary version drift detected (installed=%s, desired=%s); re-downloading", installed, desired)
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

	progress.Logf("downloading demarkus binaries (server v%s, client v%s, tools v%s, %s)",
		ServerVersion, ClientVersion, ToolsRef(), plat)
	for _, a := range artifacts(plat) {
		if err := a.fetch(tmp); err != nil {
			return false, err
		}
	}
	for _, name := range []string{"demarkus-server", "demarkus-token", "demarkus-mcp"} {
		if err := InstallFile(filepath.Join(tmp, name), filepath.Join(binDir, name), 0o755); err != nil {
			return false, fmt.Errorf("install %s: %w", name, err)
		}
	}

	progress.Logf("binaries installed to %s", binDir)
	return true, nil
}

func anyBinaryPresent() bool {
	for _, name := range []string{"demarkus-server", "demarkus-mcp", "demarkus-token"} {
		if p, err := config.BinPath(name); err == nil {
			if info, e := os.Stat(p); e == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
				return true
			}
		}
	}
	return false
}

// download fetches url to dest under a 300s budget, following redirects (GitHub
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

// InstallFile copies src to dst with the given mode (atomic via temp + rename).
func InstallFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() {
		if err := in.Close(); err != nil {
			progress.Warnf("close %s after copy: %v", src, err)
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
