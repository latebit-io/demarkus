// Package joinurl builds and parses demarkus join URLs
// (mark://host[:port]#token=<raw>). The credential rides in the URL
// fragment, which never appears in any protocol request.
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
	if j.Host == "" || strings.HasPrefix(j.Host, ":") {
		return "", fmt.Errorf("join URL requires a hostname")
	}
	if strings.ContainsAny(j.Host, "/#?@[") {
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
	if u.Host == "" || u.Hostname() == "" {
		return Join{}, fmt.Errorf("join URL has no host")
	}
	// Reject URL state Join cannot represent, so nothing is silently dropped.
	if u.User != nil {
		return Join{}, fmt.Errorf("join URL must not carry userinfo")
	}
	if u.RawQuery != "" || u.ForceQuery {
		return Join{}, fmt.Errorf("join URL must not carry a query (credentials go in the #fragment)")
	}
	if u.Path != "" && u.Path != "/" {
		return Join{}, fmt.Errorf("join URL must not carry a path (got %q)", u.Path)
	}
	// Bracketed IPv6 would be misparsed by downstream host handling (slug
	// derivation, default-port append); fail loudly until that is fixed.
	if strings.HasPrefix(u.Host, "[") {
		return Join{}, fmt.Errorf("IPv6 literal hosts are not supported in join URLs; use a DNS name")
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
	for k, v := range vals {
		if k != "token" {
			return Join{}, fmt.Errorf("join URL fragment has unknown key %q (expected token)", k)
		}
		if len(v) > 1 {
			return Join{}, fmt.Errorf("join URL fragment repeats %q", k)
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
