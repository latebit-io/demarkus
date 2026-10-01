package federation

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"time"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/generation"
	"github.com/latebit-io/demarkus/client/graphstore"
	"github.com/latebit-io/demarkus/client/listwalk"
	"github.com/latebit-io/demarkus/protocol"
)

// world is one world's derived rows and what the hub holds of them. One
// loop owns it, and it dies with that loop.
type world struct {
	*deriver
	name string
	log  *slog.Logger

	prefixLength int
	shards       map[string]map[string]graphstore.WorldSource // prefix, then path
	sources      int
	// refs pins every shard the hub holds for the world: the live
	// manifest's, then each one written since.
	refs  map[string]graphstore.WorldShardRef
	dirty map[string]struct{}
	// retry holds paths whose read failed for now.
	retry map[string]struct{}
	// partial marks rows known incomplete until the next rebuild: a cap was
	// reached or the walk could not list everything.
	partial bool

	manifestVersion  int // -1: unknown, read before the next write
	manifestComplete bool
	// changed is the last change; dirtied the first one the live manifest
	// lacks, zero when it has them all; attempted the last checkpoint.
	changed, dirtied, attempted time.Time
}

func newWorld(d *deriver, name string) *world {
	return &world{
		deriver:      d,
		name:         name,
		log:          d.Log.With("world", name),
		prefixLength: 1,
		shards:       map[string]map[string]graphstore.WorldSource{},
		refs:         map[string]graphstore.WorldShardRef{},
		dirty:        map[string]struct{}{},
		retry:        map[string]struct{}{},
	}
}

// run resumes the world from its checkpoint, or rebuilds it when there is
// none to resume, then follows its feed until ctx ends or the watch fails.
func (w *world) run(ctx context.Context) error {
	since, err := w.load(ctx)
	if err != nil {
		return err
	}
	// Superseded changes are dropped while the watch lags: the guard reads
	// the head anyway.
	watch, err := w.Source.Watch(ctx, fetch.WatchRequest{Host: w.name, Path: "/", Since: since, Coalesce: true})
	if err != nil {
		return fmt.Errorf("watch %s: %w", w.name, err)
	}
	defer watch.Close()
	// Opened before the walk, the watch carries every change made during it.
	if since.IsZero() {
		if err := w.rebuild(ctx); err != nil {
			return err
		}
	}
	return w.follow(ctx, watch)
}

// load adopts the world's checkpoint and returns the cursor to resume from,
// zero to rebuild. An incomplete checkpoint lends only its pins, to be
// rewritten by the rebuild; one that does not verify lends nothing.
func (w *world) load(ctx context.Context) (protocol.Cursor, error) {
	head, err := w.manifestHead(ctx)
	if err != nil || head.Status == protocol.StatusNotFound {
		return protocol.Cursor{}, err
	}
	load, err := graphstore.LoadWorld(ctx, graphstore.WorldLoadRequest{World: w.name, Manifest: head, Fetch: func(ctx context.Context, shard string) (protocol.Response, error) {
		if err := w.reads.Wait(ctx); err != nil {
			return protocol.Response{}, err
		}
		return w.Hub.Fetch(ctx, shard)
	}})
	switch {
	case errors.Is(err, graphstore.ErrWorldUnavailable):
		return protocol.Cursor{}, err
	case err != nil:
		w.log.Warn("federation: checkpoint does not verify, rebuilding", "err", err)
		return protocol.Cursor{}, nil
	}
	m := &load.Manifest
	w.prefixLength, w.manifestComplete = m.PrefixLength, m.Complete
	for _, ref := range m.Shards {
		w.refs[ref.Prefix] = ref
	}
	if !m.Complete {
		w.log.Info("federation: checkpoint incomplete, rebuilding")
		return protocol.Cursor{}, nil
	}
	for i := range load.Sources {
		w.place(graphstore.SourcePrefix(load.Sources[i].Path, w.prefixLength), load.Sources[i])
	}
	return m.Cursor, nil
}

