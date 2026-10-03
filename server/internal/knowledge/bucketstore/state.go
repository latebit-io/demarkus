package bucketstore

import (
	"iter"
	"strings"
	"time"

	"github.com/google/btree"
	"github.com/latebit-io/demarkus/server/internal/catalog"
)

const treeDegree = 32

// snapshot is one committed state of the world: the loaded checkpoint plus
// every slot applied since. A published snapshot is never written; a change
// derives a new one that shares every untouched tree node.
type snapshot struct {
	Sequence int64
	// Tip is the hash the next slot names as its predecessor.
	Tip string
	// Checkpoint is the one the documents' Base entries were last rebased on.
	Checkpoint *checkpointBase
	Paths      *btree.BTreeG[*pathState]
	Children   *btree.BTreeG[*dirChild]
	// Hashes holds the body hash of every live document, for LookupHash.
	Hashes *btree.BTreeG[hashEntry]
}

// pathState is one document in a snapshot; once published it is never written,
// and a change replaces it whole.
type pathState struct {
	Path    string
	Current int
	// First is the oldest retained version; 0 while only the checkpoint's
	// manifest knows it.
	First    int
	Archived bool
	BodyHash string
	Modified time.Time
	Entry    *catalog.Entry // prepared
	// Base is the document as a checkpoint holds it, nil when it arrived
	// after; Recent are the versions committed since, oldest first.
	Base   *baseEntry
	Recent []retainedVersion
}

// baseEntry is a document's checkpoint entry: a folded one names its history
// blocks, a schema 1 one the manifest that does, which must match it.
type baseEntry struct {
	History  []blockRef
	Manifest objectRef
	Current  int
	Archived bool
	BodyHash string
	Modified time.Time
}

// first is the oldest version a folded entry retains; 0 for schema 1.
func (base *baseEntry) first() int {
	if len(base.History) == 0 {
		return 0
	}
	return base.History[0].First
}

// unchanged reports whether a document is as its folded checkpoint entry
// holds it, so a compactor writes that entry again as it is.
func (state *pathState) unchanged() bool {
	base := state.Base
	return base != nil && base.History != nil && len(state.Recent) == 0 &&
		state.Archived == base.Archived && state.First == base.first()
}

// checkpointBase is a loaded checkpoint's layout, which the compactor reuses
// for shards nothing changed in.
type checkpointBase struct {
	Sequence int64
	Bits     int
	Shards   []shardRef
	Legacy   bool // schema 1: 256 shards whose entries name manifests
}

// dirChild is one name in a directory, with how many documents pass through
// it, how many it is visible for, and how many of those are live.
type dirChild struct {
	Parent, Name string
	IsDir        bool
	Docs         int
	Visible      int
	Live         int
}

type hashEntry struct{ Hash, Path string }

func newSnapshot() *snapshot {
	return &snapshot{
		Paths: btree.NewG(treeDegree, func(a, b *pathState) bool { return a.Path < b.Path }),
		Children: btree.NewG(treeDegree, func(a, b *dirChild) bool {
			return a.Parent < b.Parent || a.Parent == b.Parent && a.Name < b.Name
		}),
		Hashes: btree.NewG(treeDegree, func(a, b hashEntry) bool {
			return a.Hash < b.Hash || a.Hash == b.Hash && a.Path < b.Path
		}),
	}
}

// derive starts the next snapshot. btree.Clone must not run concurrently on
// one tree, so callers go through Store.derive.
func (s *snapshot) derive() *snapshot {
	return &snapshot{
		Sequence:   s.Sequence,
		Tip:        s.Tip,
		Checkpoint: s.Checkpoint,
		Paths:      s.Paths.Clone(),
		Children:   s.Children.Clone(),
		Hashes:     s.Hashes.Clone(),
	}
}

func (s *snapshot) path(path string) *pathState {
	state, _ := s.Paths.Get(&pathState{Path: path})
	return state
}

