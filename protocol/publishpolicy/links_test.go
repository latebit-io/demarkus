package publishpolicy

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseAllowDomains(t *testing.T) {
	if got := Parse("strictness: block\n").AllowDomains; got != nil {
		t.Errorf("absent = %v, want nil (unchecked)", got)
	}
	if got := Parse("allow_domains: GitHub.com | docs.example.org\n").AllowDomains; !reflect.DeepEqual(got, []string{"github.com", "docs.example.org"}) {
		t.Errorf("parsed = %v", got)
	}
	blank := Parse("allow_domains:\n")
	if blank.AllowDomains == nil || blank.Validate() == nil {
		t.Errorf("blank list = %#v, want empty and refused", blank.AllowDomains)
	}
	for _, bad := range []string{"https://a.com", "a.com:443", "a.com/x", "*.a.com", "a..com"} {
		if err := Parse("allow_domains: " + bad + "\n").Validate(); err == nil {
			t.Errorf("allow_domains %q validated", bad)
		}
	}
}

func TestDisallowedHosts(t *testing.T) {
	policy := Policy{AllowDomains: []string{"github.com", "latebit.io"}}
	body := strings.Join([]string{
		"[repo](https://github.com/latebit-io/demarkus) and [docs](https://docs.latebit.io/x)",
		"![beacon](https://evil.example.net.attacker.io/p.png?d=1)",
		"<https://Mixed.Case.Host:8443/path>",
		"bare https://user:pw@leak.io/x, then again https://leak.io/y.",
		"[rel](//cdn.other.org/lib.js) <img src=\"//img.other.org/a.png\">",
		"local http://localhost:8080 http://127.0.0.1:9000 http://[::1]/ http://203.0.113.5/x https://api.example.com https://x.test",
		"internal [hub](mark://team/index.md) and [rel](./a.md)",
	}, "\n")
	got := disallowedHosts(policy, body)
	want := []string{"evil.example.net.attacker.io", "mixed.case.host", "leak.io", "cdn.other.org", "img.other.org"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("DisallowedHosts = %v\nwant %v", got, want)
	}
	if got := disallowedHosts(Policy{}, body); got != nil {
		t.Errorf("unset list = %v, want unchecked", got)
	}
}

func TestEvaluateBodyAddsLinkViolations(t *testing.T) {
	policy := Policy{AllowDomains: []string{"github.com"}}
	r := EvaluateBody(policy, "/a.md", map[string]string{"tags": "a"}, "see https://evil.io/x\n")
	if !r.Has(DisallowedLinkDomain) || !reflect.DeepEqual(r.Names(DisallowedLinkDomain), []string{"evil.io"}) {
		t.Errorf("result = %+v", r)
	}
	if r := EvaluateBody(policy, "/a.md", map[string]string{"tags": "a"}, "see https://github.com/x\n"); !r.Compliant() {
		t.Errorf("allowed link = %+v", r)
	}
}

func TestMergeDomainsOnlyTightens(t *testing.T) {
	cases := []struct {
		floor, world, want []string
	}{
		{nil, nil, nil},
		{[]string{"github.com"}, nil, []string{"github.com"}},
		{nil, []string{"a.io"}, []string{"a.io"}},
		{[]string{"github.com", "latebit.io"}, []string{"docs.github.com", "evil.io"}, []string{"docs.github.com"}},
		{[]string{"a.io"}, []string{"b.io"}, []string{}},
	}
	for _, c := range cases {
		got := Merge(Policy{AllowDomains: c.floor}, Policy{AllowDomains: c.world}).AllowDomains
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("Merge(%v, %v) = %#v, want %#v", c.floor, c.world, got, c.want)
		}
	}
	if Merge(Policy{AllowDomains: []string{"a.io"}}, Policy{AllowDomains: []string{"b.io"}}).Validate() == nil {
		t.Error("a disjoint merge validated")
	}
	loosens := Loosens(Policy{AllowDomains: []string{"github.com"}}, Policy{AllowDomains: []string{"docs.github.com", "evil.io"}})
	if len(loosens) != 1 || !strings.Contains(loosens[0], "allow_domains adds evil.io") {
		t.Errorf("Loosens = %v", loosens)
	}
}

func TestExemptPathSkipsLinks(t *testing.T) {
	p := Parse("allow_domains: github.com\nexempt: /generated/**\n")
	if got := disallowedHosts(Effective(nil, p, "/generated/x.md"), "https://evil.io"); got != nil {
		t.Errorf("exempt path checked: %v", got)
	}
}
