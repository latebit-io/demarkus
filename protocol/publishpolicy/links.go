package publishpolicy

import (
	"net/netip"
	"slices"

	"github.com/latebit-io/demarkus/protocol/contentcheck"
)

// DisallowedLinkDomain means a body link points at a host outside the
// policy's allow_domains; Name is the host.
const DisallowedLinkDomain ViolationCode = "disallowed-link-domain"

// EvaluateBody checks a write's string metadata against policy, and its
// body's links against allow_domains.
func EvaluateBody(policy Policy, path string, metadata map[string]string, body string) Result {
	values := make(map[string]any, len(metadata))
	for key, value := range metadata {
		values[key] = value
	}
	r := evaluate(policy, path, values, false)
	for _, host := range disallowedHosts(policy, body) {
		r.Violations = append(r.Violations, Violation{Code: DisallowedLinkDomain, Name: host})
	}
	return r
}

// disallowedHosts lists the hosts body links point at outside the policy's
// allow_domains, first seen first; none when the list is unset. Loopback,
// documentation and reserved names always pass: no one outside answers them.
func disallowedHosts(policy Policy, body string) []string {
	if policy.AllowDomains == nil {
		return nil
	}
	return slices.DeleteFunc(contentcheck.LinkHosts(body), func(host string) bool {
		if ip, err := netip.ParseAddr(host); err == nil {
			return ip.Unmap().IsLoopback() || contentcheck.DocumentationIP(ip)
		}
		return contentcheck.ReservedName(host) || contentcheck.UnderDomain(host, policy.AllowDomains)
	})
}
