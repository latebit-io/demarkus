package update

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const testPlugin = "demarkus-test-memory"

// serve returns an input pointed at a manifest server, plus its stamp path,
// under a temp HOME.
func serve(t *testing.T, manifest string, status int) (in Input, stamp string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("DEMARKUS_UPDATE_CHECK", "")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		if _, err := io.WriteString(w, manifest); err != nil {
			t.Errorf("write manifest: %v", err)
		}
	}))
	t.Cleanup(srv.Close)

	in = Input{
		Plugin:        testPlugin,
		Installed:     "0.13.26",
		ManifestURL:   srv.URL,
		UpdateCommand: "pi update demarkus-test-memory",
	}
	return in, filepath.Join(home, ".demarkus", ".update-check-"+testPlugin)
}

func TestIsNewer(t *testing.T) {
	tests := []struct {
		latest, installed string
		want              bool
	}{
		{"0.14.0", "0.13.26", true},
		{"0.13.26", "0.13.26", false},
		{"0.13.26", "0.14.0", false},
		{"0.13.27", "0.13.26", true},
		{"0.14.0", "0.13.99", true},
		{"1.0.0", "0.99.99", true},
		{"garbage", "0.13.26", false},
		{"0.13.26", "garbage", true},
	}
	for _, tt := range tests {
		t.Run(tt.latest+"_vs_"+tt.installed, func(t *testing.T) {
			if got := isNewer(tt.latest, tt.installed); got != tt.want {
				t.Errorf("isNewer(%q, %q) = %v, want %v", tt.latest, tt.installed, got, tt.want)
			}
		})
	}
}

func TestEvaluateMessageCarriesIdentity(t *testing.T) {
	in, _ := serve(t, `{"version":"0.14.0"}`, http.StatusOK)
	out, err := Evaluate(in)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	for _, want := range []string{in.Plugin, "0.14.0", in.Installed, in.UpdateCommand} {
		if !strings.Contains(out.Message, want) {
			t.Errorf("message %q missing %q", out.Message, want)
		}
	}
}

func TestEvaluateSilentWhenCurrent(t *testing.T) {
	in, _ := serve(t, `{"version":"0.13.26"}`, http.StatusOK)
	out, err := Evaluate(in)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if out.Message != "" {
		t.Errorf("expected no notice when current, got %q", out.Message)
	}
}

func TestThrottle(t *testing.T) {
	in, stamp := serve(t, `{"version":"0.14.0"}`, http.StatusOK)

	if _, err := Evaluate(in); err != nil {
		t.Fatalf("first Evaluate: %v", err)
	}
	if _, err := os.Stat(stamp); err != nil {
		t.Fatalf("stamp not written: %v", err)
	}

	out, err := Evaluate(in)
	if err != nil {
		t.Fatalf("second Evaluate: %v", err)
	}
	if out.Message != "" {
		t.Errorf("expected the throttle to suppress the second check, got %q", out.Message)
	}

	// A stamp older than the interval re-opens the window.
	old := strconv.FormatInt(time.Now().Add(-interval-time.Minute).Unix(), 10)
	if err := os.WriteFile(stamp, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err = Evaluate(in)
	if err != nil {
		t.Fatalf("third Evaluate: %v", err)
	}
	if out.Message == "" {
		t.Error("expected a notice once the throttle window elapsed")
	}
}

func TestMalformedStampIsDue(t *testing.T) {
	in, stamp := serve(t, `{"version":"0.14.0"}`, http.StatusOK)
	if err := os.MkdirAll(filepath.Dir(stamp), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stamp, []byte("not-a-number"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := Evaluate(in)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if out.Message == "" {
		t.Error("a corrupt stamp must not disable the check")
	}
}

func TestFutureStampIsDue(t *testing.T) {
	in, stamp := serve(t, `{"version":"0.14.0"}`, http.StatusOK)
	if err := os.MkdirAll(filepath.Dir(stamp), 0o755); err != nil {
		t.Fatal(err)
	}
	ahead := strconv.FormatInt(time.Now().Add(48*time.Hour).Unix(), 10)
	if err := os.WriteFile(stamp, []byte(ahead), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := Evaluate(in)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if out.Message == "" {
		t.Error("a stamp ahead of the clock must not throttle the check")
	}
}

func TestFetchFailureLeavesNoStamp(t *testing.T) {
	in, stamp := serve(t, `{"version":"0.14.0"}`, http.StatusInternalServerError)
	if _, err := Evaluate(in); err == nil {
		t.Fatal("expected an error on HTTP 500")
	}
	if _, err := os.Stat(stamp); !os.IsNotExist(err) {
		t.Errorf("stamp must not be written on a failed fetch (stat err: %v)", err)
	}
}

func TestManifestWithoutVersion(t *testing.T) {
	in, _ := serve(t, `{"name":"demarkus-test-memory"}`, http.StatusOK)
	if _, err := Evaluate(in); err == nil {
		t.Error("expected an error when the manifest carries no version")
	}
}

func TestTurnedOff(t *testing.T) {
	t.Run("env", func(t *testing.T) {
		in, stamp := serve(t, `{"version":"0.14.0"}`, http.StatusOK)
		t.Setenv("DEMARKUS_UPDATE_CHECK", "off")
		out, err := Evaluate(in)
		if err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		if out.Message != "" {
			t.Errorf("turned off, got %q", out.Message)
		}
		if _, err := os.Stat(stamp); !os.IsNotExist(err) {
			t.Error("turned off: no stamp should be written")
		}
	})

	t.Run("config file", func(t *testing.T) {
		in, stamp := serve(t, `{"version":"0.14.0"}`, http.StatusOK)
		dir := filepath.Dir(stamp)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "plugin.update-check"), []byte("0\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		out, err := Evaluate(in)
		if err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		if out.Message != "" {
			t.Errorf("turned off by config file, got %q", out.Message)
		}
	})
}

func TestInputValidation(t *testing.T) {
	base, _ := serve(t, `{"version":"0.14.0"}`, http.StatusOK)
	for _, missing := range []string{"plugin", "installed", "manifest-url", "update-command"} {
		t.Run(missing, func(t *testing.T) {
			in := base
			switch missing {
			case "plugin":
				in.Plugin = ""
			case "installed":
				in.Installed = "  "
			case "manifest-url":
				in.ManifestURL = ""
			case "update-command":
				in.UpdateCommand = ""
			}
			if _, err := Evaluate(in); err == nil {
				t.Errorf("expected an error when %s is missing", missing)
			}
		})
	}
}
