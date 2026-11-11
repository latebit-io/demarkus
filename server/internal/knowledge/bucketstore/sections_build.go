package bucketstore

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/google/btree"
	"github.com/latebit-io/demarkus/server/blob"
	"github.com/latebit-io/demarkus/server/internal/backend"
	"github.com/latebit-io/demarkus/server/internal/catalog"
)

// bodyKey names one body of one path.
type bodyKey struct{ path, hash string }

// readStepSections indexes every body the steps introduce, once each and in
// parallel, against the bodies current already holds.
func (store *Store) readStepSections(ctx context.Context, current *sectionVersion, steps []sectionStep) (map[bodyKey]*catalog.DocSections, error) {
	latest := make(map[string]string)
	var needs []*pathState
	for _, step := range steps {
		for _, state := range step.changed {
			if state.Archived {
				latest[state.Path] = ""
				continue
			}
			previous, seen := latest[state.Path]
			if !seen {
				if doc, ok := current.docs.Get(docProbe(state.Path)); ok {
					previous = doc.bodyHash
				}
			}
			latest[state.Path] = state.BodyHash
			// A body that keeps no index is not read.
			if previous != state.BodyHash && state.Entry.Indexed() {
				needs = append(needs, state)
			}
		}
	}
	reader := store.bodyReader()
	var mu sync.Mutex
	read := make(map[bodyKey]*catalog.DocSections, len(needs))
	err := runParallel(ctx, store.shardWorkers, needs, func(ctx context.Context, state *pathState) error {
		body, err := reader.read(ctx, state)
		if err != nil {
			return err
		}
		var sections *catalog.DocSections
		if body != nil {
			sections = catalog.IndexSections(body)
		}
		mu.Lock()
		read[bodyKey{state.Path, state.BodyHash}] = sections
		mu.Unlock()
		return nil
	})
	return read, err
}

// apply is the version after step, with the sections read for it.
func (version *sectionVersion) apply(step sectionStep, read map[bodyKey]*catalog.DocSections) *sectionVersion {
	docs := version.docs.Clone()
	for _, state := range step.changed {
		if state.Archived {
			docs.Delete(docProbe(state.Path))
			continue
		}
		doc := &docSections{path: state.Path, bodyHash: state.BodyHash, entry: state.Entry}
		if old, ok := docs.Get(docProbe(state.Path)); ok && old.bodyHash == state.BodyHash {
			doc.sections = old.sections
		} else {
			doc.sections = read[bodyKey{state.Path, state.BodyHash}]
		}
		docs.ReplaceOrInsert(doc)
	}
	return &sectionVersion{sequence: step.sequence, docs: docs}
}

// resyncSections is current brought to target by comparing every document,
// for when installs came faster than the queue holds.
func (store *Store) resyncSections(ctx context.Context, current *sectionVersion, target *snapshot) (*sectionVersion, error) {
	step := sectionStep{sequence: target.Sequence}
	target.Paths.Ascend(func(state *pathState) bool {
		doc, ok := current.docs.Get(docProbe(state.Path))
		if state.Archived == ok || ok && (doc.bodyHash != state.BodyHash || doc.entry != state.Entry) {
			step.changed = append(step.changed, state)
		}
		return true
	})
	read, err := store.readStepSections(ctx, current, []sectionStep{step})
	if err != nil {
		return nil, err
	}
	return current.apply(step, read), nil
}

// bodyReader reads live documents' bodies for section work.
type bodyReader struct {
	objects blob.Store
	logger  *slog.Logger
	worldID string
}

func (store *Store) bodyReader() bodyReader {
	return bodyReader{objects: store.objects, logger: store.logger, worldID: store.worldID}
}

// read returns a live document's current body; nil when it is missing or
// corrupt, which is logged and skipped as ADR 0012 does. Any other failure
// is returned so the work is retried.
func (reader bodyReader) read(ctx context.Context, state *pathState) ([]byte, error) {
	view := &readView{objects: reader.objects}
	retained, err := view.currentRetained(ctx, state)
	var stored storedDocument
	if err == nil {
		_, stored, err = view.loadStored(ctx, state.Path, &retained)
	}
	if errors.Is(err, blob.ErrIntegrity) || errors.Is(err, backend.ErrNotFound) {
		reader.logger.Error("section index skipped a document whose body cannot be read", "world", reader.worldID, "path", state.Path, "error", err)
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read body of %s: %w", state.Path, err)
	}
	if stored.body == nil {
		return []byte{}, nil
	}
	return stored.body, nil
}

// shardGroup is a checkpoint shard's documents as a snapshot holds them.
type shardGroup struct {
	live []*pathState
}

// buildSections indexes snap's live documents, shard by shard of its
// checkpoint: bodies its segments hold, the rest from blobs.
func (store *Store) buildSections(ctx context.Context, snap *snapshot) (*btree.BTreeG[*docSections], error) {
	layout := snap.Checkpoint
	groups := make([]shardGroup, len(layout.Shards))
	snap.Paths.Ascend(func(state *pathState) bool {
		if !state.Archived {
			group := &groups[state.shard(layout.Bits)]
			group.live = append(group.live, state)
		}
		return true
	})
	listing, err := listSegments(ctx, store.objects)
	if err != nil {
		return nil, err
	}
	built := make([][]*docSections, len(groups))
	err = runParallel(ctx, store.shardWorkers, indexes(len(groups)), func(ctx context.Context, shard int) error {
		docs, err := store.buildShard(ctx, &listing, segmentLabel(shard, layout.Bits), &groups[shard])
		built[shard] = docs
		return err
	})
	if err != nil {
		return nil, err
	}
	tree := newDocTree()
	for _, docs := range built {
		for _, doc := range docs {
			tree.ReplaceOrInsert(doc)
		}
	}
	return tree, nil
}

// buildShard indexes one shard's live documents from its newest segment and
// the blobs of what that lacks. When the shard has no segment, it writes one.
func (store *Store) buildShard(ctx context.Context, listing *segmentListing, label string, group *shardGroup) ([]*docSections, error) {
	if len(group.live) == 0 {
		return nil, nil
	}
	reader := store.bodyReader()
	source, sourced := listing.latest(label)
	var carried map[string]string
	if sourced {
		var err error
		if carried, err = reader.segment(ctx, source); err != nil {
			return nil, err
		}
	}
	bodies, err := reader.shardBodies(ctx, group.live, carried)
	if err != nil {
		return nil, err
	}
	docs := make([]*docSections, len(group.live))
	for position, state := range group.live {
		docs[position] = &docSections{path: state.Path, bodyHash: state.BodyHash, entry: state.Entry}
		if body := bodies[position]; body != nil {
			docs[position].sections = catalog.IndexSectionsString(body.Body)
		}
	}
	if !sourced && !store.readOnly {
		repaired := segmentRef{label: label, window: windowOf(store.now())}
		if err := writeSegment(ctx, store.objects, repaired, readBodies(bodies)); err != nil && ctx.Err() == nil {
			store.logger.Warn("section segment not written; the next build reads its bodies again", "world", store.worldID, "segment", label, "error", err)
		}
	}
	return docs, nil
}
