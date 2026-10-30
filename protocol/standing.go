package protocol

import "strings"

// Standing is a document's trust tier from its status: tag axis, a LOOKUP
// ranking weight (SPEC §6.7). The zero value is provisional, so an entry
// never classified ranks as an untagged one.
type Standing uint8

// Standing tiers, by StandingOf.
const (
	StandingProvisional Standing = iota // no status, draft, wip, or any other value
	StandingAccepted
	StandingRetired // archived, superseded, deprecated: a soft archive, still matched
)

// statusAxis prefixes the tag that sets standing; the first one wins.
const statusAxis = "status:"

// standingOfStatus maps each status value to its tier; several values share
// one so display vocabulary (library, OKF) can change without reranking.
var standingOfStatus = map[string]Standing{
	"draft":      StandingProvisional,
	"wip":        StandingProvisional,
	"accepted":   StandingAccepted,
	"stable":     StandingAccepted, // OKF
	"archived":   StandingRetired,
	"superseded": StandingRetired,
	"deprecated": StandingRetired, // OKF
}

// StatusOf returns the first status: tag's value, trimmed and lowercased.
func StatusOf(tags []string) (string, bool) {
	for _, tag := range tags {
		if len(tag) >= len(statusAxis) && strings.EqualFold(tag[:len(statusAxis)], statusAxis) {
			return strings.ToLower(strings.TrimSpace(tag[len(statusAxis):])), true
		}
	}
	return "", false
}

// StandingOfStatus is a status value's tier; an unknown value is provisional.
func StandingOfStatus(value string) Standing {
	return standingOfStatus[strings.ToLower(strings.TrimSpace(value))]
}

// StandingOf classifies tags by their first status: tag.
func StandingOf(tags []string) Standing {
	value, _ := StatusOf(tags) // no status tag: "", provisional
	return standingOfStatus[value]
}

// Weight scales importance in LOOKUP ranking (SPEC §6.7).
func (s Standing) Weight() float64 {
	switch s {
	case StandingAccepted:
		return 1.0
	case StandingRetired:
		return 0.3
	default:
		return 0.8
	}
}

// Scale is importance weighted by standing, LOOKUP's effective importance.
func (s Standing) Scale(importance float64) float64 {
	return importance * s.Weight()
}
