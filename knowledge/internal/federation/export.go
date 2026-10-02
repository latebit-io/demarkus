package federation

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/latebit-io/demarkus/client/generation"
	"github.com/latebit-io/demarkus/client/graphstore"
	"github.com/latebit-io/demarkus/protocol"
)

// exporter renders the hub's /graph.md, the universe map the library reads,
// from every world's checkpoint in the hub: what readers verify, with no
// locks shared with the world loops. Its state dies with the term.
type exporter struct {
	*deriver
	log *slog.Logger

	mu      sync.Mutex
	pending map[string]struct{} // worlds whose checkpoint moved since their last load
	wake    chan struct{}

	worlds map[string]*exportedWorld // absent until the world has a checkpoint
	// owed is a render the live document may lack; liveVersion is that
	// document's version as last read or written, -1 unread this term.
	owed        bool
	liveVersion int
	liveContent string // generation.BodyHash of its graphstore.ExportContent
	oversized   bool
}

// exportedWorld is one world's checkpoint as last loaded.
type exportedWorld struct {
	manifest graphstore.WorldManifest
	shards   map[string][]graphstore.WorldSource // by prefix
}

func newExporter(d *deriver) *exporter {
	e := &exporter{
		deriver: d, log: d.Log.With("export", graphstore.LegacyExportPath),
		pending: make(map[string]struct{}, len(d.Worlds)), wake: make(chan struct{}, 1),
		worlds: make(map[string]*exportedWorld, len(d.Worlds)), liveVersion: -1,
	}
	e.pendAll()
	return e
}

// pendAll marks every world's checkpoint to load again.
func (e *exporter) pendAll() {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, name := range e.Worlds {
		e.pending[name] = struct{}{}
	}
}

// changed tells the exporter world's checkpoint moved; it never blocks.
func (e *exporter) changed(world string) {
	e.mu.Lock()
	e.pending[world] = struct{}{}
	e.mu.Unlock()
	select {
	case e.wake <- struct{}{}:
	default:
	}
}

// run exports when a checkpoint moved, at most once per interval, until ctx
// ends. Nothing is rendered until every world has a checkpoint, so a first
// boot never replaces the live map with part of it.
func (e *exporter) run(ctx context.Context) {
	var attempted time.Time
	for {
		if !e.due() {
			select {
			case <-e.wake:
				continue
			case <-ctx.Done():
				return
			}
		}
		select {
		case <-time.After(time.Until(attempted.Add(e.Interval))):
		case <-ctx.Done():
			return
		}
		attempted = time.Now()
		if err := e.export(ctx); err != nil && ctx.Err() == nil {
			e.log.Warn("federation: export failed", "err", err)
		}
	}
}

func (e *exporter) due() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.pending) > 0 || e.owed && len(e.worlds) == len(e.Worlds)
}

// export loads the moved checkpoints, then writes the render if the live
// document lacks it. A load or write that fails is retried at the next.
func (e *exporter) export(ctx context.Context) error {
	e.mu.Lock()
	moved := slices.Sorted(maps.Keys(e.pending))
	clear(e.pending)
	e.mu.Unlock()
	var errs []error
	for _, name := range moved {
		if err := e.load(ctx, name); err != nil {
			e.mu.Lock()
			e.pending[name] = struct{}{}
			e.mu.Unlock()
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 || len(e.worlds) < len(e.Worlds) || !e.owed {
		return errors.Join(errs...)
	}
	return e.write(ctx)
}

// load adopts world's live checkpoint, fetching only shards whose pins moved.
// A world without one is left out; its first checkpoint signals.
func (e *exporter) load(ctx context.Context, world string) error {
	if err := e.reads.Wait(ctx); err != nil {
		return err
	}
	head, _, err := e.hubHead(ctx, graphstore.WorldManifestPath(world))
	if err != nil || head.Status == protocol.StatusNotFound {
		return err
	}
	held := e.worlds[world]
	var previous *graphstore.WorldManifest
	if held != nil {
		previous = &held.manifest
	}
	load, err := graphstore.LoadWorld(ctx, graphstore.WorldLoadRequest{World: world, Manifest: head, Previous: previous, Fetch: e.hubShard})
	if err != nil {
		return err
	}
	shards := make(map[string][]graphstore.WorldSource, len(load.Manifest.Shards))
	for prefix := range load.Kept {
		shards[prefix] = held.shards[prefix]
	}
	for i := range load.Sources {
		prefix := graphstore.SourcePrefix(load.Sources[i].Path, load.Manifest.PrefixLength)
		shards[prefix] = append(shards[prefix], load.Sources[i])
	}
	e.worlds[world] = &exportedWorld{manifest: load.Manifest, shards: shards}
	e.owed = true
	return nil
}

// write publishes the render over the version last read or written, unless
// the live document holds the same graph.
func (e *exporter) write(ctx context.Context) error {
	rows := make(map[string][]graphstore.WorldSource, len(e.worlds))
	for name, w := range e.worlds {
		for _, sources := range w.shards {
			rows[name] = append(rows[name], sources...)
		}
	}
	body := graphstore.BuildWorldsExport(time.Now(), rows)
	if len(body) > protocol.MaxBodyLength {
		if !e.oversized {
			e.log.Warn("federation: graph export exceeds the body limit, not written", "bytes", len(body), "limit", protocol.MaxBodyLength)
		}
		e.oversized, e.owed = true, false
		return nil
	}
	e.oversized = false
	if e.liveVersion < 0 {
		if err := e.readLive(ctx); err != nil {
			return err
		}
	}
	content := generation.BodyHash(graphstore.ExportContent(body))
	if content == e.liveContent {
		e.owed = false
		return nil
	}
	_, version, err := e.Hub.PublishDocument(ctx, graphstore.LegacyExportPath, body, e.liveVersion)
	if errors.Is(err, generation.ErrConflict) {
		// Another term wrote it: render again from the hub's checkpoints,
		// never from the ones this term last saw.
		e.liveVersion = -1
		e.pendAll()
	}
	if err != nil {
		return fmt.Errorf("export: %w", err)
	}
	e.liveVersion, e.liveContent, e.owed = version, content, false
	return nil
}

// readLive takes the live document's version and content.
func (e *exporter) readLive(ctx context.Context) error {
	head, version, err := e.hubHead(ctx, graphstore.LegacyExportPath)
	if err != nil {
		return err
	}
	e.liveVersion, e.liveContent = version, ""
	if head.Status == protocol.StatusOK {
		e.liveContent = generation.BodyHash(graphstore.ExportContent(head.Body))
	}
	return nil
}
