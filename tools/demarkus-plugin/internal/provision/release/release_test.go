package release

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"

	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/provision/provisiontest"
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

func TestVersionDriftFormatting(t *testing.T) {
	old := PluginVersion
	t.Cleanup(func() { PluginVersion = old })

	// A dev build (no real ldflags version) falls back to fallbackToolsVersion.
	PluginVersion = "dev"
	want := "server=" + ServerVersion + ",client=" + ClientVersion + ",tools=" + fallbackToolsVersion
	if got := DesiredVersions(); got != want {
		t.Errorf("desiredVersions() [dev] = %q, want %q", got, want)
	}

	// A real release derives the tools pin from the binary's own version.
	PluginVersion = "9.9.9"
	want = "server=" + ServerVersion + ",client=" + ClientVersion + ",tools=9.9.9"
	if got := DesiredVersions(); got != want {
		t.Errorf("desiredVersions() [release] = %q, want %q", got, want)
	}

	// With no binaries installed, installedVersions reports all-empty fields,
	// which never equals desired → EnsureBinaries would re-download. (No plugin
	// field: provision doesn't manage demarkus-plugin — bootstrap.sh does.)
	t.Setenv("HOME", t.TempDir())
	got := installedVersions()
	if got != "server=,client=,tools=" {
		t.Errorf("installedVersions() with no bins = %q", got)
	}
	if got == DesiredVersions() {
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

// tarGz packs one executable script named name into an archive.
func tarGz(t *testing.T, name string, body []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// fakeRelease serves every pinned archive from a local server: each binary is
// a script answering --version with its pin, so a fresh install reads current.
// tamper rewrites the checksum of one component's archive.
func fakeRelease(t *testing.T, tamper string) (srv *httptest.Server, requests *int) {
	t.Helper()
	plat, err := detectPlatform()
	if err != nil {
		t.Fatal(err)
	}
	versions := map[string]string{"server": ServerVersion, "client": ClientVersion, "tools": ToolsRef()}
	assets := map[string][]byte{}
	sums := map[string]string{}
	for _, a := range artifacts(plat) {
		tgz := tarGz(t, a.binary, provisiontest.VersionScript(versions[a.component]))
		prefix := "/" + a.component + "/v" + versions[a.component] + "/"
		assets[prefix+a.archive] = tgz
		sum := sha256.Sum256(tgz)
		if tamper == a.component {
			sum[0] ^= 0xff
		}
		sums[prefix+a.checksums] += hex.EncodeToString(sum[:]) + "  " + a.archive + "\n"
	}
	count := 0
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count++
		if body, ok := assets[r.URL.Path]; ok {
			_, _ = w.Write(body)
			return
		}
		if body, ok := sums[r.URL.Path]; ok {
			_, _ = io.WriteString(w, body)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, &count
}

// TestEnsureBinaries covers the install loop the pins drive: a fresh home
// downloads and installs every pin, a current home downloads nothing, and a
// checksum mismatch installs nothing.
func TestEnsureBinaries(t *testing.T) {
	oldVersion, oldBase := PluginVersion, BaseURL
	t.Cleanup(func() { PluginVersion, BaseURL = oldVersion, oldBase })
	PluginVersion = "dev"

	tests := []struct {
		name         string
		tamper       string
		wantErr      string
		wantReplaced bool
	}{
		{name: "fresh install", wantReplaced: true},
		{name: "tampered server archive", tamper: "server", wantErr: "checksum mismatch"},
		{name: "tampered tools archive", tamper: "tools", wantErr: "checksum mismatch"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			srv, requests := fakeRelease(t, tt.tamper)
			BaseURL = srv.URL + "/"

			replaced, err := EnsureBinaries()
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("EnsureBinaries() = %v, %v; want error containing %q", replaced, err, tt.wantErr)
				}
				if anyBinaryPresent() {
					t.Error("a failed verification installed a binary")
				}
				return
			}
			if err != nil || replaced != tt.wantReplaced {
				t.Fatalf("EnsureBinaries() = %v, %v; want %v, nil", replaced, err, tt.wantReplaced)
			}
			if got, want := installedVersions(), DesiredVersions(); got != want {
				t.Fatalf("installed %q after install, want %q", got, want)
			}
			seen := *requests
			if replaced, err := EnsureBinaries(); err != nil || replaced {
				t.Fatalf("second EnsureBinaries() = %v, %v; want no swap", replaced, err)
			}
			if *requests != seen {
				t.Errorf("a current install downloaded %d more assets", *requests-seen)
			}
		})
	}
}
