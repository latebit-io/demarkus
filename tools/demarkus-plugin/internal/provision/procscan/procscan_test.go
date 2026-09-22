package procscan

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"syscall"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/provision/provisiontest"
)

func TestPortIsFreeAndFindFreePort(t *testing.T) {
	// Bind a UDP port ourselves, then assert PortIsFree reports it taken and
	// find_free_port skips it.
	conn, err := net.ListenPacket("udp", ":0")
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	defer func() { _ = conn.Close() }()
	taken := conn.LocalAddr().(*net.UDPAddr).Port

	if PortIsFree(taken) {
		t.Errorf("port %d is bound but portIsFree returned true", taken)
	}

	// A high port we did not bind should read free (best effort; if the host has
	// it taken the test is still valid — just pick one likely free).
	free := taken + 1
	if free > 65000 {
		free = taken - 1
	}
	if !PortIsFree(free) {
		t.Skipf("probe port %d unexpectedly busy on this host; skipping free-path assertion", free)
	}

	got, err := FindFreePortFrom(free)
	if err != nil {
		t.Fatalf("findFreePortFrom: %v", err)
	}
	if got < free || got > free+199 {
		t.Errorf("findFreePortFrom(%d) = %d, out of range", free, got)
	}
}

func TestPidIsServerAtRootRejectsNonsense(t *testing.T) {
	// PID 0 and a clearly-dead high PID are never our server.
	if ProbeAtRoot(0, "/whatever") == Ours {
		t.Error("pid 0 should never match")
	}
	if ProbeAtRoot(-5, "/whatever") == Ours {
		t.Error("negative pid should never match")
	}
	// Our own test process is alive but is not demarkus-server.
	if ProbeAtRoot(os.Getpid(), "/whatever") == Ours {
		t.Error("the test process is not a demarkus-server at any root")
	}
}

func TestArgsRootMatches(t *testing.T) {
	cases := []struct {
		args, target string
		want         bool
	}{
		{" demarkus-server -root /home/x/memory -port 6310", "/home/x/memory", true},
		{" demarkus-server -root=/home/x/memory -port 6310", "/home/x/memory", true},
		{" demarkus-server -port 6310 -root /home/x/memory", "/home/x/memory", true},   // at end
		{" demarkus-server -port 6310 -root=/home/x/memory", "/home/x/memory", true},   // at end, = form
		{" demarkus-server -root /home/x/memory2 -port 6310", "/home/x/memory", false}, // prefix only
		{" demarkus-server -root /other -port 6310", "/home/x/memory", false},
		{" demarkus-server --root /home/x/memory -port 6310", "/home/x/memory", true}, // long form
		{" demarkus-server --root=/home/x/memory", "/home/x/memory", true},            // long form, = at end
	}
	for _, c := range cases {
		if got := argsRootMatches(c.args, c.target); got != c.want {
			t.Errorf("argsRootMatches(%q, %q) = %v, want %v", c.args, c.target, got, c.want)
		}
	}
}

func TestFlagValue(t *testing.T) {
	cases := []struct {
		args string
		re   *regexp.Regexp
		want string
	}{
		{" demarkus-server -root /x -tokens /home/x/tokens.toml", tokensFlagRe, "/home/x/tokens.toml"},
		{" demarkus-server -tokens=/etc/demarkus/tokens.toml -root /x", tokensFlagRe, "/etc/demarkus/tokens.toml"},
		{" demarkus-server -root /x -port 6310", tokensFlagRe, ""},
		{" demarkus-server -root /x -port 6310", portFlagRe, "6310"},
		{" demarkus-server -root=/x -port 6310", rootFlagRe, "/x"},
	}
	for _, c := range cases {
		if got := flagValue(c.args, c.re); got != c.want {
			t.Errorf("flagValue(%q, %v) = %q, want %q", c.args, c.re, got, c.want)
		}
	}
}

func TestTokensFromArgv(t *testing.T) {
	cases := []struct {
		name string
		argv []string
		want string
	}{
		{"separate value", []string{"demarkus-server", "-tokens", "/a/tokens.toml"}, "/a/tokens.toml"},
		{"equals form", []string{"demarkus-server", "--tokens=/b/tokens.toml"}, "/b/tokens.toml"},
		{"whitespace preserved", []string{"demarkus-server", "-tokens", "/My Tokens/tokens.toml"}, "/My Tokens/tokens.toml"},
		{"absent", []string{"demarkus-server", "-root", "/x"}, ""},
		{"trailing flag without value", []string{"demarkus-server", "-tokens"}, ""},
	}
	for _, c := range cases {
		if got := tokensFromArgv(c.argv); got != c.want {
			t.Errorf("%s: tokensFromArgv(%v) = %q, want %q", c.name, c.argv, got, c.want)
		}
	}
}

