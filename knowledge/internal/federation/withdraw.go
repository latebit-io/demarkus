package federation

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/latebit-io/demarkus/client/listwalk"
	"github.com/latebit-io/demarkus/client/memory/graphstore"
	"github.com/latebit-io/demarkus/protocol"
)

const (
	// withdrawnEpoch is the cursor epoch of a manifest a withdrawal wrote:
	// never resumed from (it is incomplete), and the sign it landed.
	withdrawnEpoch = "withdrawn"
	// prunedAtKey stamps a pruning write with its time.
	prunedAtKey = "pruned-at"
)

// errNotPruned is a pruning write whose older version is still readable.
var errNotPruned = errors.New("older version not pruned")

type pruningKey struct{}

// pruning marks the hub writes under ctx as leaving no older version: the
// rows a withdrawal removes would otherwise stay readable there.
func pruning(ctx context.Context) context.Context {
	return context.WithValue(ctx, pruningKey{}, true)
}

func prunes(ctx context.Context) bool {
	on, _ := ctx.Value(pruningKey{}).(bool)
	return on
}

// HubLister is Config.ListHub over hub as l reaches it; a directory that does
// not exist lists empty, and its subdirectories are left alone.
func HubLister(l listwalk.Lister, hub string) func(ctx context.Context, dir string) ([]string, error) {
	oneDirectory := func(p *listwalk.Problem) error {
		missing := p.Kind == listwalk.ProblemStatus && p.Status == protocol.StatusNotFound
		if missing || p.Kind == listwalk.ProblemDepth {
			return nil
		}
		return p
	}
	return func(ctx context.Context, dir string) ([]string, error) {
		var paths []string
		walker := listwalk.Walker{Client: l, Host: hub, MaxDepth: listwalk.RootOnly, OnProblem: oneDirectory}
		err := walker.Walk(ctx, dir, func(docPath string) error {
			paths = append(paths, docPath)
			return nil
		})
		return paths, err
	}
}

// withdraw empties what the hub holds of world: every shard document, then
// the manifest, then the export, each write pruning older versions. A
// manifest an earlier withdrawal left skips straight to the export.
func (d *deriver) withdraw(ctx context.Context, world string) error {
	path := graphstore.WorldManifestPath(world)
	head, version, err := d.hubHead(ctx, path)
	if err != nil {
		return err
	}
	manifest := graphstore.WorldManifest{World: world, Cursor: protocol.Cursor{Epoch: withdrawnEpoch}, PrefixLength: 1}
	var pinned []graphstore.WorldShardRef
	if m, err := graphstore.ParseWorldManifest(world, head.Body); head.Status == protocol.StatusOK && err == nil {
		if m.Cursor.Epoch == withdrawnEpoch && len(m.Shards) == 0 {
			// The term that wrote it may have ended before the export was
			// pruned: ask again, which costs one render.
			d.export.withdrawn(world)
			return nil
		}
		manifest.PrefixLength, pinned = m.PrefixLength, m.Shards
	}
	prefixes, err := d.shardPrefixes(ctx, world, pinned)
	if err != nil {
		return err
	}
	if head.Status == protocol.StatusNotFound && len(prefixes) == 0 {
		return nil
	}
	for _, prefix := range prefixes {
		shard, err := graphstore.BuildWorldShard(world, prefix, nil)
		if err != nil {
			return err
		}
		if _, err := d.publishPruned(ctx, shard.Path, shard.Body, -1); err != nil {
			return err
		}
	}
	body, err := graphstore.BuildWorldManifest(manifest)
	if err != nil {
		return err
	}
	if _, err := d.publishPruned(ctx, path, body, version); err != nil {
		return err
	}
	d.export.withdrawn(world)
	return nil
}

// publishPruned writes path keeping no older version, and confirms the one
// before it is gone: a store reports a failed prune only in its own log, and
// the write itself still succeeds.
func (d *deriver) publishPruned(ctx context.Context, path, body string, expected int) (int, error) {
	_, version, err := d.Hub.PublishDocument(pruning(ctx), path, body, expected)
	if err != nil || version < 2 {
		return version, err
	}
	older, err := d.Hub.Fetch(ctx, protocol.VersionPath(path, version-1))
	if err != nil {
		return version, fmt.Errorf("confirm prune of %s: %w", path, err)
	}
	if older.Status != protocol.StatusNotFound {
		return version, fmt.Errorf("%s v%d: %w", path, version-1, errNotPruned)
	}
	return version, nil
}

// shardPrefixes are the shards world has in the hub: every one listed there,
// since a shard is written before the manifest that pins it and outlives its
// pin, or with no lister the pinned ones.
func (d *deriver) shardPrefixes(ctx context.Context, world string, pinned []graphstore.WorldShardRef) ([]string, error) {
	if d.ListHub == nil {
		prefixes := make([]string, 0, len(pinned))
		for i := range pinned {
			prefixes = append(prefixes, pinned[i].Prefix)
		}
		return prefixes, nil
	}
	dir := graphstore.WorldGraphRoot + "/" + world
	paths, err := d.ListHub(ctx, dir)
	if err != nil {
		return nil, err
	}
	prefixes := make([]string, 0, len(paths))
	for _, p := range paths {
		// Anything else there, the manifest included, is not a shard.
		prefix, isDoc := strings.CutSuffix(strings.TrimPrefix(p, dir+"/"), ".md")
		if isDoc && graphstore.IsWorldPrefix(prefix) {
			prefixes = append(prefixes, prefix)
		}
	}
	return prefixes, nil
}
