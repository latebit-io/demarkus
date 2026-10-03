package bucketstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/latebit-io/demarkus/protocol/storefmt"
	"github.com/latebit-io/demarkus/server/blob"
)

// A segment holds the bodies of one checkpoint shard's live documents, so a
// replica builds its section index from a few reads per shard instead of one
// per document. It is optional: a body it lacks is read from its blob.
const (
	segmentSchema = 1
	segmentPrefix = objectPrefix + "segments/"
	// segmentPartLimit caps one part's encoded bodies, under the 4 MiB cap on
	// production objects; a body too large for a part is left out.
	segmentPartLimit = 2<<20 - 1<<10
	// maxSegmentParts bounds what part 0 may claim; a part holds one body or more.
	maxSegmentParts = 1 << 12
	// segmentReaders reads one shard's bodies in parallel under each worker.
	segmentReaders = 8
	// segmentBackfill bounds the bodies one checkpoint reads for unchanged
	// documents its previous segments lack; the rest wait for a later rewrite
	// of their shard or a replica's repair.
	segmentBackfill = 4096
)

// segmentObject is one part of a shard's segment; part 0 names how many
// there are and is written last.
type segmentObject struct {
	Schema int           `json:"schema"`
	Shard  string        `json:"shard"`
	Part   int           `json:"part"`
	Parts  int           `json:"parts"`
	Bodies []segmentBody `json:"bodies"`
}

type segmentBody struct {
	Path     string `json:"path"`
	BodyHash string `json:"body_hash"`
	Body     string `json:"body"`
}

// segmentKey names one part of the segment of the shard object ref names.
func segmentKey(ref shardRef, part int) string {
	return fmt.Sprintf("%s%s/%s/%d.json", segmentPrefix, ref.Shard, ref.Hash, part)
}

// writeSegment writes path-sorted bodies as ref's segment, in parts of at
// most segmentPartLimit encoded, part 0 last. The split depends on the bodies
// alone, so every writer of one shard's segment writes the same bytes.
func writeSegment(ctx context.Context, objects blob.Store, ref shardRef, bodies []segmentBody) error {
	var parts [][]segmentBody
	size := 0
	for _, body := range bodies {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode segment body %s: %w", body.Path, err)
		}
		n := len(encoded) + 1
		if n > segmentPartLimit {
			continue
		}
		if len(parts) == 0 || size+n > segmentPartLimit {
			parts, size = append(parts, nil), 0
		}
		parts[len(parts)-1] = append(parts[len(parts)-1], body)
		size += n
	}
	if len(parts) == 0 {
		return nil
	}
	models := make([]modelObject, len(parts))
	for part, partBodies := range parts {
		data, err := marshalImmutable(segmentObject{Schema: segmentSchema, Shard: ref.Hash, Part: part, Parts: len(parts), Bodies: partBodies})
		if err != nil {
			return err
		}
		models[part] = modelObject{Key: segmentKey(ref, part), Data: data}
	}
	err := runParallel(ctx, segmentReaders, models[1:], func(ctx context.Context, part modelObject) error {
		return createImmutable(ctx, objects, part)
	})
	if err != nil {
		return err
	}
	return createImmutable(ctx, objects, models[0])
}

