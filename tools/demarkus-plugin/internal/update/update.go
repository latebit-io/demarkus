// Package update implements the plugin update check shared by every harness
// adapter: compare the version a plugin was installed at against its published
// manifest, and return the notice to surface. The adapters own their release
// identity (manifest URL, update command); the throttle, fetch, comparison, and
// message shape live here.
//
// Notify-only by design: the plugin is already loaded when the check runs, so
// self-installing could not take effect before a restart.
package update

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/config"
)

// Input is the calling plugin's release identity plus the version it runs.
type Input struct {
	Plugin        string // package name, e.g. demarkus-opencode-memory
	Installed     string // version the adapter found locally
	ManifestURL   string // JSON manifest carrying the published {"version":...}
	UpdateCommand string // what the user runs to update this plugin
}

// Output carries the notice to surface; empty means nothing to say.
type Output struct {
	Message string `json:"message"`
}

// interval throttles the network call; httpTimeout stays under the adapters'
// 5s subprocess bound so a slow GitHub never looks like a dead binary.
const (
	interval    = 24 * time.Hour
	httpTimeout = 3 * time.Second
)

// Evaluate returns the update notice for the calling plugin, or an empty output
// when it is current, throttled, or turned off.
func Evaluate(in Input) (Output, error) {
	if err := in.validate(); err != nil {
		return Output{}, err
	}
	enabled, err := config.UpdateCheckEnabled()
	if err != nil {
		return Output{}, err
	}
	if !enabled {
		return Output{}, nil
	}

	// One stamp per plugin: the plugins share a version line but are installed
	// and updated independently.
	stamp, err := config.StatePath(".update-check-" + in.Plugin)
	if err != nil {
		return Output{}, err
	}
	if !due(stamp) {
		return Output{}, nil
	}

	latest, err := latestVersion(in.ManifestURL)
	if err != nil {
		// Offline or the host is down: no stamp is written, so the next session
		// retries instead of going quiet for a day.
		return Output{}, err
	}
	if err := writeStamp(stamp); err != nil {
		return Output{}, err
	}
	if !isNewer(latest, in.Installed) {
		return Output{}, nil
	}
	return Output{Message: fmt.Sprintf("%s %s available (installed %s). Update: %s",
		in.Plugin, latest, in.Installed, in.UpdateCommand)}, nil
}

func (in Input) validate() error {
	for _, f := range []struct{ name, value string }{
		{"plugin", in.Plugin},
		{"installed", in.Installed},
		{"manifest-url", in.ManifestURL},
		{"update-command", in.UpdateCommand},
	} {
		if strings.TrimSpace(f.value) == "" {
			return errors.New("missing " + f.name)
		}
	}
	return nil
}

// due reports whether the throttle window has elapsed. A missing stamp means
// never checked; an unreadable or malformed one counts as due, since a corrupt
// throttle must not disable the check forever.
func due(stamp string) bool {
	b, err := os.ReadFile(stamp)
	if err != nil {
		if !os.IsNotExist(err) {
			fmt.Fprintln(os.Stderr, "[demarkus-plugin] update-check: unreadable stamp, checking anyway: "+err.Error())
		}
		return true
	}
	secs, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return true
	}
	return time.Since(time.Unix(secs, 0)) >= interval
}

func writeStamp(stamp string) error {
	if err := os.MkdirAll(filepath.Dir(stamp), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(stamp), err)
	}
	if err := os.WriteFile(stamp, []byte(strconv.FormatInt(time.Now().Unix(), 10)), 0o600); err != nil {
		return fmt.Errorf("write stamp %s: %w", stamp, err)
	}
	return nil
}

func latestVersion(url string) (string, error) {
	client := &http.Client{Timeout: httpTimeout}
	resp, err := client.Get(url)
	if err != nil {
		return "", fmt.Errorf("fetch %s: %w", url, err)
	}
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil {
			fmt.Fprintln(os.Stderr, "[demarkus-plugin] update-check: close body: "+cerr.Error())
		}
	}()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetch %s: HTTP %d", url, resp.StatusCode)
	}
	var manifest struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&manifest); err != nil {
		return "", fmt.Errorf("decode %s: %w", url, err)
	}
	if manifest.Version == "" {
		return "", fmt.Errorf("manifest %s carried no version", url)
	}
	return manifest.Version, nil
}

// isNewer compares major.minor.patch numerically; the plugin line never ships
// prereleases, and an unparseable version compares as 0.0.0.
func isNewer(latest, installed string) bool {
	a, b := parts(latest), parts(installed)
	for i := range a {
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return false
}

func parts(v string) [3]int {
	var out [3]int
	if _, err := fmt.Sscanf(strings.TrimSpace(v), "%d.%d.%d", &out[0], &out[1], &out[2]); err != nil {
		return [3]int{}
	}
	return out
}