// TestBinaryVersionAt checks the path-taking split reports a version, and stays
// empty for the inputs the drift probe must not read a verdict into.
func TestBinaryVersionAt(t *testing.T) {
	dir := t.TempDir()

	good := filepath.Join(dir, "answers")
	if err := os.WriteFile(good, []byte("#!/bin/sh\necho 0.35.0\n"), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	if got := BinaryVersionAt(good); got != "0.35.0" {
		t.Errorf("binaryVersionAt(executable) = %q, want %q", got, "0.35.0")
	}

	notExec := filepath.Join(dir, "not-exec")
	if err := os.WriteFile(notExec, []byte("#!/bin/sh\necho 0.35.0\n"), 0o644); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	if got := BinaryVersionAt(notExec); got != "" {
		t.Errorf("binaryVersionAt(non-executable) = %q, want empty", got)
	}
	if got := BinaryVersionAt(dir); got != "" {
		t.Errorf("binaryVersionAt(directory) = %q, want empty", got)
	}
	if got := BinaryVersionAt(filepath.Join(dir, "absent")); got != "" {
		t.Errorf("binaryVersionAt(missing) = %q, want empty", got)
	}
}

// TestProcessStart pins the contract the drift probe relies on: a real start
// time for a live process, and ok=false rather than a zero time for a dead one.
func TestProcessStart(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	t.Cleanup(func() { provisiontest.StopProcess(t, cmd) })

	// Wait before the first lookup: an implementation that stamps the time of
	// inspection rather than of launch would pass without this, and would then
	// miss a binary replaced during the gap.
	const settle = 2 * time.Second
	time.Sleep(settle)

	started, ok := processStart(cmd.Process.Pid)
	if !ok {
		t.Fatalf("processStart(live pid) ok = false, want true")
	}
	if d := time.Since(started); d < settle-time.Second {
		t.Errorf("processStart(live pid) = %v, only %v ago; want the launch time, not the lookup time", started, d)
	}
	if d := time.Since(started); d > time.Minute {
		t.Errorf("processStart(live pid) = %v, %v ago; want within a minute", started, d)
	}

	// Kill it, then ask about a pid that is certainly gone.
	pid := cmd.Process.Pid
	provisiontest.StopProcess(t, cmd)
	if _, ok := processStart(pid); ok {
		t.Errorf("processStart(dead pid) ok = true, want false")
	}
}

// TestServerExecutable checks the resolver returns a runnable absolute path for
// a live process on whichever mechanism this platform provides.
func TestServerExecutable(t *testing.T) {
	// Launch by absolute path: ps comm= echoes how the process was invoked, so a
	// PATH-resolved name would test nothing the real adopted server does.
	sleepPath, err := exec.LookPath("sleep")
	if err != nil {
		t.Skipf("no sleep on PATH: %v", err)
	}
	cmd := exec.Command(sleepPath, "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	t.Cleanup(func() { provisiontest.StopProcess(t, cmd) })

	exe := Executable(cmd.Process.Pid)
	if exe == "" {
		t.Skip("no executable-path mechanism on this platform")
	}
	if !filepath.IsAbs(exe) {
		t.Errorf("serverExecutable = %q, want an absolute path", exe)
	}
	if base := filepath.Base(exe); base != "sleep" {
		t.Errorf("serverExecutable basename = %q, want %q", base, "sleep")
	}
}

// TestBinaryVersionAtTimeout checks a binary that never answers is bounded
// rather than left to stall session start.
func TestBinaryVersionAtTimeout(t *testing.T) {
	hang := filepath.Join(t.TempDir(), "hangs")
	if err := os.WriteFile(hang, []byte("#!/bin/sh\nsleep 60\n"), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	start := time.Now()
	if got := BinaryVersionAt(hang); got != "" {
		t.Errorf("binaryVersionAt(hanging) = %q, want empty", got)
	}
	if d := time.Since(start); d > versionProbeTimeout+time.Second {
		t.Errorf("binaryVersionAt(hanging) took %v, want within the %v budget", d, versionProbeTimeout)
	}
}

// TestBinaryVersionAtKillsForkedChild guards the probe against a --version that
// forks: cancelling has to take the whole process group, since a kill aimed at
// the direct child leaves the grandchild running.
func TestBinaryVersionAtKillsForkedChild(t *testing.T) {
	dir := t.TempDir()
	beat := filepath.Join(dir, "beat")
	forker := filepath.Join(dir, "forks")
	script := "#!/bin/sh\n" +
		"( while :; do printf x >> \"" + beat + "\"; sleep 1; done ) &\n" +
		"sleep 60\n"
	if err := os.WriteFile(forker, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	// Guarded: an ungrouped kill does not just leak the child, it never returns,
	// and a plain call would hang until the package timeout instead of failing.
	done := make(chan string, 1)
	go func() { done <- BinaryVersionAt(forker) }()
	select {
	case got := <-done:
		if got != "" {
			t.Errorf("binaryVersionAt(forking) = %q, want empty", got)
		}
	case <-time.After(versionProbeTimeout + 2*time.Second):
		t.Fatal("binaryVersionAt never returned; the forked grandchild still holds the probe's stdout pipe")
	}
	beatSize := func() int64 {
		info, err := os.Stat(beat)
		if err != nil {
			return -1
		}
		return info.Size()
	}
	before := beatSize()
	if before < 0 {
		t.Fatal("the forked child never wrote its heartbeat; descendant cleanup went unverified")
	}
	time.Sleep(2 * time.Second)
	if after := beatSize(); after != before {
		t.Errorf("forked child outlived the cancelled probe (beat grew %d -> %d)", before, after)
	}
}

// TestExecRefusal pins the gate that stops the probe running a file another user
// could have rewritten between the adoption and the probe.
func TestExecRefusal(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, mode os.FileMode) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("#!/bin/sh\ntrue\n"), mode); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		if err := os.Chmod(p, mode); err != nil { // WriteFile respects umask
			t.Fatalf("chmod %s: %v", name, err)
		}
		return p
	}

	tests := []struct {
		name string
		path string
		want string
	}{
		{name: "owned and not writable by others", path: write("ok", 0o755)},
		{name: "world writable", path: write("world-writable", 0o777), want: "is writable by other users"},
		{name: "group writable", path: write("group-writable", 0o775), want: "is writable by other users"},
		{name: "not executable", path: write("plain", 0o644), want: "is not a regular executable"},
		{name: "directory", path: dir, want: "is not a regular executable"},
		{name: "missing", path: filepath.Join(dir, "absent"), want: "cannot be read"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ExecRefusal(tt.path); got != tt.want {
				t.Errorf("execRefusal(%s) = %q, want %q", tt.name, got, tt.want)
			}
		})
	}
}

