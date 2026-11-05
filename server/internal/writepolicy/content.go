package writepolicy

import (
	"errors"
	"fmt"
	"strings"

	"github.com/latebit-io/demarkus/protocol/contentcheck"
	"github.com/latebit-io/demarkus/server/internal/backend"
)

// ErrContentBlocked reports a write refused for a secret, personal data or
// instruction-shaped text in its body.
var ErrContentBlocked = errors.New("content check blocked mutation")

// ContentOptions configures the write scan. The zero value blocks secrets and
// personal data and warns on instruction-shaped text; what lands with a note
// reaches the writer through WriteRequest.Warnings.
type ContentOptions struct {
	contentcheck.Options
	// SecretsWarn lets a secret land with a note instead of refusing it: an
	// operator knob no tier sets by default; the rules always run.
	SecretsWarn bool
	// NearDuplicate is block (the default), warn (land with a note) or off
	// for a body the world already holds in other words; a write carrying
	// neardup.OverrideKey lands under every mode, its reason noted.
	NearDuplicate contentcheck.Mode
}

// ContentError carries the findings so the handler can list them without
// echoing a value.
type ContentError struct {
	Findings []contentcheck.Finding
}

func (err *ContentError) Error() string {
	return fmt.Sprintf("content check: %d finding(s)", len(err.Findings))
}

func (err *ContentError) Unwrap() []error {
	return []error{ErrContentBlocked, backend.ErrRejected}
}

// RejectionMessage names each rule and line and the way out: a secret is
// removed, a personal-data or injection false positive is overridden; the
// matched text never appears.
func (err *ContentError) RejectionMessage() string {
	var b strings.Builder
	b.WriteString("content check refused this write (" + contentcheck.Describe(err.Findings) + " in the body):\n")
	for _, f := range err.Findings {
		fmt.Fprintf(&b, "- %s\n", f)
	}
	b.WriteString("Fix: " + contentcheck.Remedy(err.Findings) + ".\n")
	return b.String()
}

// contentGate is the compiled scan plus the verdict rules.
type contentGate struct {
	scanner *contentcheck.Scanner
	options ContentOptions
}

func newContentGate(options ContentOptions) contentGate {
	if options.PII == "" {
		options.PII = contentcheck.Block
	}
	if options.Steering == "" {
		options.Steering = contentcheck.Warn
	}
	return contentGate{scanner: contentcheck.New(options.Options), options: options}
}

// judge scans the prepared body; notes are what it let through.
func (g contentGate) judge(body string, meta map[string]string) ([]string, error) {
	return g.verdict(g.scanner.Scan(body), meta)
}

// verdict decides a body's findings. A secret refuses unless the tier warns;
// personal data and steering text follow their modes, and an override lands
// them with its reason.
func (g contentGate) verdict(findings []contentcheck.Finding, meta map[string]string) ([]string, error) {
	if len(findings) == 0 {
		return nil, nil
	}
	secrets := contentcheck.OfKind(findings, contentcheck.Secret)
	if len(secrets) > 0 && !g.options.SecretsWarn {
		return nil, &ContentError{Findings: findings}
	}
	var notes []string
	if len(secrets) > 0 {
		notes = append(notes, note(secrets, ""))
	}
	override := strings.TrimSpace(meta[contentcheck.OverrideKey])
	steering, _ := contentcheck.SplitSteering(findings)
	for _, group := range []struct {
		hits []contentcheck.Finding
		mode contentcheck.Mode
	}{{contentcheck.OfKind(findings, contentcheck.PII), g.options.PII}, {steering, g.options.Steering}} {
		switch {
		case len(group.hits) == 0:
		case override != "":
			notes = append(notes, note(group.hits, override))
		case group.mode != contentcheck.Warn:
			return nil, &ContentError{Findings: group.hits}
		default:
			notes = append(notes, note(group.hits, ""))
		}
	}
	return notes, nil
}

// note words findings that landed for the writer: rule and line, never the
// value; the write is in history now, so the fix is the stored remedy.
func note(findings []contentcheck.Finding, override string) string {
	if override != "" {
		return "content check overridden (" + override + "): " + contentcheck.Brief(findings)
	}
	return "content check: " + contentcheck.Brief(findings) + "; fix: " + contentcheck.StoredRemedy(findings)
}