// rebuild reads the whole world again, over rows dropped first. Every
// shard is rewritten: pinned ones without rows empty.
func (w *world) rebuild(ctx context.Context) error {
	w.log.Info("federation: rebuilding world")
	clear(w.shards)
	clear(w.retry)
	w.sources, w.partial = 0, false
	walker := listwalk.Walker{Client: w.Source, Host: w.name, OnProblem: w.walkProblem, BeforeList: w.reads.Wait}
	err := walker.Walk(ctx, "/", func(docPath string) error {
		if graphstore.IsGeneratedGraphPath(docPath) {
			return nil
		}
		return w.read(ctx, docPath)
	})
	switch {
	case errors.Is(err, listwalk.ErrListBudget):
		w.markPartial("list budget exhausted", "")
	case err != nil:
		return fmt.Errorf("rebuild %s: %w", w.name, err)
	}
	w.touchAll() // the manifest is owed even with no shard to write
	return nil
}

// walkProblem fails a rebuild that cannot list the root; anything below it
// leaves the rows partial.
func (w *world) walkProblem(p *listwalk.Problem) error {
	if p.Dir == "/" && p.Kind != listwalk.ProblemInvalidEntry {
		return p
	}
	w.markPartial(p.Error(), p.Dir)
	return nil
}

// follow applies the feed, and checkpoints when one is due, until ctx ends
// or the watch fails.
func (w *world) follow(ctx context.Context, watch *fetch.Watch) error {
	for {
		at, due := w.due()
		if due && !time.Now().Before(at) {
			if err := w.checkpoint(ctx, watch.Cursor()); err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				w.log.Warn("federation: checkpoint failed", "err", err)
			}
			continue
		}
		wait, cancel := ctx, context.CancelFunc(func() {})
		if due {
			wait, cancel = context.WithDeadline(ctx, at)
		}
		notice, err := watch.Next(wait)
		cancel()
		switch {
		case err == nil && notice.Resync:
			err = w.rebuild(ctx)
		case err == nil:
			err = w.apply(ctx, &notice.Event)
		case ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded):
			err = nil
		case ctx.Err() == nil:
			err = fmt.Errorf("watch %s: %w", w.name, err)
		}
		if err != nil {
			return err
		}
	}
}

// due is when the next checkpoint may run, and whether one waits: after a
// quiet period, or at the latest an interval after the first change, but
// never sooner than an interval after the last attempt.
func (w *world) due() (time.Time, bool) {
	if !w.owed() && len(w.retry) == 0 {
		return time.Time{}, false
	}
	at := w.changed.Add(w.QuietPeriod)
	if bound := w.dirtied.Add(w.Interval); !w.dirtied.IsZero() && bound.Before(at) {
		at = bound
	}
	if next := w.attempted.Add(w.Interval); next.After(at) {
		at = next
	}
	return at, true
}

// owed is whether the live manifest lags the rows.
func (w *world) owed() bool {
	return !w.dirtied.IsZero() || w.complete() != w.manifestComplete
}

// apply folds one change into the rows. The version guards replay: a change
// at or below a row's version is already in it.
func (w *world) apply(ctx context.Context, ev *protocol.WatchEvent) error {
	if graphstore.IsGeneratedGraphPath(ev.Path) {
		return nil
	}
	prefix := graphstore.SourcePrefix(ev.Path, w.prefixLength)
	row, exists := w.shards[prefix][ev.Path]
	if ev.Op == protocol.OpArchive {
		// Archiving keeps the version, so an archive at the row's own
		// version is news.
		if exists && row.Version <= ev.Version {
			w.remove(prefix, ev.Path)
		}
		return nil
	}
	if exists && ev.Version <= row.Version {
		return nil
	}
	return w.read(ctx, ev.Path)
}

// read derives path's row from its current document. A document gone or
// refused removes the row; any other failure leaves the path to retry.
func (w *world) read(ctx context.Context, path string) error {
	if err := w.reads.Wait(ctx); err != nil {
		return err
	}
	res, err := w.Source.Fetch(ctx, fetch.FetchRequest{Host: w.name, Path: path})
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		w.later(path, err.Error())
		return nil
	}
	prefix := graphstore.SourcePrefix(path, w.prefixLength)
	switch res.Response.Status {
	case protocol.StatusOK:
		source, rejected := graphstore.DeriveSource(w.name, path, &res.Response)
		if source.Version < 1 {
			w.later(path, "no version")
			return nil
		}
		for _, rel := range rejected {
			w.log.Debug("federation: relation metadata skipped", "path", path, "key", rel.Key, "reason", rel.Reason)
		}
		w.upsert(prefix, source)
	case protocol.StatusNotFound, protocol.StatusArchived, protocol.StatusUnauthorized, protocol.StatusNotPermitted:
		w.remove(prefix, path)
	default:
		w.later(path, res.Response.Status)
		return nil
	}
	delete(w.retry, path)
	return nil
}

