package audit

import (
	"fmt"
	"strings"

	"github.com/latebit-io/demarkus/client/memory/gate"
	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/hiddentext"
	"github.com/latebit-io/demarkus/protocol/publishpolicy"
)

// Freshness is the baseline drift is judged against: how much of the scope
// is pinned, how much of that this audit could check, and how far behind.
type Freshness struct {
	// Eligible counts the live documents a pin is expected on; Pinned those
	// naming at least one versioned source.
	Eligible int `json:"eligible"`
	Pinned   int `json:"pinned"`
	// Owned counts the eligible documents with an owner.
	Owned int `json:"owned"`
	// Pins counts every rel-based-on value; PinsChecked those on a document
	// in scope, the only sources this audit reads.
	Pins        int `json:"pins"`
	PinsChecked int `json:"pins_checked"`
	// DocumentsChecked have a checked pin; DocumentsDrifted have one behind
	// its source or on a source no longer live.
	DocumentsChecked int `json:"documents_checked"`
	DocumentsDrifted int `json:"documents_drifted"`
	// DocumentsUnowned are the drifted ones with nobody to send them to.
	DocumentsUnowned int `json:"documents_unowned,omitempty"`
	MedianBehind     int `json:"median_behind,omitempty"`
	MaxBehind        int `json:"max_behind,omitempty"`
}

// pinExpected reports a document the pin coverage counts: every one a
// policy layer governs, navigation files aside.
func (a *audit) pinExpected(p string) bool {
	return !gate.NavExempt(p) && !publishpolicy.Exempt(a.opts.Floor, a.own, p)
}

// OwnerKey is the metadata naming who keeps a document current, for when
// the last writer is not that person.
const OwnerKey = "owner"

// ownerOf is who a document's drift goes to: its owner key, else the verified
// user behind its last write; "" when the store recorded neither.
func ownerOf(meta map[string]string) string {
	if o := strings.TrimSpace(meta[OwnerKey]); o != "" {
		return hiddentext.Reveal(o)
	}
	return meta[protocol.MetaUser]
}

// Lines are the baseline's report lines: coverage always, the rest when
// counted. Ownership reads only in a store that records owners.
func (f *Freshness) Lines() []string {
	pinned := fmt.Sprintf("pinned: %d of %d documents", f.Pinned, f.Eligible)
	if f.Eligible > 0 {
		pinned += fmt.Sprintf(" (%d%%)", 100*f.Pinned/f.Eligible)
	}
	lines := []string{pinned}
	if f.Owned > 0 {
		lines = append(lines, fmt.Sprintf("owned: %d of %d documents (%d%%)", f.Owned, f.Eligible, 100*f.Owned/f.Eligible))
	}
	if f.Pins > 0 {
		lines = append(lines, fmt.Sprintf("pins: %d of %d checked", f.PinsChecked, f.Pins))
	}
	if f.DocumentsChecked > 0 {
		drifted := fmt.Sprintf("drifted: %d of %d documents checked", f.DocumentsDrifted, f.DocumentsChecked)
		if f.MaxBehind > 0 {
			drifted += fmt.Sprintf("; median %d versions behind, most %d", f.MedianBehind, f.MaxBehind)
		}
		if f.Owned > 0 && f.DocumentsUnowned > 0 {
			drifted += fmt.Sprintf("; %d with no owner", f.DocumentsUnowned)
		}
		lines = append(lines, drifted)
	}
	return lines
}