// readSegment returns the bodies ref's segment holds, by body hash; nil when
// it has none. A part missing behind part 0 only costs the blob reads of its
// bodies.
func readSegment(ctx context.Context, objects blob.Store, ref shardRef) (map[string]string, error) {
	first, err := readSegmentPart(ctx, objects, ref, 0)
	if missing(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	bodies := make(map[string]string)
	var mu sync.Mutex
	add := func(part *segmentObject) {
		mu.Lock()
		defer mu.Unlock()
		for _, body := range part.Bodies {
			bodies[body.BodyHash] = body.Body
		}
	}
	add(&first)
	err = runParallel(ctx, segmentReaders, indexes(first.Parts)[1:], func(ctx context.Context, index int) error {
		part, err := readSegmentPart(ctx, objects, ref, index)
		if missing(err) {
			return nil
		}
		if err == nil && part.Parts != first.Parts {
			err = fmt.Errorf("%w: segment %s part %d names %d parts, part 0 %d", blob.ErrIntegrity, ref.Hash, index, part.Parts, first.Parts)
		}
		if err != nil {
			return err
		}
		add(&part)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return bodies, nil
}

// missing reports a clear not-found, as opposed to a missing object another
// one references, which is an integrity failure.
func missing(err error) bool {
	return errors.Is(err, blob.ErrNotFound) && !errors.Is(err, blob.ErrIntegrity)
}

func readSegmentPart(ctx context.Context, objects blob.Store, ref shardRef, part int) (segmentObject, error) {
	segment, _, err := getValidated(ctx, objects, segmentKey(ref, part), func(segment *segmentObject) error {
		return validateSegment(segment, ref, part)
	})
	return segment, err
}

// validateSegment checks a part's shape and that every body hashes to the
// hash it is filed under, which is what a reader relies on.
func validateSegment(segment *segmentObject, ref shardRef, part int) error {
	if segment.Schema != segmentSchema {
		return fmt.Errorf("schema is %d, want %d", segment.Schema, segmentSchema)
	}
	if segment.Shard != ref.Hash || segment.Part != part || segment.Parts <= part || segment.Parts > maxSegmentParts {
		return fmt.Errorf("names shard %s part %d of %d, want shard %s part %d", segment.Shard, segment.Part, segment.Parts, ref.Hash, part)
	}
	if len(segment.Bodies) == 0 {
		return fmt.Errorf("holds no bodies")
	}
	for index := range segment.Bodies {
		body := &segment.Bodies[index]
		if index > 0 && segment.Bodies[index-1].Path >= body.Path {
			return fmt.Errorf("bodies %d and %d are not strictly path-sorted", index-1, index)
		}
		if err := validateDocumentPath(body.Path); err != nil {
			return fmt.Errorf("body %d: %w", index, err)
		}
		if storefmt.ContentHash([]byte(body.Body)) != body.BodyHash {
			return fmt.Errorf("body %d of %s does not hash to %s", index, body.Path, body.BodyHash)
		}
	}
	return nil
}

// segment reads ref's segment; one that fails its checks is logged and read
// around as if missing.
func (reader bodyReader) segment(ctx context.Context, ref shardRef) (map[string]string, error) {
	bodies, err := readSegment(ctx, reader.objects, ref)
	if errors.Is(err, blob.ErrIntegrity) {
		reader.logger.Error("section segment unreadable; its bodies are read one by one", "world", reader.worldID, "shard", ref.Hash, "error", err)
		return nil, nil
	}
	return bodies, err
}

// shardBodies returns the bodies of live documents, in their order: from
// carried by body hash, the rest read in parallel. An unreadable body is nil.
func (reader bodyReader) shardBodies(ctx context.Context, live []*pathState, carried map[string]string) ([]*segmentBody, error) {
	bodies := make([]*segmentBody, len(live))
	err := runParallel(ctx, segmentReaders, indexes(len(live)), func(ctx context.Context, position int) error {
		state := live[position]
		body, ok := carried[state.BodyHash]
		if !ok {
			read, err := reader.read(ctx, state)
			if err != nil || read == nil {
				return err
			}
			body = string(read)
		}
		bodies[position] = &segmentBody{Path: state.Path, BodyHash: state.BodyHash, Body: body}
		return nil
	})
	return bodies, err
}

// sameBody reports a document live with the body its checkpoint entry holds.
func sameBody(state *pathState) bool {
	return state.Base != nil && !state.Base.Archived && !state.Archived && state.Base.BodyHash == state.BodyHash
}

// segmentRun writes the segments of one checkpoint's rewritten shards, laid
// out in bits, on top of the previous checkpoint's.
type segmentRun struct {
	reader   bodyReader
	workers  int
	previous *checkpointBase
	bits     int
	budget   atomic.Int64
	deferred atomic.Int64
}

// write writes the segment of each rewritten shard with live documents. A
// failure only costs readers blob reads, so it is logged.
func (run *segmentRun) write(ctx context.Context, groups [][]grouped, shards []int, refs []shardRef) {
	reader := run.reader
	run.budget.Store(segmentBackfill)
	err := runParallel(ctx, run.workers, shards, func(ctx context.Context, index int) error {
		var source *shardRef
		if run.bits >= run.previous.Bits {
			source = &run.previous.Shards[index>>(run.bits-run.previous.Bits)]
		}
		if err := run.shard(ctx, groups[index], refs[index], source); err != nil && ctx.Err() == nil {
			reader.logger.Warn("section segment not written; readers read its bodies", "world", reader.worldID, "shard", refs[index].Shard, "error", err)
		}
		return ctx.Err()
	})
	if err != nil {
		reader.logger.Warn("section segments cut short", "world", reader.worldID, "error", err)
	}
	if deferred := run.deferred.Load(); deferred > 0 {
		reader.logger.Info("section segments deferred to a later checkpoint", "world", reader.worldID, "shards", deferred)
	}
}

// shard writes one segment: unchanged bodies carried from the segment of the
// shard they lay in, the others read once. Unchanged bodies with nothing to
// carry them come out of the run's backfill budget, or the shard waits.
func (run *segmentRun) shard(ctx context.Context, group []grouped, ref shardRef, source *shardRef) error {
	var live []*pathState
	unchanged := false
	for _, document := range group {
		if state := document.state; !state.Archived {
			live = append(live, state)
			unchanged = unchanged || sameBody(state)
		}
	}
	if len(live) == 0 {
		return nil
	}
	var carried map[string]string
	if unchanged && source != nil {
		var err error
		if carried, err = run.reader.segment(ctx, *source); err != nil {
			return err
		}
	}
	backfill := int64(0)
	for _, state := range live {
		if _, ok := carried[state.BodyHash]; !ok && sameBody(state) {
			backfill++
		}
	}
	if backfill > 0 && run.budget.Add(-backfill) < 0 {
		run.budget.Add(backfill)
		run.deferred.Add(1)
		return nil
	}
	bodies, err := run.reader.shardBodies(ctx, live, carried)
	if err != nil {
		return err
	}
	return writeSegment(ctx, run.reader.objects, ref, readBodies(bodies, nil))
}

// readBodies keeps the bodies that could be read, and of those only the ones
// keep accepts when it is set.
func readBodies(bodies []*segmentBody, keep func(position int) bool) []segmentBody {
	var kept []segmentBody
	for position, body := range bodies {
		if body != nil && (keep == nil || keep(position)) {
			kept = append(kept, *body)
		}
	}
	return kept
}

// dropSegments deletes every segment part whose shard no kept checkpoint uses
// and that is past the grace, at its listed generation. One sweep also takes
// parts a cut-short write or a late repair left behind.
func (store *Store) dropSegments(ctx context.Context, plan dropPlan) error {
	cursor := ""
	for {
		page, err := store.objects.List(ctx, segmentPrefix, "", cursor)
		if err != nil {
			return fmt.Errorf("list %q: %w", segmentPrefix, err)
		}
		var unused []blob.Attributes
		for _, attributes := range page.Objects {
			// A key not shaped as a segment part is left alone.
			label, rest, labeled := strings.Cut(strings.TrimPrefix(attributes.Key, segmentPrefix), "/")
			hash, _, hashed := strings.Cut(rest, "/")
			if labeled && hashed && !plan.kept[shardKey(label, hash)] && plan.started.Sub(attributes.Modified) >= dropGrace {
				unused = append(unused, attributes)
			}
		}
		err = runParallel(ctx, store.shardWorkers, unused, func(ctx context.Context, attributes blob.Attributes) error {
			return deleteAt(ctx, store.objects, attributes)
		})
		if err != nil {
			return err
		}
		if page.NextCursor == "" {
			return nil
		}
		cursor = page.NextCursor
	}
}