// later keeps path to read again at the next checkpoint.
func (w *world) later(path, reason string) {
	if _, ok := w.retry[path]; !ok && len(w.retry) >= maxSources {
		w.markPartial("too many reads to retry", path)
		return
	}
	w.retry[path] = struct{}{}
	w.log.Warn("federation: read failed, will retry", "path", path, "reason", reason)
}

// upsert replaces source's row, marking its shard dirty only if it changed.
func (w *world) upsert(prefix string, source graphstore.WorldSource) { //nolint:gocritic // stored by value
	if len(source.Edges) > maxSourceEdges {
		source.Edges = slices.Clone(source.Edges[:maxSourceEdges])
		w.markPartial("source edges capped", source.Path)
	}
	row, exists := w.shards[prefix][source.Path]
	switch {
	case exists && (source.Version < row.Version || sameSource(&row, &source)):
		return
	case !exists && w.sources >= maxSources:
		w.markPartial("source cap reached", source.Path)
		return
	}
	w.place(prefix, source)
	w.touch(prefix)
}

// place stores source under prefix without marking anything changed.
func (w *world) place(prefix string, source graphstore.WorldSource) { //nolint:gocritic // stored by value
	shard := w.shards[prefix]
	if shard == nil {
		shard = map[string]graphstore.WorldSource{}
		w.shards[prefix] = shard
	}
	if _, exists := shard[source.Path]; !exists {
		w.sources++
	}
	shard[source.Path] = source
}

func (w *world) remove(prefix, path string) {
	shard := w.shards[prefix]
	if _, ok := shard[path]; !ok {
		return
	}
	delete(shard, path)
	w.sources--
	if len(shard) == 0 {
		delete(w.shards, prefix)
	}
	w.touch(prefix)
}

func (w *world) touch(prefix string) {
	w.dirty[prefix] = struct{}{}
	w.changedNow()
}

// touchAll marks every shard the hub holds or the rows fill dirty.
func (w *world) touchAll() {
	for prefix := range w.refs {
		w.dirty[prefix] = struct{}{}
	}
	for prefix := range w.shards {
		w.dirty[prefix] = struct{}{}
	}
	w.changedNow()
}

func (w *world) changedNow() {
	w.changed = time.Now()
	if w.dirtied.IsZero() {
		w.dirtied = w.changed
	}
}

// markPartial records rows known incomplete, logged once until a rebuild.
func (w *world) markPartial(reason, path string) {
	if !w.partial {
		w.log.Warn("federation: world rows incomplete until the next rebuild", "reason", reason, "path", path)
	}
	w.partial = true
}

func (w *world) complete() bool { return !w.partial && len(w.retry) == 0 }

// checkpoint retries failed reads, writes every dirty shard, then the
// manifest if it lags. A failure leaves what is unwritten for the next one.
func (w *world) checkpoint(ctx context.Context, cursor protocol.Cursor) error {
	w.attempted = time.Now()
	if len(w.retry) > 0 {
		for path := range w.retry {
			if err := w.read(ctx, path); err != nil {
				return err
			}
		}
		if len(w.retry) == 0 {
			w.retry = map[string]struct{}{} // a map never shrinks; drop a burst's buckets
		}
	}
	if err := w.writeDirty(ctx); err != nil {
		return err
	}
	if !w.owed() {
		return nil
	}
	return w.commit(ctx, cursor)
}

