package contentcheck

import (
	"net"
	"net/netip"
	"regexp"
	"slices"
	"strings"
)

// Link is a link no allowlist is needed to doubt: a shortener hiding its
// target, a raw public address, a punycode host, a script or HTML data URL.
// Mode is Options.Steering.
const Link Kind = "link"

// linkAuthority captures the authority of an http(s) URL, a protocol-relative
// markdown target and a protocol-relative href or src.
var linkAuthority = regexp.MustCompile(`(?i)\bhttps?://([^\s/<>()"'` + "`" + `|?#\\]+)|\]\(\s*//([^\s/)?#]+)|\b(?:href|src)\s*=\s*["']?//([^\s/"'>?#]+)`)

var linkRules = []rule{
	{name: "url-shortener", kind: Link, words: []string{"bit.ly", "tinyurl", "t.co/", "goo.gl", "is.gd", "ow.ly", "buff.ly", "rebrand.ly", "cutt.ly", "shorturl.at", "tiny.cc", "rb.gy", "t.ly", "s.id/"}, pattern: regexp.MustCompile(`(?i)\bhttps?://(?:www\.)?(?:bit\.ly|tinyurl\.com|t\.co|goo\.gl|is\.gd|ow\.ly|buff\.ly|rebrand\.ly|cutt\.ly|shorturl\.at|tiny\.cc|rb\.gy|t\.ly|s\.id)/`)},
	{name: "raw-ip-host", kind: Link, words: []string{"//"}, pattern: linkAuthority, confirm: func(sub []string) bool {
		ip, err := netip.ParseAddr(hostOf(sub[1] + sub[2] + sub[3]))
		return err == nil && publicIP(ip)
	}},
	{name: "punycode-host", kind: Link, words: []string{"xn--"}, pattern: linkAuthority, confirm: func(sub []string) bool {
		host := hostOf(sub[1] + sub[2] + sub[3])
		return strings.HasPrefix(host, "xn--") || strings.Contains(host, ".xn--")
	}},
	{name: "script-url", kind: Link, words: []string{"javascript:", "vbscript:", "data:text/html", "data:application/"}, pattern: regexp.MustCompile(`(?i)(?:\]\(\s*|\b(?:href|src)\s*=\s*["']?)(?:javascript:|vbscript:|data:(?:text/html|application/(?:x-)?javascript))`)},
}

// LinkHosts lists the lowercase hosts text links to, first seen first. Only
// lines holding "//" can, each read with the line before for a target split
// after "](".
func LinkHosts(text string) []string {
	var out []string
	prev := ""
	for line := range strings.SplitSeq(text, "\n") {
		if strings.Contains(line, "//") {
			for _, m := range linkAuthority.FindAllStringSubmatch(prev+"\n"+line, -1) {
				if host := hostOf(m[1] + m[2] + m[3]); host != "" && !slices.Contains(out, host) {
					out = append(out, host)
				}
			}
		}
		prev = line
	}
	return out
}

// hostOf is the lowercase host of a URL authority: userinfo, port and IPv6
// brackets dropped.
func hostOf(authority string) string {
	if at := strings.LastIndexByte(authority, '@'); at >= 0 {
		authority = authority[at+1:]
	}
	if host, _, err := net.SplitHostPort(authority); err == nil {
		authority = host
	}
	return strings.TrimRight(strings.Trim(strings.ToLower(authority), "[]"), ".,;:!")
}

// reservedNames never name a reachable outside host (RFC 2606, 6761).
var reservedNames = []string{"localhost", "example", "test", "invalid", "example.com", "example.org", "example.net"}

// ReservedName reports a host under a reserved name: no one outside
// answers it and no person's mailbox is at it.
func ReservedName(host string) bool { return UnderDomain(host, reservedNames) }

// UnderDomain reports host equal to, or a subdomain of, a listed domain.
func UnderDomain(host string, domains []string) bool {
	return slices.ContainsFunc(domains, func(d string) bool { return host == d || strings.HasSuffix(host, "."+d) })
}

// documentation ranges are the addresses examples are written with.
var documentation = []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24"), netip.MustParsePrefix("198.51.100.0/24"), netip.MustParsePrefix("203.0.113.0/24"), netip.MustParsePrefix("2001:db8::/32")}

// DocumentationIP reports an address in a range reserved for examples.
func DocumentationIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	return slices.ContainsFunc(documentation, func(p netip.Prefix) bool { return p.Contains(ip) })
}

// publicIP reports an address someone outside answers: not loopback,
// private, link-local, unspecified or a documentation range.
func publicIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	return !ip.IsLoopback() && !ip.IsPrivate() && !ip.IsLinkLocalUnicast() && !ip.IsUnspecified() && !DocumentationIP(ip)
}
