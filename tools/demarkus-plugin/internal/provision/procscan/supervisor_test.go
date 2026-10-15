package procscan

import (
	"os"
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

// A launchd job carries its label in the environment; a terminal-started
// process carries "0". Both are observable on a stub this test starts.
func TestSupervisorReadsLaunchdLabel(t *testing.T) {
	exe := provisiontest.BuildStub(t)
	s, ran := Supervisor(provisiontest.StartStub(t, exe, "XPC_SERVICE_NAME=io.latebit.demarkus"))
	if !ran || s.Kind != "launchd" || s.Unit != "io.latebit.demarkus" || !s.User {
		t.Errorf("launchd job = %+v ran=%v", s, ran)
	}
	if s, ran := Supervisor(provisiontest.StartStub(t, exe, "XPC_SERVICE_NAME=0")); !ran || s.Kind != "" {
		t.Errorf("terminal process = %+v ran=%v, want a plain process", s, ran)
	}
}
