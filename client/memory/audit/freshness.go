package audit

import (
	"fmt"
	"strings"

	"github.com/latebit-io/demarkus/client/memory/gate"
	"github.com/latebit-io/demarkus/protocol/publishpolicy"
)

// Freshness is the baseline drift is judged against: how much of the scope
// is pinned, how much of that this audit could check, and how far behind.
type Freshness struct {
	// Eligible counts the live documents a pin is expected on; Pinned those
	// naming at least one versioned source.
	Eligible int `json:"eligible"`
	Pinned   int `json:"pinned"`
	// Pins counts every rel-based-on value; PinsChecked those on a document
	// in scope, the only sources this audit reads.
	Pins        int `json:"pins"`
	PinsChecked int `json:"pins_checked"`
	// DocumentsChecked have a checked pin; DocumentsDrifted have one behind
	// its source or on a source no longer live.
	DocumentsChecked int `json:"documents_checked"`
	DocumentsDrifted int `json:"documents_drifted"`
	MedianBehind     int `json:"median_behind,omitempty"`
	MaxBehind        int `json:"max_behind,omitempty"`
}

// pinExpected reports a document the pin coverage counts: every one a
// policy layer governs, navigation files aside.
func (a *audit) pinExpected(p string) bool {
	return !gate.NavExempt(p) && !publishpolicy.Exempt(a.opts.Floor, a.own, p)
}

// markdown renders the section: coverage always, the rest when counted.
func (f *Freshness) markdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "\n### Freshness\n- pinned: %d of %d documents", f.Pinned, f.Eligible)
	if f.Eligible > 0 {
		fmt.Fprintf(&b, " (%d%%)", 100*f.Pinned/f.Eligible)
	}
	b.WriteByte('\n')
	if f.Pins > 0 {
		fmt.Fprintf(&b, "- pins: %d of %d checked", f.PinsChecked, f.Pins)
		if f.PinsChecked < f.Pins {
			b.WriteString("; the rest name a source this audit cannot read")
		}
		b.WriteByte('\n')
	}
	if f.DocumentsChecked > 0 {
		fmt.Fprintf(&b, "- drifted: %d of %d documents checked", f.DocumentsDrifted, f.DocumentsChecked)
		if f.MaxBehind > 0 {
			fmt.Fprintf(&b, "; median %d versions behind, most %d", f.MedianBehind, f.MaxBehind)
		}
		b.WriteByte('\n')
	}
	return b.String()
}
