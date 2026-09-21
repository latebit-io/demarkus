package index

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/latebit-io/demarkus/client/generation"
	"github.com/latebit-io/demarkus/protocol"
)

// PublishOptions describes one complete logical index generation.
type PublishOptions struct {
	ManifestPath            string
	Source                  string
	Indexed                 time.Time
	Entries                 []Entry
	ExpectedManifestVersion *int
	ShardTargetBytes        int
}

// PublishResult describes the committed generation and staged shard work.
type PublishResult struct {
	Manifest        Manifest
	ManifestBody    string
	ManifestVersion int
	ShardsPublished int
	ShardsReused    int
}

// FetchDocument fetches one generated index document by absolute path.
type FetchDocument func(ctx context.Context, path string) (protocol.Response, error)

// PublishDocument publishes one generated document with optimistic concurrency.
type PublishDocument func(ctx context.Context, path, body string, expectedVersion int) (protocol.Response, error)

// PublishGeneration stages an inactive shard slot, verifies every shard, then
// commits the manifest last with CAS. A manifest failure leaves the old slot live.
func PublishGeneration(ctx context.Context, opts PublishOptions, fetchDocument FetchDocument, publishDocument PublishDocument) (PublishResult, error) { //nolint:gocritic // options are copied so callers cannot mutate an active publication
	published, err := generation.Publish(ctx, generation.Spec[ShardRef]{
		Labels:                  generation.Labels{Manifest: "manifest", Shard: "shard"},
		ManifestPath:            opts.ManifestPath,
		ExpectedManifestVersion: opts.ExpectedManifestVersion,
		ActiveSlot:              func(body string) (string, error) { return activeSlot(opts.ManifestPath, body) },
		Shards: func(slot string) ([]generation.Shard[ShardRef], error) {
			artifacts, err := BuildShards(opts.ManifestPath, slot, opts.Entries, opts.ShardTargetBytes)
			if err != nil {
				return nil, err
			}
			shards := make([]generation.Shard[ShardRef], len(artifacts))
			for i := range artifacts {
				shards[i] = generation.Shard[ShardRef]{
					Artifact: generation.Artifact{Path: artifacts[i].Path, Body: artifacts[i].Body, ContentHash: artifacts[i].ContentHash},
					Ref:      artifacts[i].Ref,
				}
			}
			return shards, nil
		},
		VerifyShard: func(ref ShardRef, resp protocol.Response) error {
			_, err := VerifyShard(ref, resp)
			return err
		},
		Manifest: func(slot string, refs []ShardRef) (string, error) {
			return BuildManifest(opts.ManifestPath, Manifest{
				Source: opts.Source, Indexed: opts.Indexed.UTC(), Complete: true,
				Documents: len(opts.Entries), ActiveSlot: slot, Shards: refs,
			})
		},
	}, generation.IO{Fetch: fetchDocument, Publish: publishDocument})
	if err != nil {
		return PublishResult{}, err
	}
	parsed, err := ParseManifest(opts.ManifestPath, published.Manifest.Body)
	if err != nil {
		return PublishResult{}, fmt.Errorf("verify manifest %s: %w", opts.ManifestPath, err)
	}
	return PublishResult{
		Manifest:        parsed,
		ManifestBody:    published.Manifest.Body,
		ManifestVersion: published.ManifestVersion,
		ShardsPublished: published.ShardsPublished,
		ShardsReused:    published.ShardsReused,
	}, nil
}

// activeSlot reads the live slot from the document at the manifest path. A
// legacy single document index has no slot and is replaced by slot a.
func activeSlot(manifestPath, body string) (string, error) {
	format, err := documentFormat(body)
	if err != nil {
		return "", err
	}
	if format == "" {
		return "", nil
	}
	if format != ManifestFormat {
		return "", fmt.Errorf("%w: %q", ErrUnsupportedFormat, format)
	}
	manifest, err := ParseManifest(manifestPath, body)
	if err != nil {
		return "", err
	}
	if !manifest.Complete {
		return "", errors.New("current hash index manifest is incomplete")
	}
	return manifest.ActiveSlot, nil
}