// writeDirty writes the dirty shards one body at a time, starting over when
// a shard of several rows outgrows its target and the world splits.
func (w *world) writeDirty(ctx context.Context) error {
	for _, prefix := range slices.Sorted(maps.Keys(w.dirty)) {
		rows := w.shards[prefix]
		shard, err := graphstore.BuildWorldShard(w.name, prefix, slices.Collect(maps.Values(rows)))
		oversized := errors.Is(err, graphstore.ErrWorldShardTooLarge) || err == nil && shard.Bytes > shardTarget
		if oversized && len(rows) > 1 && w.prefixLength < graphstore.MaxWorldPrefixLength {
			w.split()
			return w.writeDirty(ctx)
		}
		if err != nil {
			return fmt.Errorf("checkpoint %s: %w", w.name, err)
		}
		if err := w.writeShard(ctx, &shard); err != nil {
			return err
		}
	}
	return nil
}

// writeShard publishes shard unless the hub holds it already, or it is empty
// and was never pinned. Unconditional: only a manifest pin makes it count.
func (w *world) writeShard(ctx context.Context, shard *graphstore.WorldShard) error {
	ref, pinned := w.refs[shard.Prefix]
	if pinned && ref.ContentHash != shard.ContentHash || !pinned && shard.Sources > 0 {
		_, version, err := w.Hub.PublishDocument(ctx, shard.Path, shard.Body, -1)
		if err != nil {
			return fmt.Errorf("checkpoint %s: %w", w.name, err)
		}
		if shard.Sources == 0 {
			delete(w.refs, shard.Prefix)
		} else {
			w.refs[shard.Prefix] = shard.Ref(version)
		}
	}
	delete(w.dirty, shard.Prefix)
	return nil
}

// split moves every row under prefixes one hex digit longer: every new shard
// is written, and every pinned old one rewritten empty.
func (w *world) split() {
	w.prefixLength++
	rows := w.shards
	w.shards = make(map[string]map[string]graphstore.WorldSource, len(rows)*16)
	w.sources = 0
	for _, shard := range rows {
		for _, row := range shard {
			w.place(graphstore.SourcePrefix(row.Path, w.prefixLength), row)
		}
	}
	w.dirty = map[string]struct{}{}
	w.touchAll()
	w.log.Info("federation: world shards split", "prefixLength", w.prefixLength, "sources", w.sources)
}

// commit writes the manifest over the version it expects to replace, so a
// write that raced another leader's fails and is retried over the new head.
func (w *world) commit(ctx context.Context, cursor protocol.Cursor) error {
	m := graphstore.WorldManifest{World: w.name, Cursor: cursor, Complete: w.complete(), PrefixLength: w.prefixLength}
	m.Shards = slices.Collect(maps.Values(w.refs))
	body, err := graphstore.BuildWorldManifest(m)
	if err != nil {
		return err
	}
	if w.manifestVersion < 0 {
		if _, err := w.manifestHead(ctx); err != nil {
			return err
		}
	}
	_, version, err := w.Hub.PublishDocument(ctx, graphstore.WorldManifestPath(w.name), body, w.manifestVersion)
	if err != nil {
		w.manifestVersion = -1
		return fmt.Errorf("checkpoint %s: %w", w.name, err)
	}
	w.manifestVersion, w.manifestComplete, w.dirtied = version, m.Complete, time.Time{}
	return nil
}

// manifestHead reads the world's live manifest, not-found when there is none,
// and takes its version as the one the next write replaces.
func (w *world) manifestHead(ctx context.Context) (protocol.Response, error) {
	path := graphstore.WorldManifestPath(w.name)
	head, err := w.Hub.Fetch(ctx, path)
	if err != nil {
		return protocol.Response{}, fmt.Errorf("fetch %s: %w", path, err)
	}
	switch head.Status {
	case protocol.StatusNotFound:
		w.manifestVersion = 0
		return head, nil
	case protocol.StatusOK:
	default:
		return protocol.Response{}, fmt.Errorf("fetch %s returned %s", path, head.Status)
	}
	if w.manifestVersion, err = generation.ResponseVersion(path, head); err != nil {
		return protocol.Response{}, err
	}
	return head, nil
}

// sameSource reports whether a and b render the same row.
func sameSource(a, b *graphstore.WorldSource) bool {
	return a.Version == b.Version && a.Etag == b.Etag && a.Title == b.Title && slices.Equal(a.Edges, b.Edges)
}
