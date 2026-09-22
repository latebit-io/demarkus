// Package provisiontest holds the fixtures the lifecycle packages' tests
// share: a compiled demarkus-server stub, a fake demarkus-token, process control.
package provisiontest

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// StopProcess ends a helper process started by a test. Wait's error is the kill
// we just sent, so only a Kill failure is worth reporting.
func StopProcess(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		t.Errorf("kill test process %d: %v", cmd.Process.Pid, err)
	}
	if cmd.ProcessState != nil {
		return // already reaped
	}
	var exit *exec.ExitError
	if err := cmd.Wait(); err != nil && !errors.As(err, &exit) {
		t.Errorf("reap test process %d: %v", cmd.Process.Pid, err)
	}
}

// stub caches the compiled server stand-in for the test binary; Main removes it.
var stub struct {
	sync.Mutex
	dir, exe string
	err      error
}

// goCache is the build cache before any test points HOME at a temp dir, where
// a fresh cache would recompile the standard library per build.
var goCache = func() string {
	out, err := exec.Command("go", "env", "GOCACHE").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}()

// Main wraps a package's TestMain so the stub built by BuildStub is removed once.
func Main(m *testing.M) {
	code := m.Run()
	stub.Lock()
	if stub.dir != "" {
		if err := os.RemoveAll(stub.dir); err != nil {
			fmt.Fprintln(os.Stderr, "remove stub dir:", err)
		}
	}
	stub.Unlock()
	os.Exit(code)
}

// BuildStub returns the test's own copy of a demarkus-server stand-in
// (--version from STUB_VERSION, -port bound or exit 1, SIGHUP ignored), built
// once because ps reports a script's interpreter; a copy because tests chmod it.
func BuildStub(t *testing.T) string {
	t.Helper()
	stub.Lock()
	if stub.exe == "" && stub.err == nil {
		stub.dir, stub.exe, stub.err = buildStub()
	}
	exe, err := stub.exe, stub.err
	stub.Unlock()
	if err != nil {
		t.Skipf("cannot build version stub: %v", err)
	}
	body, err := os.ReadFile(exe)
	if err != nil {
		t.Fatalf("read stub: %v", err)
	}
	own := filepath.Join(t.TempDir(), "demarkus-server")
	if err := os.WriteFile(own, body, 0o700); err != nil { //nolint:gosec // executable stub
		t.Fatalf("copy stub: %v", err)
	}
	return own
}

func buildStub() (dir, exe string, err error) {
	dir, err = os.MkdirTemp("", "demarkus-stub-")
	if err != nil {
		return "", "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module versionstub\n\ngo 1.21\n"), 0o600); err != nil {
		return "", "", err
	}
	const src = `package main

import (
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "--version" {
		fmt.Println(os.Getenv("STUB_VERSION"))
		return
	}
	signal.Ignore(syscall.SIGHUP)
	for i, a := range os.Args {
		if a == "-port" && i+1 < len(os.Args) {
			conn, err := net.ListenPacket("udp", ":"+os.Args[i+1])
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			defer conn.Close()
		}
	}
	time.Sleep(5 * time.Minute)
}
`
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(src), 0o600); err != nil {
		return "", "", err
	}
	exe = filepath.Join(dir, "demarkus-server")
	build := exec.Command("go", "build", "-o", exe, ".") //nolint:gosec // test stub build
	build.Dir = dir
	if goCache != "" {
		build.Env = append(os.Environ(), "GOCACHE="+goCache)
	}
	if out, err := build.CombinedOutput(); err != nil {
		return "", "", fmt.Errorf("%w\n%s", err, out)
	}
	return dir, exe, nil
}

// ServerArgs are the flags a demarkus-server stub is started with.
type ServerArgs struct {
	Root   string
	Port   int
	Tokens string
}