// isDirectory reports whether any document lies under path; "/" always does.
func (s *snapshot) isDirectory(path string) bool {
	if path == "/" {
		return true
	}
	found := false
	s.Children.AscendGreaterOrEqual(&dirChild{Parent: path}, func(child *dirChild) bool {
		found = child.Parent == path
		return false
	})
	return found
}

// children visits dir's names after the given one, in name order.
func (s *snapshot) children(dir, after string, visit func(*dirChild) bool) {
	s.Children.AscendGreaterOrEqual(&dirChild{Parent: dir, Name: after}, func(child *dirChild) bool {
		if child.Parent != dir {
			return false
		}
		if after != "" && child.Name == after {
			return true
		}
		return visit(child)
	})
}

// lookupHash returns the smallest live path whose body has hash.
func (s *snapshot) lookupHash(hash string) (string, bool) {
	path, found := "", false
	s.Hashes.AscendGreaterOrEqual(hashEntry{Hash: hash}, func(entry hashEntry) bool {
		path, found = entry.Path, entry.Hash == hash
		return false
	})
	return path, found
}

// Entries yields each live document under scope for a catalog search
// (catalog.Index); sections live in the section index, not the snapshot.
func (s *snapshot) Entries(scope string) iter.Seq2[*catalog.Entry, *catalog.DocSections] {
	return func(yield func(*catalog.Entry, *catalog.DocSections) bool) {
		ascendScope(s.Paths, scope, func(path string) *pathState { return &pathState{Path: path} }, func(state *pathState) bool {
			return state.Archived || yield(state.Entry, nil)
		})
	}
}

// ascendScope visits a path-ordered tree's items under scope: the scope
// itself, then the range below scope+"/"; probe makes a search key.
func ascendScope[T any](tree *btree.BTreeG[T], scope string, probe func(path string) T, visit func(T) bool) {
	if scope == "/" {
		tree.Ascend(visit)
		return
	}
	if item, ok := tree.Get(probe(scope)); ok && !visit(item) {
		return
	}
	// '0' follows '/', so the range is exactly the paths under scope+"/".
	tree.AscendRange(probe(scope+"/"), probe(scope+"0"), visit)
}

// put installs state over old (nil for a new path) in a derived snapshot,
// keeping directory counts and the hash index in step.
func (s *snapshot) put(old, state *pathState) {
	s.Paths.ReplaceOrInsert(state)
	switch {
	case old == nil:
		s.count(state.Path, func(child *dirChild, visible bool) {
			child.Docs++
			if visible {
				child.Visible++
				if !state.Archived {
					child.Live++
				}
			}
		})
	case old.Archived != state.Archived:
		delta := 1
		if state.Archived {
			delta = -1
		}
		s.count(state.Path, func(child *dirChild, visible bool) {
			if visible {
				child.Live += delta
			}
		})
	}
	if old != nil && !old.Archived {
		s.Hashes.Delete(hashEntry{Hash: old.BodyHash, Path: old.Path})
	}
	if !state.Archived {
		s.Hashes.ReplaceOrInsert(hashEntry{Hash: state.BodyHash, Path: state.Path})
	}
}

// count applies change to a copy of every directory name on path's way.
func (s *snapshot) count(path string, change func(child *dirChild, visible bool)) {
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	lastHidden := -1
	for index, name := range parts {
		if hiddenLogicalName(name) {
			lastHidden = index
		}
	}
	parent := "/"
	for index, name := range parts {
		next := dirChild{Parent: parent, Name: name, IsDir: index < len(parts)-1}
		if existing, ok := s.Children.Get(&next); ok {
			next = *existing
		}
		change(&next, index > lastHidden)
		s.Children.ReplaceOrInsert(&next)
		parent = joinPath(parent, name)
	}
}

func hiddenLogicalName(name string) bool {
	return strings.HasPrefix(name, ".") || name == "versions"
}

func joinPath(parent, child string) string {
	if parent == "/" {
		return parent + child
	}
	return parent + "/" + child
}
