// Package contentcheck finds secrets, personal data and instruction-shaped
// text in a document body before it lands in a shared, versioned catalog,
// where history keeps it. The server runs it as a write precondition and the
// doctor over a stored corpus. A finding names the rule and the line, never
// the matched value.
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
	// Injection is text addressed to an agent rather than a reader: a
	// heuristic, so warned by default and overridable with a reason.
	Injection Kind = "injection"
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

// OverrideKey is the metadata key that lets a write land despite personal-
// data or injection findings; its value is the reason, recorded with the
// version so overrides can be listed and reviewed. A secret never is.
const OverrideKey = "content-check-override"

// Finding is one rule hit on one line. Via names how the text was hidden
// from the plain rules (ViaObfuscated, ViaBase64, ...), "" when it was not.
type Finding struct {
	Rule string
	Kind Kind
	Line int
	Via  string
}

func (f Finding) String() string {
	if f.Via != "" {
		return fmt.Sprintf("%s/%s (%s) at line %d", f.Kind, f.Rule, f.Via, f.Line)
	}
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
	// Steering is Off to skip the rules for text that steers an agent (the
	// kinds Steers reports); any other value, zero included, runs them.
	Steering Mode
}

// rule is one pattern plus an optional confirmation on its submatches, for
// the checksum and placeholder rules whose pattern alone over-matches.
type rule struct {
	name    string
	kind    Kind
	pattern *regexp.Regexp
	confirm func(sub []string) bool
	// words and tokens gate the pattern: a word must occur in the case-folded
	// line, or a token as a whole word (the pattern's \b on both sides).
	words, tokens []string
}

// Secret formats, high confidence only: a documented prefix plus a length,
// or a structure (PEM header, JWT segments) nothing else writes.
var secretRules = []rule{
	{name: "pem-private-key", kind: Secret, words: []string{"-----begin "}, pattern: regexp.MustCompile(`-{5}BEGIN [A-Z ]*PRIVATE KEY-{5}`)},
	{name: "aws-access-key", kind: Secret, words: []string{"akia", "asia"}, pattern: regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`)},
	{name: "github-token", kind: Secret, words: []string{"ghp_", "gho_", "ghu_", "ghs_", "ghr_", "github_pat_"}, pattern: regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{36,}|github_pat_[A-Za-z0-9_]{22,})\b`)},
	{name: "slack-token", kind: Secret, words: []string{"xoxa-", "xoxb-", "xoxp-", "xoxr-", "xoxs-"}, pattern: regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}\b`)},
	{name: "google-api-key", kind: Secret, words: []string{"aiza"}, pattern: regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}\b`)},
	{name: "anthropic-api-key", kind: Secret, words: []string{"sk-ant-"}, pattern: regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_-]{20,}\b`)},
	{name: "openai-api-key", kind: Secret, words: []string{"sk-"}, pattern: regexp.MustCompile(`\bsk-(?:proj-)?[A-Za-z0-9]{32,}\b`)},
	{name: "jwt", kind: Secret, words: []string{"eyj"}, pattern: regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\b`)},
	{name: "connection-string-password", kind: Secret, words: []string{"://"},
		pattern: regexp.MustCompile(`\b[a-z][a-z0-9+.-]*://[^\s:/@]+:([^\s@]+)@\S+`),
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
	rules    []rule
	steering []rule // the folded pass; nil when off
	squashed []rule // steering rules over squashed text; nil when off
}

// New compiles the rule set opts selects.
func New(opts Options) *Scanner {
	rules := slices.Clone(secretRules)
	if opts.PII != Off {
		rules = append(rules, piiRules...)
		if len(opts.WorkDomains) > 0 {
			rules = append(rules, emailRule(opts.WorkDomains))
		}
	}
	if opts.Steering == Off {
		return &Scanner{rules: rules}
	}
	return &Scanner{rules: slices.Concat(rules, steeringRules), steering: steeringRules, squashed: squashedRules}
}

// Scan applies the rules per line, code fences included (a pasted credential
// is usually in one), also folded and with encoded runs decoded. One finding
// per rule per line, in line order, the plainest reading first.
func (s *Scanner) Scan(body string) []Finding { return s.scan(body, 0) }

func (s *Scanner) scan(body string, depth int) []Finding {
	var findings []Finding
	line := 0
	for text := range strings.SplitSeq(body, "\n") {
		line++
		var seen []string
		add := func(name string, kind Kind, via string) {
			if !slices.Contains(seen, name) {
				seen = append(seen, name)
				findings = append(findings, Finding{Rule: name, Kind: kind, Line: line, Via: via})
			}
		}
		matchRules(s.rules, text, func(r *rule) { add(r.name, r.kind, "") })
		if s.steering != nil && needsFold(text) {
			folded := fold(text)
			matchRules(s.steering, folded, func(r *rule) { add(r.name, r.kind, ViaObfuscated) })
			matchRules(s.squashed, squash(folded), func(r *rule) { add(r.name, r.kind, ViaObfuscated) })
		}
		if depth < maxDecodeDepth && mayEncode(text) {
			for _, d := range decodedRuns(text) {
				for _, f := range s.scan(d.text, depth+1) {
					add(f.Rule, f.Kind, d.via)
				}
			}
		}
	}
	return findings
}

