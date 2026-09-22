// Package procscan discovers demarkus-server processes and probes ports and
// binaries: every ps, pgrep and --version call is bounded, and a probe that
// could not run is reported as unknown, never as an answer.
package procscan

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/lockdir"
)

// demarkusProtocolPort is demarkus-server's built-in default.
const demarkusProtocolPort = 6309

// versionProbeTimeout is the whole budget for one probe: the drift check runs it
// on an adopted binary this plugin did not install, and a hung one would stall
// session start. probeKillGrace is the slice of it reserved for pipe cleanup.
const (
	versionProbeTimeout = 2 * time.Second
	probeKillGrace      = 500 * time.Millisecond
)

// probeBounded runs a read-only discovery command under the probe budget, so a
// stalled ps or pgrep cannot block session start or /soul-status. Empty on any
// failure, matching its callers' best-effort contract; nil env inherits ours.
func probeBounded(env []string, name string, args ...string) (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), versionProbeTimeout-probeKillGrace)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	if env != nil {
		cmd.Env = append(os.Environ(), env...)
	}
	cmd.WaitDelay = probeKillGrace
	out, err := cmd.Output()
	if err != nil {
		// A clean non-zero exit is an answer (pgrep says "no match" that way);
		// only a failure to run at all leaves us without one.
		var exit *exec.ExitError
		return "", errors.As(err, &exit) && ctx.Err() == nil
	}
	return strings.TrimSpace(string(out)), true
}

// BinaryVersionAt runs --version on an executable path. Empty when the path is
// missing, not executable, or the binary is too old or too slow to answer.
func BinaryVersionAt(p string) string {
	info, err := os.Stat(p)
	if err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), versionProbeTimeout-probeKillGrace)
	defer cancel()
	cmd := exec.CommandContext(ctx, p, "--version")
	// Own group, killed whole: a --version that forks leaves the grandchild
	// holding the stdout pipe, and a kill aimed only at the direct child never
	// unblocks Output() (measured: the probe hangs indefinitely).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = probeKillGrace
	cmd.Stderr = io.Discard
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// PortIsFree reports whether nothing appears to own UDP port (demarkus is QUIC):
// a bind that fails with "address in use" means taken, any other failure reads
// as free, and the post-spawn probe catches a genuine conflict loudly.
func PortIsFree(port int) bool {
	conn, err := net.ListenPacket("udp", net.JoinHostPort("", strconv.Itoa(port)))
	if err != nil {
		if errors.Is(err, syscall.EADDRINUSE) {
			return false
		}
		return true // permissive on any other probe failure
	}
	_ = conn.Close() // probe connection; the dial result is the answer
	return true
}

// FindFreePortFrom returns the first apparently-free UDP port in [start, start+199].
func FindFreePortFrom(start int) (int, error) {
	end := start + 199
	for p := start; p <= end; p++ {
		if PortIsFree(p) {
			return p, nil
		}
	}
	return 0, fmt.Errorf("no free UDP port in %d..%d", start, end)
}

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