// StartServer runs the stub as a server at args.Root, the way a user-managed
// server would run, and returns its pid.
func StartServer(t *testing.T, exe string, args ServerArgs) int {
	t.Helper()
	cmd := exec.Command(exe, "-root", args.Root, "-port", strconv.Itoa(args.Port), "-tokens", args.Tokens) //nolint:gosec // the stub under test
	if err := cmd.Start(); err != nil {
		t.Fatalf("start server stub: %v", err)
	}
	t.Cleanup(func() { StopProcess(t, cmd) })
	// The port is bound before the stub sleeps; a probe that raced it would read free.
	for range 50 {
		if conn, err := net.ListenPacket("udp", ":"+strconv.Itoa(args.Port)); err != nil {
			return cmd.Process.Pid
		} else if cerr := conn.Close(); cerr != nil {
			t.Fatalf("close probe: %v", cerr)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("server stub never bound port %d", args.Port)
	return 0
}

// FreePort returns a UDP port nothing owned a moment ago.
func FreePort(t *testing.T) int {
	t.Helper()
	conn, err := net.ListenPacket("udp", ":0")
	if err != nil {
		t.Fatalf("probe port: %v", err)
	}
	port := conn.LocalAddr().(*net.UDPAddr).Port
	if err := conn.Close(); err != nil {
		t.Fatalf("close probe: %v", err)
	}
	return port
}

// InstallBinary copies exe to $HOME/.demarkus/bin/<name> executable.
func InstallBinary(t *testing.T, home, name, exe string) string {
	t.Helper()
	body, err := os.ReadFile(exe)
	if err != nil {
		t.Fatalf("read %s: %v", exe, err)
	}
	return WriteBinary(t, home, name, body)
}

// WriteBinary installs body executable at $HOME/.demarkus/bin/<name> by temp
// file and rename, as the installer does: Linux refuses to rewrite a running
// executable in place (ETXTBSY), and a test may replace a live stub.
func WriteBinary(t *testing.T, home, name string, body []byte) string {
	t.Helper()
	binDir := filepath.Join(home, ".demarkus", "bin")
	if err := os.MkdirAll(binDir, 0o750); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(binDir, name)
	if err := os.WriteFile(p+".tmp", body, 0o700); err != nil { //nolint:gosec // executable stub
		t.Fatal(err)
	}
	if err := os.Rename(p+".tmp", p); err != nil {
		t.Fatal(err)
	}
	return p
}

// VersionScript is a binary that only answers --version with version.
func VersionScript(version string) []byte {
	return []byte("#!/bin/sh\necho " + version + "\n")
}

// ReplaceBinary swaps exe for a fresh copy the way an installer does, by rename.
// The running process keeps the original inode, which is the only replacement
// Linux allows at all: it refuses to rewrite a running executable.
func ReplaceBinary(t *testing.T, exe string) {
	t.Helper()
	staged := exe + ".next"
	body, err := os.ReadFile(exe)
	if err != nil {
		t.Fatalf("read %s: %v", exe, err)
	}
	if err := os.WriteFile(staged, body, 0o700); err != nil { //nolint:gosec // executable stub
		t.Fatalf("stage replacement: %v", err)
	}
	if err := os.Rename(staged, exe); err != nil {
		t.Fatalf("replace %s: %v", exe, err)
	}
}

// StartStub runs the stub so it stays alive and returns its pid.
func StartStub(t *testing.T, exe string) int {
	t.Helper()
	cmd := exec.Command(exe) //nolint:gosec // the stub under test
	if err := cmd.Start(); err != nil {
		t.Fatalf("start stub: %v", err)
	}
	t.Cleanup(func() { StopProcess(t, cmd) })
	return cmd.Process.Pid
}

// WriteFakeTokenBin installs a fake demarkus-token: revoke truncates
// (FAKE_REVOKE_FAIL=1 fails), generate appends a real sha256- entry,
// --version prints FAKE_TOKEN_VERSION; argsLog, if set, records each argv.
func WriteFakeTokenBin(t *testing.T, home, argsLog string) {
	t.Helper()
	logLine := ""
	if argsLog != "" {
		logLine = `echo "$@" >> "` + argsLog + `"`
	}
	fake := `#!/bin/bash
` + logLine + `
cmd="$1"; shift
if [[ "$cmd" == "--version" ]]; then echo "$FAKE_TOKEN_VERSION"; exit 0; fi
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
	WriteBinary(t, home, "demarkus-token", []byte(fake))
}