// matchRules calls hit for each rule text matches. A rule with words or
// tokens runs only when one occurs in the line case-folded and space-led.
func matchRules(rules []rule, text string, hit func(*rule)) {
	lower := ""
	for i := range rules {
		r := &rules[i]
		if len(r.words)+len(r.tokens) > 0 {
			if lower == "" {
				lower = " " + strings.ToLower(strings.Map(caseFoldRune, text))
			}
			if !slices.ContainsFunc(r.words, func(w string) bool { return strings.Contains(lower, w) }) &&
				!slices.ContainsFunc(r.tokens, func(w string) bool { return hasToken(lower, w) }) {
				continue
			}
		}
		if r.hits(text) {
			hit(r)
		}
	}
}

// hasToken reports tok in s between non-word bytes, as \b reads them.
func hasToken(s, tok string) bool {
	for i := 0; ; {
		j := strings.Index(s[i:], tok)
		if j < 0 {
			return false
		}
		start, end := i+j, i+j+len(tok)
		if (start == 0 || !isWordByte(s[start-1])) && (end == len(s) || !isWordByte(s[end])) {
			return true
		}
		i = start + 1
	}
}

func isWordByte(c byte) bool {
	return c == '_' || c >= '0' && c <= '9' || c|0x20 >= 'a' && c|0x20 <= 'z'
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
// subdomain of one) nor a reserved name.
func emailRule(workDomains []string) rule {
	allowed := make([]string, 0, len(workDomains))
	for _, d := range workDomains {
		if d = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(d), "@")); d != "" {
			allowed = append(allowed, d)
		}
	}
	return rule{name: "email-outside-work-domains", kind: PII, pattern: emailPattern, confirm: func(sub []string) bool {
		domain := strings.ToLower(sub[1])
		return !ReservedName(domain) && !UnderDomain(domain, allowed)
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
func Secrets(findings []Finding) bool { return HasKind(findings, Secret) }

// HasKind reports whether any finding is of kind k.
func HasKind(findings []Finding, k Kind) bool {
	return slices.ContainsFunc(findings, func(f Finding) bool { return f.Kind == k })
}

// SplitSteering separates the findings Steers reports from the rest, each
// in order.
func SplitSteering(findings []Finding) (steering, rest []Finding) {
	for _, f := range findings {
		if Steers(f.Kind) {
			steering = append(steering, f)
		} else {
			rest = append(rest, f)
		}
	}
	return steering, rest
}

// OfKind keeps the findings of kind k, in order.
func OfKind(findings []Finding, k Kind) []Finding {
	var out []Finding
	for _, f := range findings {
		if f.Kind == k {
			out = append(out, f)
		}
	}
	return out
}

// Remedy is the one sentence every surface offers for findings on a refused
// write: a secret is removed; personal data or instruction-shaped text is
// removed or overridden with a reason.
func Remedy(findings []Finding) string {
	if Secrets(findings) {
		return "remove the secret and re-publish; a secret cannot be overridden"
	}
	override := "re-publish with metadata " + OverrideKey + " set to the reason; the override is recorded with the version"
	if !HasKind(findings, PII) {
		return "an agent reading it may act on it: remove it and re-publish, or if it is quoted for readers " + override
	}
	return "remove it and re-publish, or for a false positive " + override
}

// StoredRemedy is Remedy for findings on a document already published, where
// every earlier version keeps the body: a stored secret is compromised and
// re-publishing does not take it out of history.
func StoredRemedy(findings []Finding) string {
	if Secrets(findings) {
		return "treat the secret as compromised: rotate or revoke it, then remove it and re-publish; earlier versions keep it until the operator purges them"
	}
	return Remedy(findings)
}

// Describe names the kinds findings carry, for a refusal or a warning:
// "a secret", "personal data and text addressed to an agent".
func Describe(findings []Finding) string {
	var nouns []string
	for _, k := range []struct {
		kind Kind
		noun string
	}{{Secret, "a secret"}, {PII, "personal data"}, {Injection, "text addressed to an agent"}, {Command, "a dangerous command"}, {Link, "a suspicious link"}} {
		if HasKind(findings, k.kind) {
			nouns = append(nouns, k.noun)
		}
	}
	if len(nouns) < 2 {
		return strings.Join(nouns, "")
	}
	return strings.Join(nouns[:len(nouns)-1], ", ") + " and " + nouns[len(nouns)-1]
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
