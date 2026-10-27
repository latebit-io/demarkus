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

// maxDependentsNamed bounds the documents one dependents note names.
const maxDependentsNamed = 10

// pinNotes is what drift a write meets: its own pins behind their sources,
// and documents pinned to an earlier version of this one. Read before the
// commit and only ever noted, so a race costs a stale note.
func pinNotes(ctx context.Context, view backend.ReadView, req backend.WriteRequest) []string {
	return append(stalePins(ctx, view, req), dependents(ctx, view, req.Path)...)
}

// stalePins checks each path pin the write declares against the current
// version here, reading each source once; mark:// URL pins name a world
// this store cannot see.
func stalePins(ctx context.Context, reader backend.Reader, req backend.WriteRequest) []string {
	type read struct {
		doc *storefmt.Document
		err error
	}
	var notes []string
	reads := map[string]read{}
	for _, pin := range provenance.Parse(req.Metadata[provenance.Key]) {
		target, pinned := pin.Local(req.Path), pin.DocumentVersion()
		if target == "" || pinned == 0 {
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

// dependents names the documents pinned to docPath: every pin is on a
// version this write replaces, so each may now be stale.
func dependents(ctx context.Context, view backend.CatalogReader, docPath string) []string {
	own := storefmt.CanonicalPath(docPath)
	rows, err := view.Lookup(ctx, "*", catalog.Options{Filter: []catalog.Predicate{catalog.BasedOn(own)}, Max: maxDependentsNamed + 2})
	if err != nil {
		return []string{"dependents not checked: " + err.Error()}
	}
	var paths []string
	for i := range rows {
		if rows[i].Path != own {
			paths = append(paths, rows[i].Path)
		}
	}
	if len(paths) == 0 {
		return nil
	}
	named := paths[:min(len(paths), maxDependentsNamed)]
	more := ""
	if len(paths) > len(named) {
		more = " and more"
	}
	return []string{fmt.Sprintf("based on an earlier version of this document: %s%s; check them against this change and re-pin", strings.Join(named, ", "), more)}
}
