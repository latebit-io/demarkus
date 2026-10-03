package bucketstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/latebit-io/demarkus/protocol/storefmt"
	"github.com/latebit-io/demarkus/server/blob"
)

// A segment is a cache of one shard's live bodies, keyed by shard label and
// time window, so a section build reads a few objects per shard; a body it
// lacks, or holds at an older hash, is read from its blob (ADR 0036).
const (
	segmentSchema = 1
	segmentPrefix = objectPrefix + "segments/"
	// segmentWindow is how often at most one shard's segment is written, so
	// a world written everywhere does not rewrite every body each checkpoint.
	segmentWindow = 10 * time.Minute
	// segmentPartLimit caps one part's encoded bodies, under the 4 MiB cap on
	// production objects; a body too large for a part is left out.
	segmentPartLimit = 2<<20 - 1<<10
	// maxSegmentParts bounds what part 0 may claim; a part holds one body or more.
	maxSegmentParts = 1 << 12
	// segmentReaders reads one shard's bodies in parallel under each worker.
	segmentReaders = 8
	// segmentBackfill bounds the bodies one checkpoint reads for unchanged
	// documents no earlier segment holds; the rest wait for a later window.
	segmentBackfill = 4096
)

// segmentObject is one part of a shard's segment; part 0 names how many
// there are and is written last.
type segmentObject struct {
	Schema int           `json:"schema"`
	Label  string        `json:"label"`
	Window int64         `json:"window"`
	Part   int           `json:"part"`
	Parts  int           `json:"parts"`
	Bodies []segmentBody `json:"bodies"`
}

type segmentBody struct {
	Path     string `json:"path"`
	BodyHash string `json:"body_hash"`
	Body     string `json:"body"`
}

// segmentRef names one segment: a shard label and a window.
type segmentRef struct {
	label  string
	window int64
}

// segmentLabel names a shard by its place in a layout, not its contents.
func segmentLabel(index, bits int) string {
	return fmt.Sprintf("%02d-%s", bits, shardLabel(index, bits))
}

func segmentKey(ref segmentRef, part int) string {
	return fmt.Sprintf("%s%s/%016x/%d.json", segmentPrefix, ref.label, ref.window, part)
}

// windowOf is the segment window a time falls in.
func windowOf(at time.Time) int64 { return at.Unix() / int64(segmentWindow/time.Second) }

// segmentPart is one listed part, by the segment it belongs to.
type segmentPart struct {
	blob.Attributes
	segmentRef
	part int
}

// segmentListing is every listed part and the newest window of each label.
type segmentListing struct {
	parts  []segmentPart
	newest map[string]int64
}

// listSegments lists every segment part; a key not shaped as one is left out.
func listSegments(ctx context.Context, objects blob.Store) (segmentListing, error) {
	listing := segmentListing{newest: make(map[string]int64)}
	for page, err := range attributePages(ctx, objects, segmentPrefix, "") {
		if err != nil {
			return segmentListing{}, err
		}
		for _, attributes := range page {
			part, ok := parseSegmentKey(attributes)
			if !ok {
				continue
			}
			listing.parts = append(listing.parts, part)
			if newest, seen := listing.newest[part.label]; part.part == 0 && (!seen || part.window > newest) {
				listing.newest[part.label] = part.window
			}
		}
	}
	return listing, nil
}

func parseSegmentKey(attributes blob.Attributes) (segmentPart, bool) {
	fields := strings.Split(strings.TrimPrefix(attributes.Key, segmentPrefix), "/")
	if len(fields) != 3 || !strings.HasSuffix(fields[2], ".json") {
		return segmentPart{}, false
	}
	window, windowErr := strconv.ParseInt(fields[1], 16, 64)
	part, partErr := strconv.Atoi(strings.TrimSuffix(fields[2], ".json"))
	if windowErr != nil || partErr != nil || part < 0 {
		return segmentPart{}, false
	}
	return segmentPart{Attributes: attributes, segmentRef: segmentRef{label: fields[0], window: window}, part: part}, true
}

// latest is the newest segment of label, if any.
func (listing segmentListing) latest(label string) (segmentRef, bool) {
	window, ok := listing.newest[label]
	return segmentRef{label: label, window: window}, ok
}

// writeSegment writes path-sorted bodies as ref's segment, in parts of at
// most segmentPartLimit encoded, part 0 last. A part already there is left
// as it is: a racer's serves as well, since every body is checked on read.
func writeSegment(ctx context.Context, objects blob.Store, ref segmentRef, bodies []segmentBody) error {
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
		data, err := marshalImmutable(segmentObject{Schema: segmentSchema, Label: ref.label, Window: ref.window, Part: part, Parts: len(parts), Bodies: partBodies})
		if err != nil {
			return err
		}
		models[part] = modelObject{Key: segmentKey(ref, part), Data: data}
	}
	create := func(ctx context.Context, part modelObject) error {
		if _, err := objects.Create(ctx, part.Key, part.Data); err != nil && !errors.Is(err, blob.ErrPrecondition) {
			return fmt.Errorf("create %q: %w", part.Key, err)
		}
		return nil
	}
	if err := runParallel(ctx, segmentReaders, models[1:], create); err != nil {
		return err
	}
	return create(ctx, models[0])
}

