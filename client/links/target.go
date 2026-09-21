package links

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"

	"github.com/latebit-io/demarkus/protocol"
)

// Target is a parsed mark:// URL. Dialing needs a port and identity omits the
// default one (ADR 0005), so both forms come from this one parse.
type Target struct {
	Path string // decoded request path, "/" when the URL has none

	hostname    string // as written, without IPv6 brackets
	port        int
	escapedPath string
}

// ParseMark parses a mark:// URL. Query, fragment and userinfo are dropped:
// none of them reaches a request or names a node. Errors say what is wrong
// and leave naming the failure ("invalid URL: ...") to the caller.
func ParseMark(raw string) (Target, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return Target{}, err
	}
	return targetOf(u)
}

// ParseServer parses a URL that names a server and nothing else: no document
// path, userinfo, query or fragment. Index rows, seeds and hubs are these.
func ParseServer(raw string) (Target, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return Target{}, err
	}
	if u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(raw, "#") {
		return Target{}, fmt.Errorf("invalid server URL %q: only mark://host[:port] is allowed", raw)
	}
	if u.Path != "" && u.Path != "/" {
		return Target{}, fmt.Errorf("invalid server URL %q: must not contain a document path", raw)
	}
	return targetOf(u)
}

func targetOf(u *url.URL) (Target, error) {
	var err error
	if u.Scheme != protocol.ALPN {
		return Target{}, fmt.Errorf("unsupported scheme: %s (expected mark://)", u.Scheme)
	}
	hostname := u.Hostname()
	if hostname == "" {
		return Target{}, errors.New("authority host is required")
	}
	port := protocol.DefaultPort
	if p := u.Port(); p != "" {
		port, err = strconv.Atoi(p)
		if err != nil || port < 1 || port > 65535 {
			return Target{}, fmt.Errorf("port %q must be between 1 and 65535", p)
		}
	}
	target := Target{Path: u.Path, hostname: hostname, port: port, escapedPath: u.EscapedPath()}
	if target.Path == "" {
		target.Path, target.escapedPath = "/", "/"
	}
	return target, nil
}

// DialHost is the lowercase host:port to open a connection to.
func (t Target) DialHost() string {
	return net.JoinHostPort(strings.ToLower(t.hostname), strconv.Itoa(t.port))
}

// Hostname is the host as written, without port or IPv6 brackets. The broker
// routes it as a world name, which it matches case sensitively.
func (t Target) Hostname() string { return t.hostname }

// AuthorityURL is the identity of the server: mark://host in lowercase, with
// a port only when it is not the default (ADR 0005, ADR 0018).
func (t Target) AuthorityURL() string {
	host := strings.ToLower(t.hostname)
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if t.port != protocol.DefaultPort {
		host += ":" + strconv.Itoa(t.port)
	}
	return markScheme + host
}

// NodeURL is the graph identity of the document.
func (t Target) NodeURL() string { return t.AuthorityURL() + t.escapedPath }

// AuthorityURL builds a server identity from host[:port]. A host that does
// not parse is returned as written, so a caller never loses what it was given.
func AuthorityURL(host string) string {
	target, err := ParseMark(markScheme + host)
	if err != nil {
		return markScheme + host
	}
	return target.AuthorityURL()
}

// DialHost parses raw and returns only the address to dial, for callers that
// name a server rather than a document.
func DialHost(raw string) (string, error) {
	target, err := ParseMark(raw)
	if err != nil {
		return "", err
	}
	return target.DialHost(), nil
}
