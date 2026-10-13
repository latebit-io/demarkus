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
// value blocks personal data with no work domains.
type ContentOptions struct {
	contentcheck.Options
	// Warn reports findings that did not refuse the write: a warn-mode PII
	// hit, or any hit under an override. Nil drops them.
	Warn func(path string, findings []contentcheck.Finding, override string)
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

// RejectionMessage names each rule and line and how to override a false
// positive; the matched text never appears.
func (err *ContentError) RejectionMessage() string {
	var b strings.Builder
	b.WriteString("content check refused this write (a secret or personal data in the body):\n")
	for _, f := range err.Findings {
		fmt.Fprintf(&b, "- %s\n", f)
	}
	fmt.Fprintf(&b, "Remove it, or for a false positive set metadata %s to the reason; the override is recorded with the version.\n", contentcheck.OverrideKey)
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

// judge scans the prepared body. Secrets refuse regardless of mode; PII
// follows the mode. An override lets any finding land and is reported.
func (g contentGate) judge(write storefmt.PreparedWrite) error {
	findings := g.scanner.Scan(string(write.Content))
	if len(findings) == 0 {
		return nil
	}
	if override := strings.TrimSpace(write.Metadata[contentcheck.OverrideKey]); override != "" {
		g.warn(write.Path, findings, override)
		return nil
	}
	if contentcheck.Secrets(findings) || g.options.PII != contentcheck.Warn {
		return &ContentError{Findings: findings}
	}
	g.warn(write.Path, findings, "")
	return nil
}

func (g contentGate) warn(path string, findings []contentcheck.Finding, override string) {
	if g.options.Warn != nil {
		g.options.Warn(path, findings, override)
	}
}
