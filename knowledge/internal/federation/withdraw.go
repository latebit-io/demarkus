package federation

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/latebit-io/demarkus/client/fetch"

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
			// The write that left the marker may not have pruned, and its
			// term may have ended before the export was: both are finished.
			if held, err := d.holdsOlder(ctx, path, version); err != nil {
				return err
			} else if held {
				if _, err := d.publishPruned(ctx, path, head.Body, version); err != nil {
					return err
				}
			}
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
	if err != nil {
		return version, err
	}
	held, err := d.holdsOlder(ctx, path, version)
	if err != nil {
		return version, fmt.Errorf("confirm prune of %s: %w", path, err)
	}
	if held {
		return version, fmt.Errorf("%s below v%d: %w", path, version, errNotPruned)
	}
	return version, nil
}

// holdsOlder reports whether the hub still keeps a version of path older
// than version. The count sees every one; without it only the version just
// before is asked for, and a store that skipped an older one goes unnoticed.
func (d *deriver) holdsOlder(ctx context.Context, path string, version int) (bool, error) {
	if d.CountVersions != nil {
		kept, err := d.CountVersions(ctx, path)
		return kept > 1, err
	}
	if version < 2 {
		return false, nil
	}
	older, err := d.Hub.Fetch(ctx, protocol.VersionPath(path, version-1))
	return older.Status != protocol.StatusNotFound, err
}

// VersionLister is what HubVersions asks: the hub's VERSIONS.
type VersionLister interface {
	Versions(ctx context.Context, r fetch.VersionsRequest) (fetch.Result, error)
}

// HubVersions is Config.CountVersions over hub as v reaches it: the total a
// VERSIONS answer reports.
func HubVersions(v VersionLister, hub string) func(ctx context.Context, path string) (int, error) {
	return func(ctx context.Context, path string) (int, error) {
		res, err := v.Versions(ctx, fetch.VersionsRequest{Host: hub, Path: path})
		if err != nil {
			return 0, err
		}
		if res.Response.Status != protocol.StatusOK {
			return 0, fmt.Errorf("versions of %s returned %s", path, res.Response.Status)
		}
		total, err := strconv.Atoi(res.Response.Metadata["total"])
		if err != nil {
			return 0, fmt.Errorf("versions of %s: total %q: %w", path, res.Response.Metadata["total"], err)
		}
		return total, nil
	}
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
