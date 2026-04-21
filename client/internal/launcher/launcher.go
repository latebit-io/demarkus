// Package launcher opens URLs in the user's default system handler.
//
// It is intentionally narrow: given a URL and a scheme allowlist, it dispatches
// to the platform's handler (open, xdg-open, rundll32) by passing the URL as a
// single argv argument — never through a shell, never via string concatenation.
// Unknown or excluded schemes are rejected before any process is spawned.
//
// This package exists so the TUI can follow http/https/gemini/mailto links in
// the user's default handler. The demarkus server only serves markdown, so
// this is strictly about non-mark:// links in rendered documents — not about
// fetching binary content over the protocol. It is not used by the CLI or MCP
// server, which are non-interactive and must not spawn GUI processes.
package launcher

import (
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"runtime"
	"slices"
)

// ErrDisallowedScheme is returned when the URL's scheme is not in the allowlist.
var ErrDisallowedScheme = errors.New("scheme not in allowlist")

// ErrNoHandler is returned when the platform's expected launcher binary is not
// on PATH (e.g. xdg-open missing on a minimal Linux install).
var ErrNoHandler = errors.New("no URL handler available on this system")

// runner executes a launcher binary with args. It is injectable for tests.
// The real implementation starts the process and releases it so the caller
// does not block on the handler's exit.
type runner func(name string, args ...string) error

func execRunner(name string, args ...string) error {
	path, err := exec.LookPath(name)
	if err != nil {
		return fmt.Errorf("%w: %s not found on PATH", ErrNoHandler, name)
	}
	cmd := exec.Command(path, args...)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", name, err)
	}
	// Detach: we don't care about the handler's exit code, and we must not
	// block the TUI on the browser process.
	return cmd.Process.Release()
}

// Open opens rawURL in the user's default handler for its scheme.
//
// Only schemes in allowed are launched; any other scheme returns
// ErrDisallowedScheme. The URL is passed as a single argv argument to the
// platform handler — no shell is invoked, so URL contents are never
// interpreted as shell syntax.
func Open(rawURL string, allowed []string) error {
	return openWith(rawURL, allowed, runtime.GOOS, execRunner)
}

func openWith(rawURL string, allowed []string, goos string, run runner) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("parse url: %w", err)
	}
	if u.Scheme == "" {
		return fmt.Errorf("%w: missing scheme", ErrDisallowedScheme)
	}
	if !schemeAllowed(u.Scheme, allowed) {
		return fmt.Errorf("%w: %q", ErrDisallowedScheme, u.Scheme)
	}

	name, args := handlerFor(goos, rawURL)
	if name == "" {
		return fmt.Errorf("%w: unsupported platform %q", ErrNoHandler, goos)
	}
	return run(name, args...)
}

func schemeAllowed(scheme string, allowed []string) bool {
	return slices.Contains(allowed, scheme)
}

// handlerFor returns the launcher binary and argv for the given platform.
// The URL is always the final argv arg — never embedded in a shell string.
func handlerFor(goos, rawURL string) (name string, args []string) {
	switch goos {
	case "darwin":
		return "open", []string{rawURL}
	case "linux", "freebsd", "openbsd", "netbsd":
		return "xdg-open", []string{rawURL}
	case "windows":
		// rundll32 invokes the URL handler directly. The URL is still an
		// argv arg — Windows performs its own quoting, but no shell is used.
		return "rundll32", []string{"url.dll,FileProtocolHandler", rawURL}
	default:
		return "", nil
	}
}

// DefaultAllowlist is the set of URL schemes the TUI opens externally by default.
// file, javascript, and data are deliberately excluded — they have a long
// history of handler-level RCEs and filesystem smuggling.
var DefaultAllowlist = []string{"http", "https", "gemini", "mailto"}
