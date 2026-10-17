package graphstore

import (
	"context"
	"fmt"
	"time"

	"github.com/latebit-io/demarkus/client/generation"
	"github.com/latebit-io/demarkus/protocol"
)

// SnapshotPublishOptions describes one complete graph generation.
type SnapshotPublishOptions struct {
	ManifestPath            string
	Exported                time.Time
	Nodes                   []StoredNode
	Edges                   []StoredEdge
	ExpectedManifestVersion *int
	ShardTargetBytes        int
}

// SnapshotPublishResult describes the committed generation.
type SnapshotPublishResult struct {
	Manifest        SnapshotManifest
	ManifestVersion int
	ShardsPublished int
	ShardsReused    int
}

// SnapshotFetch fetches one generated graph document.
type SnapshotFetch func(ctx context.Context, path string) (protocol.Response, error)

// SnapshotPublish publishes one generated graph document with CAS.
type SnapshotPublish func(ctx context.Context, path, body string, expectedVersion int) (protocol.Response, error)

// PublishSnapshot stages verified shards and commits the manifest last.
func PublishSnapshot(ctx context.Context, opts SnapshotPublishOptions, fetchDocument SnapshotFetch, publishDocument SnapshotPublish) (SnapshotPublishResult, error) { //nolint:gocritic // immutable options isolate active publication
	published, err := generation.Publish(ctx, generation.Spec[SnapshotShardRef]{
		Labels:                  generation.Labels{Manifest: "snapshot manifest", Shard: "graph shard"},
		ManifestPath:            opts.ManifestPath,
		ExpectedManifestVersion: opts.ExpectedManifestVersion,
		ActiveSlot: func(body string) (string, error) {
			manifest, err := ParseSnapshotManifest(opts.ManifestPath, body)
			return manifest.ActiveSlot, err
		},
		Shards: func(slot string) ([]generation.Shard[SnapshotShardRef], error) {
			artifacts, err := BuildSnapshotShards(opts.ManifestPath, slot, opts.Nodes, opts.Edges, opts.ShardTargetBytes)
			if err != nil {
				return nil, err
			}
			shards := make([]generation.Shard[SnapshotShardRef], len(artifacts))
			for i := range artifacts {
				shards[i] = generation.Shard[SnapshotShardRef]{
					Artifact: generation.Artifact{Path: artifacts[i].Path, Body: artifacts[i].Body, ContentHash: artifacts[i].ContentHash},
					Ref:      artifacts[i].Ref,
				}
			}
			return shards, nil
		},
		VerifyShard: func(ref SnapshotShardRef, resp protocol.Response) error {
			_, _, err := verifySnapshotShard(ref, resp)
			return err
		},
		Manifest: func(slot string, refs []SnapshotShardRef) (string, error) {
			return BuildSnapshotManifest(opts.ManifestPath, SnapshotManifest{
				Exported: opts.Exported.UTC(), Complete: true, Nodes: len(opts.Nodes), Edges: len(opts.Edges),
				ActiveSlot: slot, Shards: refs,
			})
		},
	}, generation.IO{Fetch: fetchDocument, Publish: publishDocument})
	if err != nil {
		return SnapshotPublishResult{}, err
	}
	parsed, err := ParseSnapshotManifest(opts.ManifestPath, published.Manifest.Body)
	if err != nil {
		return SnapshotPublishResult{}, fmt.Errorf("verify snapshot manifest: %w", err)
	}
	return SnapshotPublishResult{
		Manifest:        parsed,
		ManifestVersion: published.ManifestVersion,
		ShardsPublished: published.ShardsPublished,
		ShardsReused:    published.ShardsReused,
	}, nil
}
