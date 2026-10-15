package procscan

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/provision/provisiontest"
)

func TestSystemdUnit(t *testing.T) {
	tests := []struct {
		name, cgroup, unit string
		user               bool
	}{
		{"system unit", "0::/system.slice/demarkus.service\n", "demarkus.service", false},
		{"user unit", "0::/user.slice/user-501.slice/user@501.service/app.slice/demarkus.service\n", "demarkus.service", true},
		{"v1 hierarchy", "12:pids:/system.slice/demarkus.service\n1:name=systemd:/system.slice/demarkus.service\n", "demarkus.service", false},
		{"tmux is not demarkus", "0::/user.slice/user-501.slice/user@501.service/app.slice/tmux.service\n", "", false},
		{"session scope", "0::/user.slice/user-501.slice/session-3.scope\n", "", false},
		{"empty", "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			unit, user := systemdUnit(tt.cgroup)
			if unit != tt.unit || user != tt.user {
				t.Errorf("systemdUnit = %q, %v; want %q, %v", unit, user, tt.unit, tt.user)
			}
		})
	}
}

func TestLaunchdLabel(t *testing.T) {
	for v, want := range map[string]bool{"io.latebit.demarkus": true, "0": false, "": false, "application.com.apple.Terminal.1": false} {
		if launchdLabel(v) != want {
			t.Errorf("launchdLabel(%q) = %v, want %v", v, !want, want)
		}
	}
}

func TestManualRestart(t *testing.T) {
	uid := strconv.Itoa(os.Getuid())
	for _, c := range []struct {
		s    Supervision
		want string
	}{
		{Supervision{Kind: "launchd", Unit: "io.latebit.demarkus", User: true}, "launchctl kickstart -k gui/" + uid + "/io.latebit.demarkus"},
		{Supervision{Kind: "systemd", Unit: "demarkus.service", User: true}, "systemctl --user restart demarkus.service"},
		{Supervision{Kind: "systemd", Unit: "demarkus.service"}, "sudo systemctl restart demarkus.service"},
	} {
		if got := c.s.ManualRestart(); got != c.want {
			t.Errorf("%+v: ManualRestart = %q, want %q", c.s, got, c.want)
		}
	}
	if err := (Supervision{Kind: "systemd", Unit: "demarkus.service"}).Restart(); err == nil {
		t.Error("a system unit must not be restarted by the plugin")
	}
}

// fakeLaunchctlPrint puts a launchctl on PATH whose print reports jobPID.
func fakeLaunchctlPrint(t *testing.T, jobPID int) {
	t.Helper()
	bin := t.TempDir()
	script := fmt.Sprintf("#!/bin/sh\n[ \"$1\" = print ] || exit 1\nprintf 'state = running\\n\\tpid = %d\\n'\n", jobPID)
	if err := os.WriteFile(filepath.Join(bin, "launchctl"), []byte(script), 0o755); err != nil { //nolint:gosec // test fake
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// A launchd job carries its label in the environment, and so does every
// process it spawned; only the job's own pid is supervised. A
// terminal-started process carries "0".
func TestSupervisorReadsLaunchdLabel(t *testing.T) {
	exe := provisiontest.BuildStub(t)
	job := provisiontest.StartStub(t, exe, "XPC_SERVICE_NAME=io.latebit.demarkus")
	fakeLaunchctlPrint(t, job)
	if s, ran := Supervisor(job); !ran || s.Kind != "launchd" || s.Unit != "io.latebit.demarkus" || !s.User {
		t.Errorf("launchd job = %+v ran=%v", s, ran)
	}
	inherited := provisiontest.StartStub(t, exe, "XPC_SERVICE_NAME=io.latebit.demarkus")
	if s, ran := Supervisor(inherited); !ran || s.Kind != "" {
		t.Errorf("descendant of the job = %+v ran=%v, want a plain process", s, ran)
	}
	if s, ran := Supervisor(provisiontest.StartStub(t, exe, "XPC_SERVICE_NAME=0")); !ran || s.Kind != "" {
		t.Errorf("terminal process = %+v ran=%v, want a plain process", s, ran)
	}
}
