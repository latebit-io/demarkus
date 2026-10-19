package writepolicy

import (
	"errors"
	"fmt"
	"strings"

	"github.com/latebit-io/demarkus/protocol/contentcheck"
	"github.com/latebit-io/demarkus/protocol/storefmt"
	"github.com/latebit-io/demarkus/server/internal/backend"
)

// ErrContentBlocked reports a write refused for a secret or personal data in
// its body.
var ErrContentBlocked = errors.New("content check blocked mutation")

// ContentOptions configures the content scan on every write. The zero
// value blocks secrets and personal data with no work domains. What a scan
// lets through with a note reaches the writer through WriteRequest.Warnings.
type ContentOptions struct {
	contentcheck.Options
	// SecretsWarn lets a secret land with a note, the one release a tier
	// warns before it refuses; the rules always run.
	SecretsWarn bool
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
// removed, a personal-data false positive is overridden; the matched text
// never appears.
func (err *ContentError) RejectionMessage() string {
	var b strings.Builder
	b.WriteString("content check refused this write (a secret or personal data in the body):\n")
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
	return contentGate{scanner: contentcheck.New(options.Options), options: options}
}

// judge scans the prepared body; notes are what it let through. A secret
// refuses unless the tier warns, override or not; personal data follows its
// mode, and an override lets it land, reported with its reason.
func (g contentGate) judge(write storefmt.PreparedWrite) ([]string, error) {
	findings := g.scanner.Scan(string(write.Content))
	if len(findings) == 0 {
		return nil, nil
	}
	var secrets, personal []contentcheck.Finding
	for _, f := range findings {
		if f.Kind == contentcheck.Secret {
			secrets = append(secrets, f)
		} else {
			personal = append(personal, f)
		}
	}
	if len(secrets) > 0 && !g.options.SecretsWarn {
		return nil, &ContentError{Findings: findings}
	}
	var notes []string
	if len(secrets) > 0 {
		notes = append(notes, note(secrets, ""))
	}
	if len(personal) > 0 {
		override := strings.TrimSpace(write.Metadata[contentcheck.OverrideKey])
		switch {
		case override != "":
			notes = append(notes, note(personal, override))
		case g.options.PII != contentcheck.Warn:
			return nil, &ContentError{Findings: personal}
		default:
			notes = append(notes, note(personal, ""))
		}
	}
	return notes, nil
}

// note words findings that landed for the writer: rule and line, never the
// value; the write is in history now, so the fix is the stored remedy.
func note(findings []contentcheck.Finding, override string) string {
	if override != "" {
		return "content check overridden (" + override + "): " + contentcheck.Summary(findings)
	}
	return "content check: " + contentcheck.Summary(findings) + "; fix: " + contentcheck.StoredRemedy(findings)
}
