package audit

import (
	"slices"
	"strconv"
	"strings"

	"github.com/latebit-io/demarkus/protocol/provenance"
)

// checkDrift compares each pin on a document in this store with its current
// version: a source past its pin may leave the pinning document stale. Pins
// on other stores, repositories or the web need a reader this audit lacks.
func (a *audit) checkDrift() {
	outside := 0
	var behind []int
	for _, p := range a.order {
		d := a.docs[p]
		if !live(d.status) {
			continue
		}
		pins := provenance.Parse(d.meta[provenance.Key])
		if a.pinExpected(p) {
			a.fresh.Eligible++
			if slices.ContainsFunc(pins, provenance.Pin.Pinned) {
				a.fresh.Pinned++
			}
		}
		a.fresh.Pins += len(pins)
		// Only drift findings are added below, so a new one is this document's.
		checked, found := a.fresh.PinsChecked, len(a.findings)
		for _, pin := range pins {
			pinned := pin.DocumentVersion()
			target := ""
			if pinned > 0 {
				target = a.resolve.Path(p, pin.Source)
			}
			if target == "" {
				continue
			}
			source, inventoried := a.docs[target]
			switch {
			case !inventoried && !strings.HasPrefix(target, a.opts.Scope):
				outside++
			case !inventoried || !live(source.status):
				a.fresh.PinsChecked++
				a.add(CheckDrift, p, "based on "+target+", which is not a live document here", "pin the document that replaced it, or drop the pin")
			default:
				current, err := strconv.Atoi(source.meta["version"])
				if err != nil {
					a.note("%s returned version %q; the pin from %s not checked", target, source.meta["version"], p)
					continue
				}
				a.fresh.PinsChecked++
				if problem, fix := provenance.Drift(pinned, current); problem != "" {
					a.add(CheckDrift, p, "based on "+target+", which "+problem, fix)
				}
				if current > pinned {
					behind = append(behind, current-pinned)
				}
			}
		}
		if a.fresh.PinsChecked > checked {
			a.fresh.DocumentsChecked++
		}
		if len(a.findings) > found {
			a.fresh.DocumentsDrifted++
		}
	}
	if n := len(behind); n > 0 {
		slices.Sort(behind)
		a.fresh.MedianBehind, a.fresh.MaxBehind = behind[n/2], behind[n-1]
	}
	if outside > 0 {
		a.note("%d %s pins on documents outside the scope not checked for drift", outside, provenance.Key)
	}
}
