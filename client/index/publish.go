package index

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

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
type FetchDocument func(path string) (protocol.Response, error)

// PublishDocument publishes one generated document with optimistic concurrency.
type PublishDocument func(path, body string, expectedVersion int) (protocol.Response, error)

// PublishGeneration stages an inactive shard slot, verifies every shard, then
// commits the manifest last with CAS. A manifest failure leaves the old slot live.
func PublishGeneration(ctx context.Context, opts PublishOptions, fetchDocument FetchDocument, publishDocument PublishDocument) (PublishResult, error) { //nolint:gocritic // options are copied so callers cannot mutate an active publication
	if err := ctx.Err(); err != nil {
		return PublishResult{}, err
	}
	currentVersion, activeSlot, err := fetchCurrentManifest(opts.ManifestPath, fetchDocument)
	if err != nil {
		return PublishResult{}, err
	}
	if opts.ExpectedManifestVersion != nil && *opts.ExpectedManifestVersion != currentVersion {
		return PublishResult{}, fmt.Errorf("manifest version changed: got %d, expected %d", currentVersion, *opts.ExpectedManifestVersion)
	}
	slot := InactiveSlot(activeSlot)
	artifacts, err := BuildShards(opts.ManifestPath, slot, opts.Entries, opts.ShardTargetBytes)
	if err != nil {
		return PublishResult{}, err
	}
	refs := make([]ShardRef, 0, len(artifacts))
	result := PublishResult{}
	for _, artifact := range artifacts {
		if err := ctx.Err(); err != nil {
			return PublishResult{}, err
		}
		ref, published, err := stageShard(artifact, fetchDocument, publishDocument)
		if err != nil {
			return PublishResult{}, err
		}
		refs = append(refs, ref)
		if published {
			result.ShardsPublished++
		} else {
			result.ShardsReused++
		}
	}

	manifest := Manifest{
		Source: opts.Source, Indexed: opts.Indexed.UTC(), Complete: true,
		Documents: len(opts.Entries), ActiveSlot: slot, Shards: refs,
	}
	manifestBody, err := BuildManifest(opts.ManifestPath, manifest)
	if err != nil {
		return PublishResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return PublishResult{}, err
	}
	verified, manifestVersion, err := publishAndVerify(opts.ManifestPath, manifestBody, currentVersion, fetchDocument, publishDocument)
	if err != nil {
		return PublishResult{}, err
	}
	parsed, err := ParseManifest(opts.ManifestPath, verified.Body)
	if err != nil {
		return PublishResult{}, fmt.Errorf("verify manifest %s: %w", opts.ManifestPath, err)
	}
	result.Manifest = parsed
	result.ManifestBody = manifestBody
	result.ManifestVersion = manifestVersion
	return result, nil
}

func fetchCurrentManifest(manifestPath string, fetchDocument FetchDocument) (version int, activeSlot string, err error) {
	current, err := fetchDocument(manifestPath)
	if err != nil {
		return 0, "", fmt.Errorf("fetch manifest %s: %w", manifestPath, err)
	}
	switch current.Status {
	case protocol.StatusNotFound:
		return 0, "", nil
	case protocol.StatusOK:
		version, err := responseVersion(manifestPath, current)
		if err != nil {
			return 0, "", err
		}
		if current.Metadata["content-hash"] != BodyHash(current.Body) {
			return 0, "", fmt.Errorf("current manifest %s content hash mismatch", manifestPath)
		}
		format, err := documentFormat(current.Body)
		if err != nil {
			return 0, "", err
		}
		if format == "" {
			return version, "", nil
		}
		if format != ManifestFormat {
			return 0, "", fmt.Errorf("%w: %q", ErrUnsupportedFormat, format)
		}
		manifest, err := ParseManifest(manifestPath, current.Body)
		if err != nil {
			return 0, "", err
		}
		if !manifest.Complete {
			return 0, "", errors.New("current hash index manifest is incomplete")
		}
		return version, manifest.ActiveSlot, nil
	default:
		return 0, "", fmt.Errorf("fetch manifest %s returned %s", manifestPath, current.Status)
	}
}

func stageShard(artifact ShardArtifact, fetchDocument FetchDocument, publishDocument PublishDocument) (ShardRef, bool, error) { //nolint:gocritic // immutable publication value
	current, err := fetchDocument(artifact.Path)
	if err != nil {
		return ShardRef{}, false, fmt.Errorf("fetch shard %s: %w", artifact.Path, err)
	}
	expectedVersion := 0
	if current.Status == protocol.StatusOK {
		expectedVersion, err = responseVersion(artifact.Path, current)
		if err != nil {
			return ShardRef{}, false, err
		}
		if current.Metadata["content-hash"] == artifact.ContentHash && current.Body == artifact.Body {
			ref := artifact.Ref(expectedVersion)
			if _, err := VerifyShard(ref, current); err != nil {
				return ShardRef{}, false, err
			}
			return ref, false, nil
		}
	} else if current.Status != protocol.StatusNotFound {
		return ShardRef{}, false, fmt.Errorf("fetch shard %s returned %s", artifact.Path, current.Status)
	}

	verified, version, err := publishAndVerify(artifact.Path, artifact.Body, expectedVersion, fetchDocument, publishDocument)
	if err != nil {
		return ShardRef{}, false, err
	}
	ref := artifact.Ref(version)
	if _, err := VerifyShard(ref, verified); err != nil {
		return ShardRef{}, false, err
	}
	return ref, true, nil
}

func publishAndVerify(docPath, body string, expectedVersion int, fetchDocument FetchDocument, publishDocument PublishDocument) (protocol.Response, int, error) {
	published, publishErr := publishDocument(docPath, body, expectedVersion)
	if publishErr == nil && published.Status != protocol.StatusOK && published.Status != protocol.StatusCreated {
		publishErr = fmt.Errorf("publish returned %s", published.Status)
	}
	version := 0
	if publishErr == nil {
		version, publishErr = responseVersion(docPath, published)
	}
	if publishErr != nil {
		current, err := fetchDocument(docPath)
		if err != nil {
			return protocol.Response{}, 0, fmt.Errorf("publish %s: %v; reconcile: %w", docPath, publishErr, err)
		}
		version, err = verifyPublishedBody(docPath, body, current)
		if err != nil {
			return protocol.Response{}, 0, fmt.Errorf("publish %s: %v", docPath, publishErr)
		}
	}
	versionedPath := VersionPath(docPath, version)
	verified, err := fetchDocument(versionedPath)
	if err != nil {
		return protocol.Response{}, 0, fmt.Errorf("verify %s: %w", versionedPath, err)
	}
	if _, err := verifyPublishedBody(docPath, body, verified); err != nil {
		return protocol.Response{}, 0, err
	}
	return verified, version, nil
}

func verifyPublishedBody(docPath, body string, resp protocol.Response) (int, error) {
	if resp.Status != protocol.StatusOK {
		return 0, fmt.Errorf("verify %s returned %s", docPath, resp.Status)
	}
	if resp.Body != body || resp.Metadata["content-hash"] != BodyHash(body) {
		return 0, fmt.Errorf("verify %s content mismatch", docPath)
	}
	return responseVersion(docPath, resp)
}

func responseVersion(docPath string, resp protocol.Response) (int, error) {
	version, err := strconv.Atoi(resp.Metadata["version"])
	if err != nil || version < 1 {
		return 0, fmt.Errorf("document %s has invalid version %q", docPath, resp.Metadata["version"])
	}
	return version, nil
}
