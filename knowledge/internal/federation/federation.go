// Package federation derives the federation graph of the worlds this process
// serves from their change feeds, and checkpoints it into the hub world one
// changed shard at a time (graphstore's world checkpoint format), rendering
// the hub's /graph.md from the checkpoints. Only a world whose feed cannot be
// resumed is read whole again.
package federation

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/generation"
	"github.com/latebit-io/demarkus/client/graphstore"
	"github.com/latebit-io/demarkus/client/listwalk"
	"github.com/latebit-io/demarkus/protocol"
	"golang.org/x/time/rate"
)

const (
	// maxSources caps one world's rows; past it the checkpoint is incomplete.
	maxSources = 50_000
	// maxSourceEdges caps one row's edges, so a source always fits a shard.
	maxSourceEdges = 1024
	// shardTarget is the shard body size past which a world splits its
	// shards by one more hex digit.
	shardTarget = 128 << 10
	// retention keeps a checkpoint document's newest versions; a reader whose
	// pinned shard was pruned rereads the manifest.
	retention = 5
	// readsPerSecond paces reads across worlds, so a rebuild or a load leaves
	// the in-process rate budget each world shares to tool calls.
	readsPerSecond = 20
)

// Grant is everything the deriver may write: its checkpoints in the hub and
// the export rendered from them.
var Grant = protocol.Grant{Label: "federation", Paths: []string{graphstore.WorldGraphRoot + "/**", graphstore.LegacyExportPath}}

// Source reads the derived worlds by name, anonymously: no token and no
// grant, so a document the public cannot read never enters the graph.
type Source interface {
	listwalk.Lister
	Watch(ctx context.Context, r fetch.WatchRequest) (*fetch.Watch, error)
	Fetch(ctx context.Context, r fetch.FetchRequest) (fetch.Result, error)
}

// Writer reaches the hub world by name.
type Writer interface {
	Fetch(ctx context.Context, r fetch.FetchRequest) (fetch.Result, error)
	Publish(ctx context.Context, r fetch.WriteRequest) (fetch.Result, error)
}

// HubIO reads hub's checkpoints anonymously, so they must be publicly
// readable, and writes them under Grant.
func HubIO(w Writer, hub string) generation.IO {
	return generation.IO{
		Fetch: func(ctx context.Context, path string) (protocol.Response, error) {
			res, err := w.Fetch(ctx, fetch.FetchRequest{Host: hub, Path: path})
			return res.Response, err
		},
		Publish: func(ctx context.Context, path, body string, expected int) (protocol.Response, error) {
			res, err := w.Publish(protocol.WithGrant(ctx, Grant), fetch.WriteRequest{
				Host: hub, Path: path, Body: body, ExpectedVersion: expected,
				Metadata: generation.ArtifactMetadata("demarkus-federation", retention),
			})
			return res.Response, err
		},
	}
}

// Config is one deriver: the worlds it derives, where it reads them and the
// hub it checkpoints them into.
type Config struct {
	Worlds []string
	Source Source
	Hub    generation.IO
	// QuietPeriod is how long a changed world rests before its checkpoint;
	// Interval the least time between two checkpoints of one world.
	QuietPeriod, Interval time.Duration
	Log                   *slog.Logger
}

// deriver is what every world's loop shares.
type deriver struct {
	Config
	reads  *rate.Limiter
	export *exporter
}

// Run derives every world until ctx ends. A world that fails starts over
// from its checkpoint after a backoff; state dies with ctx.
func Run(ctx context.Context, cfg Config) { //nolint:gocritic // a config is passed once per term
	newDeriver(cfg).run(ctx)
}

func newDeriver(cfg Config) *deriver { //nolint:gocritic // a config is passed once per term
	cfg.Log = cmp.Or(cfg.Log, slog.Default())
	d := &deriver{Config: cfg, reads: rate.NewLimiter(readsPerSecond, readsPerSecond)}
	d.export = newExporter(d)
	return d
}

func (d *deriver) run(ctx context.Context) {
	var wg sync.WaitGroup
	for _, name := range d.Worlds {
		wg.Go(func() { d.derive(ctx, name) })
	}
	wg.Go(func() { d.export.run(ctx) })
	wg.Wait()
}

// hubHead reads path's live document in the hub and its version: zero, with
// the not-found response, when there is none.
func (d *deriver) hubHead(ctx context.Context, path string) (protocol.Response, int, error) {
	head, err := d.Hub.Fetch(ctx, path)
	if err != nil {
		return protocol.Response{}, 0, fmt.Errorf("fetch %s: %w", path, err)
	}
	switch head.Status {
	case protocol.StatusNotFound:
		return head, 0, nil
	case protocol.StatusOK:
	default:
		return protocol.Response{}, 0, fmt.Errorf("fetch %s returned %s", path, head.Status)
	}
	version, err := generation.ResponseVersion(path, head)
	if err != nil {
		return protocol.Response{}, 0, err
	}
	return head, version, nil
}

// hubShard reads one pinned checkpoint shard, paced with the other reads.
func (d *deriver) hubShard(ctx context.Context, path string) (protocol.Response, error) {
	if err := d.reads.Wait(ctx); err != nil {
		return protocol.Response{}, err
	}
	return d.Hub.Fetch(ctx, path)
}

// derive runs one world's loop until ctx ends, starting it over on failure.
func (d *deriver) derive(ctx context.Context, name string) {
	const minBackoff, maxBackoff = time.Second, time.Minute
	backoff := minBackoff
	for {
		started := time.Now()
		w := newWorld(d, name)
		err := w.run(ctx)
		if ctx.Err() != nil {
			return
		}
		if time.Since(started) > maxBackoff {
			backoff = minBackoff
		}
		w.log.Warn("federation: world failed, starting over", "err", err, "retry", backoff)
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return
		}
		backoff = min(backoff*2, maxBackoff)
	}
}
