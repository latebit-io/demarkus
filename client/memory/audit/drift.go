package audit

import (
	"context"
	"slices"
	"strconv"
	"strings"

	"github.com/latebit-io/demarkus/protocol/provenance"
)

// Drift runs the drift check and the freshness baseline alone, on metadata:
// no body is kept and no other check runs. It counts what Run counts, for a
// caller that audits whole stores on a schedule.
func Drift(ctx context.Context, store Store, opts Options) (*Report, error) {
	a, err := newAudit(store, opts)
	if err != nil {
		return nil, err
	}
	a.metaOnly = true
	ctx, cancel := context.WithTimeout(ctx, a.opts.Timeout)
	defer cancel()
	a.readPolicy(ctx)
	if err := a.inventory(ctx); err != nil {
		return nil, err
	}
	if err := a.fetchAll(ctx); err != nil {
		return nil, err
	}
	a.checkDrift()
	return &Report{Scope: a.opts.Scope, Documents: len(a.order), Freshness: a.fresh, Findings: a.findings, Coverage: a.coverage}, nil
}

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
		owner := ownerOf(d.meta)
		if a.pinExpected(p) {
			a.fresh.Eligible++
			if slices.ContainsFunc(pins, provenance.Pin.Pinned) {
				a.fresh.Pinned++
			}
			if owner != "" {
				a.fresh.Owned++
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
			if owner == "" {
				a.fresh.DocumentsUnowned++
			}
		}
		for i := found; i < len(a.findings); i++ {
			a.findings[i].Owner = owner
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
