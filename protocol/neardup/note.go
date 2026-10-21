package neardup

import (
	"fmt"
	"strings"
)

// String words the hit as a gate warns and a report lists it.
func (h Hit) String() string {
	if h.Verdict == VerdictNearDuplicate {
		return fmt.Sprintf("near-duplicate of %s (%.2f shared)", h.Location(), h.Score)
	}
	return fmt.Sprintf("contained in %s (%.2f)", h.Location(), h.Score)
}

// Why is the verdict and score alone, for a row that already names the path.
func (h Hit) Why() string {
	if h.Verdict == VerdictNearDuplicate {
		return fmt.Sprintf("near-duplicate (%.2f shared)", h.Score)
	}
	return fmt.Sprintf("contained (%.2f)", h.Score)
}

// NotePrefix opens the gate's warning line.
const NotePrefix = "near-duplicate check: "

// noteFix follows the hits.
const noteFix = "; fix: keep one document, append to or link the existing one"

// Note words the hits for the writer; "" when there are none.
func Note(hits []Hit) string {
	if len(hits) == 0 {
		return ""
	}
	parts := make([]string, len(hits))
	for i, h := range hits {
		parts[i] = h.String()
	}
	return NotePrefix + strings.Join(parts, ", ") + noteFix
}