// readSegment returns the bodies ref's segment holds, by body hash; nil when
// it has none. A part missing behind part 0, or one a racer wrote for
// another split, only costs the blob reads of its bodies.
func readSegment(ctx context.Context, objects blob.Store, ref segmentRef) (map[string]string, error) {
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
		if missing(err) || err == nil && part.Parts != first.Parts {
			return nil
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

func readSegmentPart(ctx context.Context, objects blob.Store, ref segmentRef, part int) (segmentObject, error) {
	return getValidated(ctx, objects, segmentKey(ref, part), func(segment *segmentObject) error {
		return validateSegment(segment, ref, part)
	})
}

// validateSegment checks a part's shape and that every body hashes to the
// hash it is filed under, which is what a reader relies on.
func validateSegment(segment *segmentObject, ref segmentRef, part int) error {
	if segment.Schema != segmentSchema {
		return fmt.Errorf("schema is %d, want %d", segment.Schema, segmentSchema)
	}
	if segment.Label != ref.label || segment.Window != ref.window || segment.Part != part || segment.Parts <= part || segment.Parts > maxSegmentParts {
		return fmt.Errorf("names %s window %d part %d of %d, want %s window %d part %d", segment.Label, segment.Window, segment.Part, segment.Parts, ref.label, ref.window, part)
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
func (reader bodyReader) segment(ctx context.Context, ref segmentRef) (map[string]string, error) {
	bodies, err := readSegment(ctx, reader.objects, ref)
	if errors.Is(err, blob.ErrIntegrity) {
		reader.logger.Error("section segment unreadable; its bodies are read one by one", "world", reader.worldID, "segment", ref.label, "window", ref.window, "error", err)
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

// segmentRun writes, after a checkpoint, the segments of its rewritten
// shards that have none for this window yet, then sweeps superseded ones.
type segmentRun struct {
	reader       bodyReader
	workers      int
	window       int64
	bits         int
	previousBits int
	fence        dropFence
	budget       atomic.Int64
	deferred     atomic.Int64
}

// write writes and sweeps; a failure only costs readers blob reads, so it
// is logged.
func (run *segmentRun) write(ctx context.Context, groups [][]grouped, shards []int) {
	reader := run.reader
	listing, err := listSegments(ctx, reader.objects)
	if err != nil {
		reader.logger.Warn("section segments not written; readers read their bodies", "world", reader.worldID, "error", err)
		return
	}
	run.budget.Store(segmentBackfill)
	// Workers read the listing, so what they write is merged into it after.
	var mu sync.Mutex
	var written []string
	err = runParallel(ctx, run.workers, shards, func(ctx context.Context, index int) error {
		label := segmentLabel(index, run.bits)
		source, sourced := listing.latest(label)
		if sourced && source.window >= run.window {
			return nil
		}
		if !sourced && run.bits > run.previousBits {
			// A shard split from a parent carries the parent's bodies.
			source, sourced = listing.latest(segmentLabel(index>>(run.bits-run.previousBits), run.previousBits))
		}
		var from *segmentRef
		if sourced {
			from = &source
		}
		wrote, err := run.shard(ctx, groups[index], label, from)
		if err != nil && ctx.Err() == nil {
			reader.logger.Warn("section segment not written; readers read its bodies", "world", reader.worldID, "segment", label, "error", err)
		}
		if wrote {
			mu.Lock()
			written = append(written, label)
			mu.Unlock()
		}
		return ctx.Err()
	})
	if err != nil {
		reader.logger.Warn("section segments cut short", "world", reader.worldID, "error", err)
		return
	}
	for _, label := range written {
		listing.newest[label] = run.window
	}
	if deferred := run.deferred.Load(); deferred > 0 {
		reader.logger.Info("section segments deferred to a later window", "world", reader.worldID, "shards", deferred)
	}
	if err := run.sweep(ctx, listing); err != nil {
		reader.logger.Warn("superseded section segments not deleted; a later checkpoint retries", "world", reader.worldID, "error", err)
	}
}

// shard writes one segment: bodies the source holds, if any, carried by
// hash, the others read once. Unchanged bodies with nothing to carry them come
// out of the run's backfill budget, or the shard waits for a later window.
func (run *segmentRun) shard(ctx context.Context, group []grouped, label string, source *segmentRef) (bool, error) {
	var live []*pathState
	for _, document := range group {
		if !document.state.Archived {
			live = append(live, document.state)
		}
	}
	if len(live) == 0 {
		return false, nil
	}
	var carried map[string]string
	if source != nil {
		var err error
		if carried, err = run.reader.segment(ctx, *source); err != nil {
			return false, err
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
		return false, nil
	}
	bodies, err := run.reader.shardBodies(ctx, live, carried)
	if err != nil {
		return false, err
	}
	err = writeSegment(ctx, run.reader.objects, segmentRef{label: label, window: run.window}, readBodies(bodies))
	return err == nil, err
}

// sweep deletes, past the fence's grace, every part of a segment its label
// has a newer one for, and of every label of another layout.
func (run *segmentRun) sweep(ctx context.Context, listing segmentListing) error {
	layout := fmt.Sprintf("%02d-", run.bits)
	var superseded []blob.Attributes
	for _, part := range listing.parts {
		stale := !strings.HasPrefix(part.label, layout) || part.window < listing.newest[part.label]
		if stale && run.fence.expired(part.Modified) {
			superseded = append(superseded, part.Attributes)
		}
	}
	return runParallel(ctx, run.workers, superseded, func(ctx context.Context, attributes blob.Attributes) error {
		return deleteAt(ctx, run.reader.objects, attributes)
	})
}

// readBodies keeps the bodies that could be read.
func readBodies(bodies []*segmentBody) []segmentBody {
	var kept []segmentBody
	for _, body := range bodies {
		if body != nil {
			kept = append(kept, *body)
		}
	}
	return kept
}
