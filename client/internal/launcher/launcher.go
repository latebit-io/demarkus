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
	"strings"
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

// schemeAllowed reports whether scheme is in allowed, comparing
// case-insensitively per RFC 3986 §3.1.
func schemeAllowed(scheme string, allowed []string) bool {
	return slices.ContainsFunc(allowed, func(a string) bool {
		return strings.EqualFold(a, scheme)
	})
}

// handlerFor returns the launcher binary and argv for the given platform.
// The URL is always the final argv arg — never embedded in a shell string.
func handlerFor(goos, rawURL string) (name string, args []string) {
	switch goos {
	case "darwin":
		return "open", []string{rawURL}
	case "linux", "freebsd", "openbsd", "netbsd":
		return "xdg-open", []string{rawURL}
	default:
		return "", nil
	}
}

// DefaultAllowlist is the set of URL schemes the TUI opens externally by default.
// file, javascript, and data are deliberately excluded — they have a long
// history of handler-level RCEs and filesystem smuggling.
var DefaultAllowlist = []string{"http", "https", "gemini", "mailto"}

// SchemeOf returns the scheme of raw in lowercase, or "" when it has none:
// ALPHA *( ALPHA / DIGIT / + / - / . ) before a colon (RFC 3986 §3.1). Both
// authority form (scheme://...) and opaque form (mailto:user@...) count.
func SchemeOf(raw string) string {
	for i, r := range raw {
		switch {
		case i == 0:
			if !isAlpha(r) {
				return ""
			}
		case r == ':':
			return strings.ToLower(raw[:i])
		case !isSchemeRune(r):
			return ""
		}
	}
	return ""
}

// ParseSchemes splits a comma-separated scheme list, trimming whitespace and
// dropping empty entries; "" is nil, which disables launching. An invalid
// entry fails the whole list: silently dropped, it would read as enabled.
func ParseSchemes(csv string) ([]string, error) {
	if csv == "" {
		return nil, nil
	}
	parts := strings.Split(csv, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		s := strings.TrimSpace(p)
		if s == "" {
			continue
		}
		if SchemeOf(s+":") != strings.ToLower(s) {
			return nil, fmt.Errorf("invalid URL scheme %q (schemes must match ALPHA *( ALPHA / DIGIT / \"+\" / \"-\" / \".\" ))", s)
		}
		out = append(out, s)
	}
	return out, nil
}

func isAlpha(r rune) bool { return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') }

func isSchemeRune(r rune) bool {
	return isAlpha(r) || (r >= '0' && r <= '9') || r == '+' || r == '-' || r == '.'
}
