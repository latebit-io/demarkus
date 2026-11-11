// Package federation derives the federation graph of the worlds this process
// serves from their change feeds, and checkpoints it into the hub world one
// changed shard at a time (graphstore's world checkpoint format), rendering
// the hub's /graph.md from the checkpoints. Only a world whose feed cannot be
// resumed is read whole again.
package federation

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/generation"
	"github.com/latebit-io/demarkus/client/listwalk"
	"github.com/latebit-io/demarkus/client/memory/graphstore"
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
	// gatePoll bounds how long a world goes without asking whether it may
	// still be derived: nothing tells the deriver that a gate moved.
	gatePoll = 15 * time.Second
	// readsPerSecond paces reads across worlds, so a rebuild or a load leaves
	// the in-process rate budget each world shares to tool calls.
	readsPerSecond = 20
)

// Grant is everything the deriver may write: its checkpoints in the hub and
// the export rendered from them.
var Grant = protocol.Grant{Label: "federation", Paths: []string{graphstore.WorldGraphRoot + "/**", graphstore.GraphExportPath}}

// Source reads the derived worlds by name with no token and no grant. In
// process that passes a gated world's gate, so it does not keep a private
// document out of the graph: Config.Refuse does, by gate.
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
// readable, and writes them under Grant. A write under pruning keeps no
// older version.
func HubIO(w Writer, hub string) generation.IO {
	return generation.IO{
		Fetch: func(ctx context.Context, path string) (protocol.Response, error) {
			res, err := w.Fetch(ctx, fetch.FetchRequest{Host: hub, Path: path})
			return res.Response, err
		},
		Publish: func(ctx context.Context, path, body string, expected int) (protocol.Response, error) {
			meta := generation.ArtifactMetadata("demarkus-federation", retention)
			if prunes(ctx) {
				// The stamp makes every pruning write a new version: an
				// unchanged publish would prune nothing, so no retry could.
				meta["retention"], meta[prunedAtKey] = "1", time.Now().UTC().Format(time.RFC3339Nano)
			}
			res, err := w.Publish(protocol.WithGrant(ctx, Grant), fetch.WriteRequest{
				Host: hub, Path: path, Body: body, ExpectedVersion: expected, Metadata: meta,
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
	// ListHub names the documents directly in a hub directory (HubLister),
	// so a withdrawal finds every shard a world left there. Nil withdraws
	// only the shards its manifest pins.
	ListHub func(ctx context.Context, dir string) ([]string, error)
	// QuietPeriod is how long a changed world rests before its checkpoint;
	// Interval the least time between two checkpoints of one world.
	QuietPeriod, Interval time.Duration
	Log                   *slog.Logger
	// Refuse says why a world must not be derived now, nil when it may: the
	// broker's GateCheck. Asked before every run and at every wake of its
	// loop, since a reload can gate a world or open the hub. Nil derives all.
	Refuse func(world string) error
}

// ErrGatedIntoOpenHub refuses a gated world whose graph an ungated hub
// would hand to anonymous readers.
var ErrGatedIntoOpenHub = errors.New("gated world would be published into an ungated hub; gate the hub or leave the world out")

// GateCheck is the Refuse of a deployment: a world that is gated while hub
// is not would have its paths, titles and links served anonymously.
func GateCheck(hub string, gated func(world string) bool) func(world string) error {
	return func(world string) error {
		if gated(world) && !gated(hub) {
			return fmt.Errorf("federation: world %q: %w (hub %q)", world, ErrGatedIntoOpenHub, hub)
		}
		return nil
	}
}

// deriver is what every world's loop shares.
type deriver struct {
	Config
	reads  *rate.Limiter
	export *exporter
	// gatePoll is how often a world asks again whether it may be derived.
	gatePoll time.Duration
}

// refused is Config.Refuse, nil without one.
func (d *deriver) refused(world string) error {
	if d.Refuse == nil {
		return nil
	}
	return d.Refuse(world)
}

// Run derives every world until ctx ends. A world that fails starts over
// from its checkpoint after a backoff; state dies with ctx.
func Run(ctx context.Context, cfg Config) { //nolint:gocritic // a config is passed once per term
	newDeriver(cfg).run(ctx)
}

func newDeriver(cfg Config) *deriver { //nolint:gocritic // a config is passed once per term
	cfg.Log = cmp.Or(cfg.Log, slog.Default())
	d := &deriver{Config: cfg, reads: rate.NewLimiter(readsPerSecond, readsPerSecond), gatePoll: gatePoll}
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
// A world that may not be derived is no failure: it is withdrawn and waited on.
func (d *deriver) derive(ctx context.Context, name string) {
	const minBackoff, maxBackoff = time.Second, time.Minute
	log := d.Log.With("world", name)
	backoff := minBackoff
	for {
		if reason := d.refused(name); reason != nil {
			if !d.stayOut(ctx, name, log, reason) {
				return
			}
			backoff = minBackoff
			continue
		}
		started := time.Now()
		err := newWorld(d, name).run(ctx)
		if ctx.Err() != nil {
			return
		}
		if d.refused(name) != nil {
			continue // the loop ended because the gate closed under it
		}
		if time.Since(started) > maxBackoff {
			backoff = minBackoff
		}
		log.Warn("federation: world failed, starting over", "err", err, "retry", backoff)
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

// stayOut withdraws a refused world from the hub, retried until it lands,
// then waits for the refusal to lift; false when ctx ended first.
func (d *deriver) stayOut(ctx context.Context, name string, log *slog.Logger, reason error) bool {
	log.Warn("federation: world refused, withdrawing what the hub holds of it", "reason", reason)
	withdrawn := false
	for d.refused(name) != nil {
		if !withdrawn {
			if err := d.withdraw(ctx, name); err != nil && ctx.Err() == nil {
				log.Error("federation: refused world not withdrawn from the hub", "err", err)
			} else {
				withdrawn = true
			}
		}
		select {
		case <-time.After(d.gatePoll):
		case <-ctx.Done():
			return false
		}
	}
	return true
}