// TokensPathOf returns the tokens.toml the server actually reads (-tokens
// flag, else DEMARKUS_TOKENS env). ok is false when the env probe could not run,
// where "" means unknown, not none, and no conventional fallback is safe.
func TokensPathOf(args string, pid int) (string, bool) {
	if p := tokensFromArgv(procArgv(pid)); p != "" {
		return p, true
	}
	if p := flagValue(args, tokensFlagRe); p != "" {
		return p, true
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

// PIDAtRoot returns the PID of a demarkus-server whose -root flag (or
// DEMARKUS_ROOT env) equals root (literal match), or 0 when none match.
func PIDAtRoot(root string) int {
	// For callers that treat "could not look" as "none"; others use the probed form.
	pid, _ := PIDAtRootProbed(root)
	return pid
}

// PIDAtRootProbed adds whether process discovery ran at all, so callers
// that would otherwise announce "no server" can say they could not look.
func PIDAtRootProbed(root string) (int, bool) {
	pids, ran := pgrepDemarkusServer()
	if !ran {
		return 0, false
	}
	for _, pid := range pids {
		args, ran := Args(pid)
		if !ran {
			return 0, false // a skipped candidate could be the server we want
		}
		if argsRootMatches(args, root) {
			return pid, true
		}
		env, ran := procEnv(pid, "DEMARKUS_ROOT")
		if !ran {
			return 0, false
		}
		if env == root {
			return pid, true
		}
	}
	return 0, true
}

// Ownership is the answer to "is this pid our demarkus-server for root".
type Ownership int

// The three answers; only Ours allows a kill, only NotOurs allows a respawn.
const (
	NotOurs Ownership = iota // dead, reused by another process, or another root
	Ours                     // live demarkus-server whose root matches
	Unknown                  // the process probe did not run; decide nothing
)

// ProbeAtRoot is the ownership check that makes reusing or killing a
// recorded .pid safe. A probe that could not run is unknown, never "not ours".
func ProbeAtRoot(pid int, root string) Ownership {
	if pid <= 0 || !lockdir.PidAlive(pid) {
		return NotOurs
	}
	args, ran := Args(pid)
	if !ran {
		return Unknown
	}
	if !strings.Contains(args, "demarkus-server") {
		return NotOurs
	}
	if argsRootMatches(args, root) {
		return Ours
	}
	env, ran := procEnv(pid, "DEMARKUS_ROOT")
	if !ran {
		return Unknown
	}
	if env == root {
		return Ours
	}
	return NotOurs
}

// ErrOwnershipUnknown aborts a lifecycle step rather than guess about a live pid.
var ErrOwnershipUnknown = errors.New("cannot verify the recorded server pid: process probe did not complete; retry")

// argsRootMatches finds "-root TARGET" or "-root=TARGET" at a word boundary by
// literal compare, so paths with regex metacharacters or spaces are safe.
func argsRootMatches(args, target string) bool {
	for _, form := range []string{" -root " + target, " -root=" + target, " --root " + target, " --root=" + target} {
		if strings.Contains(args, form+" ") || strings.HasSuffix(args, form) {
			return true
		}
	}
	return false
}

// ListServers emits "PID PORT ROOT" per running demarkus-server, one per
// line (empty when none). Args take precedence over env; falls back to
// DEMARKUS_PORT/DEMARKUS_ROOT env, then the protocol default port / "(unknown)" root.
func ListServers() (string, bool) {
	pids, ran := pgrepDemarkusServer()
	if !ran {
		return "", false
	}
	var lines []string
	for _, pid := range pids {
		args, ran := Args(pid)
		if !ran {
			return "", false // an unlistable process is not an absent one
		}
		if args == "" {
			continue
		}
		port := PortOf(args, pid)
		root := flagValue(args, rootFlagRe)
		if root == "" {
			// An unreadable environment lists as "(unknown)" below.
			root, _ = procEnv(pid, "DEMARKUS_ROOT")
		}
		if root == "" {
			root = "(unknown)"
		}
		lines = append(lines, fmt.Sprintf("%d %s %s", pid, port, root))
	}
	return strings.Join(lines, "\n"), true
}

// PortOf returns the port the server listens on (-port flag in args,
// else DEMARKUS_PORT env, else the protocol default).
func PortOf(args string, pid int) string {
	if p := flagValue(args, portFlagRe); p != "" {
		return p
	}
	if p, _ := procEnv(pid, "DEMARKUS_PORT"); p != "" {
		return p
	}
	return strconv.Itoa(demarkusProtocolPort)
}

// Executable resolves pid's executable, refusing anything this plugin must
// not run: another user's process, or a file someone else can rewrite. Owner and
// path come from one observation, so a recycled PID cannot split the two.
func Executable(pid int) string {
	exe, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
	if err == nil {
		// Readlink fails on a foreign process only while unprivileged, so as
		// root it proves nothing: check the process owner explicitly.
		if !procIsOurs(pid) {
			return ""
		}
		exe = strings.TrimSuffix(exe, " (deleted)")
	} else {
		exe = psOwnedExecutable(pid)
	}
	if !filepath.IsAbs(exe) {
		return ""
	}
	return exe
}

// procIsOurs reports whether /proc/<pid> is owned by this user. False where
// procfs is absent, leaving the ps path to establish ownership instead.
func procIsOurs(pid int) bool {
	var st syscall.Stat_t
	if err := syscall.Stat(fmt.Sprintf("/proc/%d", pid), &st); err != nil {
		return false
	}
	return int(st.Uid) == os.Getuid()
}

// psOwnedExecutable reads pid's owner and executable in a single ps observation
// so the two cannot describe different processes. Empty unless the owner is us,
// or where ps reports a bare name (Linux comm) rather than a path.
func psOwnedExecutable(pid int) string {
	line, _ := probeBounded(nil, "ps", "-p", strconv.Itoa(pid), "-o", "uid=,comm=")
	uidField, exe, found := strings.Cut(strings.TrimSpace(line), " ")
	if !found {
		return ""
	}
	uid, err := strconv.Atoi(uidField)
	if err != nil || uid != os.Getuid() {
		return ""
	}
	return strings.TrimSpace(exe)
}

// ExecRefusal explains why the probe must not run p, empty when it is safe: a
// regular executable owned by us or root that other users cannot rewrite.
func ExecRefusal(p string) string {
	info, err := os.Stat(p)
	if err != nil {
		return "cannot be read"
	}
	if !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
		return "is not a regular executable"
	}
	if info.Mode().Perm()&0o022 != 0 {
		return "is writable by other users"
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "has unreadable ownership"
	}
	if int(st.Uid) != os.Getuid() && st.Uid != 0 {
		return "is owned by another user"
	}
	return ""
}

// binaryReplaceSkew covers the one-second truncation of ps lstart on the only
// platform still reduced to comparing mtimes: macOS, whose ps reads an exact
// start time that only the display rounds down. Linux ps is not this accurate.
const binaryReplaceSkew = time.Second

// lstartLayouts covers both C-locale ps orderings: BSD/macOS puts the day of the
// month before the month name, GNU after it.
var lstartLayouts = []string{"Mon _2 Jan 15:04:05 2006", "Mon Jan _2 15:04:05 2006"}

// processStart reports when pid started. ok is false whenever that cannot be
// established, which every caller treats as "stay silent".
func processStart(pid int) (time.Time, bool) {
	// /proc/<pid> ModTime is not a defined start time: it can reflect when the
	// procfs entry was instantiated, which hides a replacement. ps lstart is.
	out, _ := probeBounded([]string{"LC_ALL=C"}, "ps", "-p", strconv.Itoa(pid), "-o", "lstart=") // lstart is locale-formatted
	if out == "" {
		return time.Time{}, false
	}
	s := strings.Join(strings.Fields(out), " ")
	for _, layout := range lstartLayouts {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// exeReplaced compares pid's running image against the file at exe. Linux
// refuses to rewrite a running executable (ETXTBSY), so every real replacement
// swaps the inode and procfs still names the original: exact, with no window.
func exeReplaced(pid int, exe string) (replaced, ok bool) {
	if _, err := os.Stat("/proc/self"); err != nil {
		return false, false // no procfs: mtime is the only signal left
	}
	var running, onDisk syscall.Stat_t
	if err := syscall.Stat(fmt.Sprintf("/proc/%d/exe", pid), &running); err != nil {
		return false, true // procfs is here but will not answer; claim nothing
	}
	if err := syscall.Stat(exe, &onDisk); err != nil {
		return false, true
	}
	return running.Dev != onDisk.Dev || running.Ino != onDisk.Ino, true
}

// BinaryReplaced reports whether pid runs code the file at exe no longer holds.
// The mtime comparison is the fallback, not a supplement: Linux ps derives
// lstart from a whole-second boot time and can place a start after the build.
func BinaryReplaced(pid int, exe string) bool {
	if replaced, ok := exeReplaced(pid, exe); ok {
		return replaced
	}
	info, err := os.Stat(exe)
	if err != nil {
		return false
	}
	started, ok := processStart(pid)
	return ok && info.ModTime().After(started.Add(binaryReplaceSkew))
}

// ReadPID reads and validates the bare positive integer in a .pid file, or 0.
func ReadPID(pidFile string) int {
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

// AlreadyExited reports a signal failure that just means the process is gone,
// which is the outcome the stop sequence wants, not a warning.
func AlreadyExited(err error) bool {
	return errors.Is(err, os.ErrProcessDone) || errors.Is(err, syscall.ESRCH)
}

// Signal sends sig to pid, refusing the non-positive pids kill(2) broadcasts to.
func Signal(pid int, sig syscall.Signal) error {
	if pid <= 0 {
		return fmt.Errorf("invalid pid %d", pid)
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Signal(sig)
}

// pgrepDemarkusServer returns the PIDs of processes whose command line contains
// "demarkus-server", sorted ascending (deterministic, matching pgrep order).
func pgrepDemarkusServer() ([]int, bool) {
	out, ran := probeBounded(nil, "pgrep", "-f", "demarkus-server")
	if !ran {
		return nil, false
	}
	if out == "" {
		return nil, true
	}
	var pids []int
	self := os.Getpid()
	for ln := range strings.SplitSeq(out, "\n") {
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
	return pids, true
}

// Args returns the full command line of pid, or "". A leading space is added
// so argsRootMatches can match a flag at the first position as a word.
func Args(pid int) (string, bool) {
	args, ran := probeBounded(nil, "ps", "-p", strconv.Itoa(pid), "-o", "args=")
	if !ran {
		return "", false
	}
	if args == "" {
		return "", true
	}
	return " " + args, true
}

// procEnv returns the value of env var name for pid, or "". Portable across
// Linux (/proc/<pid>/environ) and macOS (ps eww).
func procEnv(pid int, name string) (string, bool) {
	if b, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid)); err == nil {
		for kv := range bytes.SplitSeq(b, []byte{0}) {
			s := string(kv)
			if v, ok := strings.CutPrefix(s, name+"="); ok {
				return v, true
			}
		}
		return "", true
	}
	env, ran := probeBounded(nil, "ps", "eww", "-p", strconv.Itoa(pid))
	if !ran {
		return "", false
	}
	for tok := range strings.FieldsSeq(env) {
		if v, ok := strings.CutPrefix(tok, name+"="); ok {
			return v, true
		}
	}
	return "", true
}
