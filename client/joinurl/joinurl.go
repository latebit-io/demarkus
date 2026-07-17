// Package joinurl builds and parses demarkus join URLs: a single string that
// carries everything needed to join a server, of the form
//
//	mark://host[:port]#token=<raw>
//
// The credential rides in the URL fragment, which never appears in any
// protocol request; the string exists only to be pasted into a join flow
// (/soul-join, demarkus join) or rendered as a QR code.
package joinurl

import (
	"fmt"
	"net/url"
	"strings"
)

// Join is the parsed content of a join URL.
type Join struct {
	Host  string // host[:port] as written; no scheme
	Token string // raw capability token, may be empty (read-only server)
}

// Build renders a join URL. Host is required; token is optional.
func Build(j Join) (string, error) {
	if j.Host == "" {
		return "", fmt.Errorf("join URL requires a host")
	}
	if strings.ContainsAny(j.Host, "/#?") {
		return "", fmt.Errorf("join URL host must be host[:port], got %q", j.Host)
	}
	s := "mark://" + j.Host
	if j.Token != "" {
		s += "#token=" + url.QueryEscape(j.Token)
	}
	return s, nil
}

// Parse decodes a join URL. Accepts a bare host, mark://host, or the full
// fragment form; unknown fragment keys are rejected so a typo (or a URL from
// a newer issuer with keys this build does not support) fails loudly instead
// of silently dropping a credential.
func Parse(raw string) (Join, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return Join{}, fmt.Errorf("empty join URL")
	}
	if !strings.Contains(s, "://") {
		s = "mark://" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return Join{}, fmt.Errorf("invalid join URL: %w", err)
	}
	if u.Scheme != "mark" {
		return Join{}, fmt.Errorf("join URL must use mark:// (got %s://)", u.Scheme)
	}
	if u.Host == "" {
		return Join{}, fmt.Errorf("join URL has no host")
	}
	if u.Path != "" && u.Path != "/" {
		return Join{}, fmt.Errorf("join URL must not carry a path (got %q)", u.Path)
	}
	j := Join{Host: u.Host}
	if u.Fragment == "" {
		return j, nil
	}
	// EscapedFragment keeps percent-escapes intact so ParseQuery decodes them
	// exactly once (u.Fragment is already decoded; parsing it would corrupt
	// tokens containing '+' or '%').
	vals, err := url.ParseQuery(u.EscapedFragment())
	if err != nil {
		return Join{}, fmt.Errorf("invalid join URL fragment: %w", err)
	}
	for k := range vals {
		if k != "token" {
			return Join{}, fmt.Errorf("join URL fragment has unknown key %q (expected token)", k)
		}
	}
	j.Token = vals.Get("token")
	return j, nil
}

// HasCredentials reports whether raw looks like a join URL fragment form
// (carries a token) rather than a bare host.
func HasCredentials(raw string) bool {
	j, err := Parse(raw)
	return err == nil && j.Token != ""
}
