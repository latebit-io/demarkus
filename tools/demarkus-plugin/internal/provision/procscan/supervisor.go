package procscan

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Supervision names the service manager that runs a process. Restarting
// through it keeps the user's own configuration; the plugin never kills the
// process itself.
type Supervision struct {
	Kind string // "launchd" or "systemd"; "" for a plain process
	Unit string // launchd label or systemd unit
	User bool   // the user's own domain or instance; false needs root
}

// restartTimeout bounds one restart request to the supervisor.
const restartTimeout = 10 * time.Second

// Supervisor reports the service manager whose job is pid itself; a plain
// process, or a descendant of some job, has an empty Kind. ran is false when
// discovery could not run at all.
func Supervisor(pid int) (s Supervision, ran bool) {
	label, ran := procEnv(pid, "XPC_SERVICE_NAME")
	if !ran {
		return Supervision{}, false
	}
	if launchdLabel(label) {
		s = Supervision{Kind: "launchd", Unit: label, User: true}
	} else if cg, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pid)); err == nil {
		if unit, user := systemdUnit(string(cg)); unit != "" {
			s = Supervision{Kind: "systemd", Unit: unit, User: user}
		}
	}
	// A label or cgroup is inherited by every descendant of the job; a
	// restart through it must hit the job's own process.
	if s.Kind == "" || s.mainPID() != pid {
		return Supervision{}, true
	}
	return s, true
}

// launchdPIDLine is the "pid = N" line of launchctl print.
var launchdPIDLine = regexp.MustCompile(`(?m)^\s*pid = (\d+)\s*$`)

// mainPID is the process the supervisor runs for the unit, 0 when it cannot say.
func (s Supervision) mainPID() int {
	var out string
	var ran bool
	switch {
	case s.Kind == "launchd":
		out, ran = probeBounded(nil, "launchctl", "print", "gui/"+strconv.Itoa(os.Getuid())+"/"+s.Unit)
		m := launchdPIDLine.FindStringSubmatch(out)
		if m == nil {
			return 0
		}
		out = m[1]
	case s.User:
		out, ran = probeBounded(nil, "systemctl", "--user", "show", "-p", "MainPID", "--value", s.Unit)
	default:
		out, ran = probeBounded(nil, "systemctl", "show", "-p", "MainPID", "--value", s.Unit)
	}
	if !ran {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return 0
	}
	return pid
}

// launchdLabel reports a launchd job label. launchd also stamps "0" on
// processes started from a terminal and "application.*" on app bundles.
func launchdLabel(v string) bool {
	return v != "" && v != "0" && !strings.HasPrefix(v, "application.")
}

// systemdUnit finds the demarkus service unit in a /proc/<pid>/cgroup body
// and whether it sits under the user's own instance. Only a demarkus unit
// counts: a shell under tmux.service must never be restarted.
func systemdUnit(cgroup string) (unit string, user bool) {
	for line := range strings.SplitSeq(strings.TrimSpace(cgroup), "\n") {
		// v1 "N:ctl:/path" and v2 "0::/path" both end in the path.
		for seg := range strings.SplitSeq(line[strings.LastIndex(line, ":")+1:], "/") {
			switch {
			case strings.HasPrefix(seg, "user@") && strings.HasSuffix(seg, ".service"):
				user = true
			case strings.HasPrefix(seg, "demarkus") && strings.HasSuffix(seg, ".service"):
				unit = seg
			}
		}
		if unit != "" {
			return unit, user
		}
	}
	return "", false
}

// restartArgs is the restart request; a system unit needs root.
func (s Supervision) restartArgs() []string {
	switch {
	case s.Kind == "launchd":
		return []string{"launchctl", "kickstart", "-k", "gui/" + strconv.Itoa(os.Getuid()) + "/" + s.Unit}
	case s.User:
		return []string{"systemctl", "--user", "restart", s.Unit}
	}
	return []string{"sudo", "systemctl", "restart", s.Unit}
}

// ManualRestart is the command that restarts the unit.
func (s Supervision) ManualRestart() string { return strings.Join(s.restartArgs(), " ") }

// Restart asks the user's own supervisor to restart the unit and waits for
// the request, not for the new process; the caller watches for it.
func (s Supervision) Restart() error {
	if !s.User {
		return fmt.Errorf("%s %s needs privileges: run %s", s.Kind, s.Unit, s.ManualRestart())
	}
	ctx, cancel := context.WithTimeout(context.Background(), restartTimeout)
	defer cancel()
	argv := s.restartArgs()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // fixed supervisor binary; the unit came from the process itself
	cmd.WaitDelay = probeKillGrace
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s: %w: %s", strings.Join(argv, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}
