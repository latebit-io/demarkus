package landing

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/latebit-io/demarkus/client/docwrite"
	"github.com/latebit-io/demarkus/client/listwalk"
	"github.com/latebit-io/demarkus/client/memory/hub"
)

// Bounds on a recursive archive, so one call cannot sweep a whole store.
const (
	archiveListCalls = 100
	archiveDocs      = 500
)

// Archived is one archive the server answered and the hub bullets dropped
// for it. Hubs is empty unless it landed.
type Archived struct {
	Result docwrite.Result
	Landed bool
	Hubs   []hub.Dropped
}

// Archive archives one document; when it lands, every ancestor hub drops the
// bullets that only linked it. A hub that could not be edited is reported in
// Hubs, the archive stands.
func Archive(ctx context.Context, docs hub.Docs, path string, stamp map[string]string) (Archived, error) {
	result, err := docs(path).Archive(ctx)
	if err != nil {
		return Archived{}, err
	}
	out := Archived{Result: result, Landed: Landed(&result)}
	if out.Landed {
		out.Hubs = hub.DropFromHubs(ctx, docs, hub.Ancestors(path), &hub.Drop{Targets: []string{path}, Stamp: stamp})
	}
	return out, nil
}

// Sweep is a recursive archive: every live document under Dir. ExpectedDocs,
// when positive, is the live count the caller approved: another refuses.
type Sweep struct {
	Dir          string
	ExpectedDocs int
	// Walker lists the store; Sweep bounds its LIST calls.
	Walker listwalk.Walker
	Stamp  map[string]string
}

// SweptDoc is one document of a sweep: its answer, or why it has none.
type SweptDoc struct {
	Path   string
	Result docwrite.Result
	Err    error
}

// Swept is a sweep that ran. Complete: every live document was archived, so
// the directory's own bullet went; otherwise each archived path was unlinked
// from its own hubs.
type Swept struct {
	Docs     []SweptDoc
	Archived []SweptDoc
	Complete bool
	Hubs     []hub.Dropped
}

// A sweep's refusals, worded as the surfaces show them.
var (
	ErrNotADirectory = errors.New("recursive needs a directory url ending in /")
	ErrStoreRoot     = errors.New("recursive archive of the store root is refused; name a directory under it")
)

// ArchiveUnder archives every live document under s.Dir, bounded, then drops
// the hub bullets the sweep orphaned. The error is a refusal before any
// write; a document that failed is in Swept.Docs.
func ArchiveUnder(ctx context.Context, docs hub.Docs, s *Sweep) (Swept, error) {
	switch {
	case !strings.HasSuffix(s.Dir, "/"):
		return Swept{}, ErrNotADirectory
	case s.Dir == "/":
		return Swept{}, ErrStoreRoot
	}
	paths, err := liveUnder(ctx, s)
	if err != nil {
		return Swept{}, fmt.Errorf("archive failed: %w", err)
	}
	if len(paths) == 0 {
		return Swept{}, fmt.Errorf("archive failed: no live documents under %s", s.Dir)
	}
	if s.ExpectedDocs > 0 && len(paths) != s.ExpectedDocs {
		return Swept{}, fmt.Errorf("archive refused: %d live documents under %s, expected %d; the directory changed since it was listed, list and gate again", len(paths), s.Dir, s.ExpectedDocs)
	}
	var out Swept
	var archived []string
	for _, p := range paths {
		result, err := docs(p).Archive(ctx)
		doc := SweptDoc{Path: p, Result: result, Err: err}
		out.Docs = append(out.Docs, doc)
		if err == nil && Landed(&result) {
			out.Archived = append(out.Archived, doc)
			archived = append(archived, p)
		}
	}
	if len(archived) == 0 {
		return out, nil
	}
	// A bullet linking the directory goes only when nothing live is left
	// under it; a partial sweep unlinks each archived path from its own hubs.
	hubs, targets := hub.AncestorsOf(archived...), archived
	if out.Complete = len(archived) == len(paths); out.Complete {
		hubs, targets = hub.Ancestors(s.Dir), []string{s.Dir}
	}
	out.Hubs = hub.DropFromHubs(ctx, docs, hubs, &hub.Drop{Targets: targets, Stamp: s.Stamp})
	return out, nil
}

// liveUnder lists every live document under the directory, bounded.
func liveUnder(ctx context.Context, s *Sweep) ([]string, error) {
	var paths []string
	w := s.Walker
	w.MaxLists = archiveListCalls
	err := w.Walk(ctx, s.Dir, func(docPath string) error {
		if len(paths) >= archiveDocs {
			return fmt.Errorf("over %d documents under %s; archive a narrower directory", archiveDocs, s.Dir)
		}
		paths = append(paths, docPath)
		return nil
	})
	return paths, err
}
