package graphstore

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

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
	currentVersion, activeSlot, err := fetchCurrentSnapshot(ctx, opts.ManifestPath, fetchDocument)
	if err != nil {
		return SnapshotPublishResult{}, err
	}
	if opts.ExpectedManifestVersion != nil && *opts.ExpectedManifestVersion != currentVersion {
		return SnapshotPublishResult{}, fmt.Errorf("snapshot manifest version changed: got %d, expected %d", currentVersion, *opts.ExpectedManifestVersion)
	}
	slot := InactiveSnapshotSlot(activeSlot)
	artifacts, err := BuildSnapshotShards(opts.ManifestPath, slot, opts.Nodes, opts.Edges, opts.ShardTargetBytes)
	if err != nil {
		return SnapshotPublishResult{}, err
	}
	refs := make([]SnapshotShardRef, 0, len(artifacts))
	result := SnapshotPublishResult{}
	for _, artifact := range artifacts {
		ref, published, err := stageSnapshotShard(ctx, artifact, fetchDocument, publishDocument)
		if err != nil {
			return SnapshotPublishResult{}, err
		}
		refs = append(refs, ref)
		if published {
			result.ShardsPublished++
		} else {
			result.ShardsReused++
		}
	}
	manifest := SnapshotManifest{
		Exported: opts.Exported.UTC(), Complete: true, Nodes: len(opts.Nodes), Edges: len(opts.Edges),
		ActiveSlot: slot, Shards: refs,
	}
	body, err := BuildSnapshotManifest(opts.ManifestPath, manifest)
	if err != nil {
		return SnapshotPublishResult{}, err
	}
	verified, version, err := publishAndVerifySnapshot(ctx, opts.ManifestPath, body, currentVersion, fetchDocument, publishDocument)
	if err != nil {
		return SnapshotPublishResult{}, err
	}
	parsed, err := ParseSnapshotManifest(opts.ManifestPath, verified.Body)
	if err != nil {
		return SnapshotPublishResult{}, fmt.Errorf("verify snapshot manifest: %w", err)
	}
	result.Manifest = parsed
	result.ManifestVersion = version
	return result, nil
}

func fetchCurrentSnapshot(ctx context.Context, manifestPath string, fetchDocument SnapshotFetch) (version int, activeSlot string, err error) {
	current, err := fetchDocument(ctx, manifestPath)
	if err != nil {
		return 0, "", fmt.Errorf("fetch snapshot manifest: %w", err)
	}
	switch current.Status {
	case protocol.StatusNotFound:
		return 0, "", nil
	case protocol.StatusOK:
		version, err := snapshotResponseVersion(manifestPath, current)
		if err != nil {
			return 0, "", err
		}
		if current.Metadata["content-hash"] != SnapshotBodyHash(current.Body) {
			return 0, "", errors.New("current snapshot manifest content hash mismatch")
		}
		manifest, err := ParseSnapshotManifest(manifestPath, current.Body)
		if err != nil {
			return 0, "", err
		}
		return version, manifest.ActiveSlot, nil
	default:
		return 0, "", fmt.Errorf("fetch snapshot manifest returned %s", current.Status)
	}
}

func stageSnapshotShard(ctx context.Context, artifact SnapshotArtifact, fetchDocument SnapshotFetch, publishDocument SnapshotPublish) (SnapshotShardRef, bool, error) { //nolint:gocritic // immutable artifact value
	current, err := fetchDocument(ctx, artifact.Path)
	if err != nil {
		return SnapshotShardRef{}, false, fmt.Errorf("fetch graph shard %s: %w", artifact.Path, err)
	}
	expectedVersion := 0
	if current.Status == protocol.StatusOK {
		expectedVersion, err = snapshotResponseVersion(artifact.Path, current)
		if err != nil {
			return SnapshotShardRef{}, false, err
		}
		if current.Body == artifact.Body && current.Metadata["content-hash"] == artifact.ContentHash {
			ref := artifact.Ref(expectedVersion)
			if _, _, err := verifySnapshotShard(ref, current); err != nil {
				return SnapshotShardRef{}, false, err
			}
			return ref, false, nil
		}
	} else if current.Status != protocol.StatusNotFound {
		return SnapshotShardRef{}, false, fmt.Errorf("fetch graph shard %s returned %s", artifact.Path, current.Status)
	}
	verified, version, err := publishAndVerifySnapshot(ctx, artifact.Path, artifact.Body, expectedVersion, fetchDocument, publishDocument)
	if err != nil {
		return SnapshotShardRef{}, false, err
	}
	ref := artifact.Ref(version)
	if _, _, err := verifySnapshotShard(ref, verified); err != nil {
		return SnapshotShardRef{}, false, err
	}
	return ref, true, nil
}

func publishAndVerifySnapshot(ctx context.Context, docPath, body string, expectedVersion int, fetchDocument SnapshotFetch, publishDocument SnapshotPublish) (protocol.Response, int, error) {
	published, publishErr := publishDocument(ctx, docPath, body, expectedVersion)
	if publishErr == nil && published.Status != protocol.StatusOK && published.Status != protocol.StatusCreated {
		publishErr = fmt.Errorf("publish returned %s", published.Status)
	}
	version := 0
	if publishErr == nil {
		version, publishErr = snapshotResponseVersion(docPath, published)
	}
	if publishErr != nil {
		current, err := fetchDocument(ctx, docPath)
		if err != nil {
			return protocol.Response{}, 0, fmt.Errorf("publish %s: %v; reconcile: %w", docPath, publishErr, err)
		}
		version, err = verifySnapshotBody(docPath, body, current)
		if err != nil {
			return protocol.Response{}, 0, fmt.Errorf("publish %s: %w; reconcile: %w", docPath, publishErr, err)
		}
	}
	verified, err := fetchDocument(ctx, SnapshotVersionPath(docPath, version))
	if err != nil {
		return protocol.Response{}, 0, fmt.Errorf("verify graph snapshot %s: %w", docPath, err)
	}
	if _, err := verifySnapshotBody(docPath, body, verified); err != nil {
		return protocol.Response{}, 0, err
	}
	return verified, version, nil
}

func verifySnapshotBody(docPath, body string, response protocol.Response) (int, error) {
	if response.Status != protocol.StatusOK {
		return 0, fmt.Errorf("verify graph snapshot %s returned %s", docPath, response.Status)
	}
	if response.Body != body || response.Metadata["content-hash"] != SnapshotBodyHash(body) {
		return 0, fmt.Errorf("verify graph snapshot %s content mismatch", docPath)
	}
	return snapshotResponseVersion(docPath, response)
}

func snapshotResponseVersion(docPath string, response protocol.Response) (int, error) {
	version, err := strconv.Atoi(response.Metadata["version"])
	if err != nil || version < 1 {
		return 0, fmt.Errorf("graph snapshot document %s has invalid version %q", docPath, response.Metadata["version"])
	}
	return version, nil
}
