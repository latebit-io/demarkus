package writepolicy

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/latebit-io/demarkus/protocol/provenance"
	"github.com/latebit-io/demarkus/protocol/storefmt"
	"github.com/latebit-io/demarkus/server/internal/backend"
	"github.com/latebit-io/demarkus/server/internal/catalog"
)

// maxDependentsNamed caps the documents a dependents note names; one more
// is read to say there are more.
const maxDependentsNamed = 10

// drift is what a write's notes about other documents need: the writer's
// read check and the dependents read before the commit.
type drift struct {
	readable   func(string) bool
	dependents []string
}

// notes are the prepared write's stale pins, then its dependents.
func (d drift) notes(ctx context.Context, state backend.Reader, write storefmt.PreparedWrite) []string {
	return append(stalePins(ctx, state, write, d.readable), d.dependents...)
}

// stalePins checks the prepared write's path pins, inherited by an append
// too, against the commit state, reading each source once and naming only
// what the writer may read; mark:// URL pins name a world not seen here.
func stalePins(ctx context.Context, reader backend.Reader, write storefmt.PreparedWrite, readable func(string) bool) []string {
	type read struct {
		doc *storefmt.Document
		err error
	}
	var notes []string
	reads := map[string]read{}
	for _, pin := range provenance.Parse(write.Metadata[provenance.Key]) {
		target, pinned := pin.Local(write.Path), pin.DocumentVersion()
		if target == "" || pinned == 0 || !readable(target) {
			continue
		}
		r, ok := reads[target]
		if !ok {
			r.doc, r.err = reader.Get(ctx, target, 0)
			reads[target] = r
		}
		doc, err := r.doc, r.err
		switch {
		case errors.Is(err, backend.ErrNotFound):
			notes = append(notes, fmt.Sprintf("%s: %s is not a document here", provenance.Key, target))
		case err != nil:
			notes = append(notes, fmt.Sprintf("%s: %s not checked: %v", provenance.Key, target, err))
		case doc.Archived:
			notes = append(notes, fmt.Sprintf("%s: %s is archived", provenance.Key, target))
		default:
			if problem, fix := provenance.Drift(pinned, doc.Version); problem != "" {
				notes = append(notes, fmt.Sprintf("%s: %s %s; %s", provenance.Key, target, problem, fix))
			}
		}
	}
	return notes
}

// dependents names the readable documents pinned to docPath, read before
// the commit: every pin is on a version this write replaces. The lookup
// skips unreadable rows, so they never take a slot under the cap.
func dependents(ctx context.Context, view backend.CatalogReader, docPath string, readable func(string) bool) []string {
	own := storefmt.CanonicalPath(docPath)
	rows, err := view.Lookup(ctx, "*", catalog.Options{
		Filter: []catalog.Predicate{catalog.BasedOn(own)},
		Skip:   func(p string) bool { return p == own || !readable(p) },
		Max:    maxDependentsNamed + 1,
	})
	if err != nil {
		return []string{"dependents not checked: " + err.Error()}
	}
	if len(rows) == 0 {
		return nil
	}
	named := make([]string, min(len(rows), maxDependentsNamed))
	for i := range named {
		named[i] = rows[i].Path
	}
	more := ""
	if len(rows) > len(named) {
		more = " and more"
	}
	return []string{fmt.Sprintf("based on an earlier version of this document: %s%s; check them against this change and re-pin", strings.Join(named, ", "), more)}
}