// TestProbeBounded separates the two failures a discovery command can have: a
// clean non-zero exit is an answer, an unrunnable command is not.
func TestProbeBounded(t *testing.T) {
	if out, ran := probeBounded(nil, "sh", "-c", "echo hello"); out != "hello" || !ran {
		t.Errorf("probeBounded(echo) = %q, %v; want %q, true", out, ran, "hello")
	}
	// pgrep reports "no match" as exit 1, which must not read as a failed probe.
	if out, ran := probeBounded(nil, "sh", "-c", "exit 1"); out != "" || !ran {
		t.Errorf("probeBounded(exit 1) = %q, %v; want empty, true", out, ran)
	}
	if _, ran := probeBounded(nil, "definitely-not-a-real-command-9f3c"); ran {
		t.Error("probeBounded(missing command) reported the probe as having run")
	}
	if _, ran := probeBounded(nil, "sh", "-c", "sleep 60"); ran {
		t.Error("probeBounded(hanging command) reported the probe as having run")
	}
}

// TestProcIsOurs pins the explicit owner check that stops a privileged plugin
// resolving a foreign process's executable through /proc/<pid>/exe.
func TestProcIsOurs(t *testing.T) {
	if _, err := os.Stat("/proc/self"); err != nil {
		// No procfs: Executable takes the ps path, which carries its own
		// owner check, and procIsOurs is expected to refuse everything.
		if procIsOurs(os.Getpid()) {
			t.Error("procIsOurs reported true without procfs; the ps path must own that decision")
		}
		return
	}
	if !procIsOurs(os.Getpid()) {
		t.Error("procIsOurs(self) = false, want true")
	}
	// Not "pid 1 is root's": under a rootless container it can be ours, and
	// procIsOurs is then right to say true.
	var st syscall.Stat_t
	if err := syscall.Stat("/proc/1", &st); err == nil && int(st.Uid) != os.Getuid() && procIsOurs(1) {
		t.Error("procIsOurs(pid 1) = true for a process owned by another user, want false")
	}
}

// TestPsArgsReportsProbeRan checks the discovery probes distinguish a process
// that is gone from an inspection that never happened.
func TestPsArgsReportsProbeRan(t *testing.T) {
	args, ran := Args(os.Getpid())
	if !ran || args == "" {
		t.Errorf("psArgs(self) = %q, %v; want args and true", args, ran)
	}
	// A pid that cannot exist: ps exits non-zero, which is still an answer.
	if _, ran := Args(-1); !ran {
		t.Error("psArgs(bad pid) reported the probe as not having run")
	}
}

// With ps unavailable the probe cannot answer; that must read as unknown, never
// as "not ours", or a live managed server loses its pid file and gets a twin.
func TestProbeServerAtRootUnknownWhenProbeCannotRun(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if got := ProbeAtRoot(os.Getpid(), "/some/root"); got != Unknown {
		t.Fatalf("probeServerAtRoot = %v, want ownershipUnknown", got)
	}
}
