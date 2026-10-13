// Package contentcheck finds secrets and personal data in a document body
// before it lands in a shared, versioned catalog, where history keeps it. The
// server runs it as a write precondition and the doctor over a stored corpus.
// A finding names the rule and the line, never the matched value.
package contentcheck

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// Kind separates the rules a policy may relax (PII) from the ones it never
// does (secrets).
type Kind string

const (
	// Secret is a credential: always blocked.
	Secret Kind = "secret"
	// PII is personal data: blocked, warned or ignored per Mode.
	PII Kind = "pii"
)

// Mode is how a personal-data finding is enforced; secrets always block.
type Mode string

const (
	// Block refuses the write.
	Block Mode = "block"
	// Warn reports the finding and lets the write land.
	Warn Mode = "warn"
	// Off skips the personal-data rules.
	Off Mode = "off"
)

// Valid reports a Mode the config accepts.
func (m Mode) Valid() bool { return m == Block || m == Warn || m == Off }

// OverrideKey is the metadata key that lets a write land despite findings;
// its value is the reason, recorded with the version so overrides can be
// listed and reviewed.
const OverrideKey = "content-check-override"

// Finding is one rule hit on one line.
type Finding struct {
	Rule string
	Kind Kind
	Line int
}

func (f Finding) String() string {
	return fmt.Sprintf("%s/%s at line %d", f.Kind, f.Rule, f.Line)
}

// Options select the rule set; the secret rules always run.
type Options struct {
	// PII is Off to skip the personal-data rules; Block and Warn both run
	// them (the caller decides what a hit means).
	PII Mode
	// WorkDomains are the email domains a knowledge system treats as its
	// own; an address outside them is a PII finding. Empty disables the
	// email rule, since every address would otherwise count.
	WorkDomains []string
}

// rule is one pattern plus an optional confirmation on its submatches, for
// the checksum and placeholder rules whose pattern alone over-matches.
type rule struct {
	name    string
	kind    Kind
	pattern *regexp.Regexp
	confirm func(sub []string) bool
}

// Secret formats, high confidence only: a documented prefix plus a length,
// or a structure (PEM header, JWT segments) nothing else writes.
var secretRules = []rule{
	{name: "pem-private-key", kind: Secret, pattern: regexp.MustCompile(`-{5}BEGIN [A-Z ]*PRIVATE KEY-{5}`)},
	{name: "aws-access-key", kind: Secret, pattern: regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`)},
	{name: "github-token", kind: Secret, pattern: regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{36,}|github_pat_[A-Za-z0-9_]{22,})\b`)},
	{name: "slack-token", kind: Secret, pattern: regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}\b`)},
	{name: "google-api-key", kind: Secret, pattern: regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}\b`)},
	{name: "anthropic-api-key", kind: Secret, pattern: regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_-]{20,}\b`)},
	{name: "openai-api-key", kind: Secret, pattern: regexp.MustCompile(`\bsk-(?:proj-)?[A-Za-z0-9]{32,}\b`)},
	{name: "jwt", kind: Secret, pattern: regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\b`)},
	{name: "connection-string-password", kind: Secret,
		pattern: regexp.MustCompile(`\b[a-z][a-z0-9+.-]*://[^\s:/@]+:([^\s@/]+)@\S+`),
		confirm: func(sub []string) bool { return !placeholderPassword.MatchString(sub[1]) }},
}

// placeholderPassword matches what documentation writes in the password
// slot of an example URL: a variable, an angle-bracket slot, a mask, or a
// word that says it is a placeholder.
var placeholderPassword = regexp.MustCompile(`(?i)^(?:\$\{?[A-Z0-9_]+\}?|<[^>]*>|\[[^\]]*\]|\*+|x+|\.\.\.|pass(?:word)?|secret|changeme|redacted|hunter2|example)$`)

var (
	// A SIN is written in three separated groups; nine bare digits are too
	// often an order or record number to flag on a checksum alone.
	sinPattern = regexp.MustCompile(`\b\d{3}[ -]\d{3}[ -]\d{3}\b`)
	// Card numbers: 13 to 19 digits, single separators allowed, Luhn-valid,
	// opening with a digit a card network issues.
	cardPattern  = regexp.MustCompile(`\b[3-6](?:\d[ -]?){11,17}\d\b`)
	emailPattern = regexp.MustCompile(`\b[A-Za-z0-9._%+-]+@((?:[A-Za-z0-9-]+\.)+[A-Za-z]{2,})\b`)
	digitRe      = regexp.MustCompile(`\D`)
)

// reservedDomains never name a person: documentation and test addresses.
var reservedDomains = []string{"example.com", "example.org", "example.net", "example.invalid", "localhost"}

var piiRules = []rule{
	{name: "canadian-sin", kind: PII, pattern: sinPattern, confirm: func(sub []string) bool { return luhn(digitRe.ReplaceAllString(sub[0], "")) }},
	{name: "card-number", kind: PII, pattern: cardPattern, confirm: func(sub []string) bool {
		digits := digitRe.ReplaceAllString(sub[0], "")
		return len(digits) >= 13 && len(digits) <= 19 && luhn(digits)
	}},
}

// Scanner is a compiled rule set; build one per configuration and reuse it,
// the write path runs it on every commit.
type Scanner struct {
	rules []rule
}

// New compiles the rule set opts selects.
func New(opts Options) *Scanner {
	if opts.PII == Off {
		return &Scanner{rules: secretRules}
	}
	rules := slices.Concat(secretRules, piiRules)
	if len(opts.WorkDomains) > 0 {
		rules = append(rules, emailRule(opts.WorkDomains))
	}
	return &Scanner{rules: rules}
}

// Scan applies the rules line by line, code fences included: a pasted
// credential is usually inside one. Findings are in line order, then rule
// order; one finding per rule per line.
func (s *Scanner) Scan(body string) []Finding {
	var findings []Finding
	line := 0
	for text := range strings.SplitSeq(body, "\n") {
		line++
		for _, r := range s.rules {
			if r.hits(text) {
				findings = append(findings, Finding{Rule: r.name, Kind: r.kind, Line: line})
			}
		}
	}
	return findings
}

// Scan is a one-off scan with a fresh Scanner.
func Scan(body string, opts Options) []Finding { return New(opts).Scan(body) }

func (r rule) hits(text string) bool {
	if r.confirm == nil {
		return r.pattern.MatchString(text)
	}
	return slices.ContainsFunc(r.pattern.FindAllStringSubmatch(text, -1), r.confirm)
}

// emailRule flags an address whose domain is neither a work domain (or a
// subdomain of one) nor reserved for examples.
func emailRule(workDomains []string) rule {
	allowed := make([]string, 0, len(workDomains)+len(reservedDomains))
	for _, d := range slices.Concat(workDomains, reservedDomains) {
		if d = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(d), "@")); d != "" {
			allowed = append(allowed, d)
		}
	}
	return rule{name: "email-outside-work-domains", kind: PII, pattern: emailPattern, confirm: func(sub []string) bool {
		domain := strings.ToLower(sub[1])
		return !slices.ContainsFunc(allowed, func(a string) bool { return domain == a || strings.HasSuffix(domain, "."+a) })
	}}
}

// luhn reports a digit string with a valid Luhn check digit.
func luhn(digits string) bool {
	sum, double := 0, false
	for i := len(digits) - 1; i >= 0; i-- {
		d := int(digits[i] - '0')
		if d < 0 || d > 9 {
			return false
		}
		if double {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
		double = !double
	}
	return len(digits) > 1 && sum%10 == 0
}

// Secrets reports whether any finding is a credential.
func Secrets(findings []Finding) bool {
	return slices.ContainsFunc(findings, func(f Finding) bool { return f.Kind == Secret })
}

// Summary words the findings for a refusal or a note: one per line, rule
// and line only.
func Summary(findings []Finding) string {
	parts := make([]string, len(findings))
	for i, f := range findings {
		parts[i] = f.String()
	}
	return strings.Join(parts, "; ")
}
