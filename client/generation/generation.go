// Package generation publishes a sharded, generated artifact so readers never
// see half of one: shards go to the slot that is not live, each is verified,
// and the manifest that flips the slot is committed last. The hash index and
// the graph snapshot are both published this way.
package generation

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/storefmt"
)

// The two slots a generation alternates between.
const (
	SlotA = "a"
	SlotB = "b"
)

// InactiveSlot returns the slot the current manifest does not name.
func InactiveSlot(active string) string {
	if active == SlotA {
		return SlotB
	}
	return SlotA
}

// BodyHash is the protocol content hash of body.
func BodyHash(body string) string { return storefmt.ContentHash([]byte(body)) }

// IO reads and writes generated documents by absolute path. Publish must
// wrap a lost answer with %w so errors.Is finds protocol.ErrOutcomeUnknown;
// a %v wrap turns the reconcile off and the lost write reads as a failure.
type IO struct {
	Fetch   func(ctx context.Context, path string) (protocol.Response, error)
	Publish func(ctx context.Context, path, body string, expectedVersion int) (protocol.Response, error)
}

// Labels name the documents in errors, so a failure says which artifact it was.
type Labels struct {
	Manifest string // "manifest", "snapshot manifest"
	Shard    string // "shard", "graph shard"
}

// Artifact is one shard body before its published version is known.
type Artifact struct {
	Path, Body, ContentHash string
}

// Shard is an artifact plus how its format describes it once published.
type Shard[R any] struct {
	Artifact
	Ref func(version int) R
}

// Spec is one complete generation of a format with shard descriptors of type R.
type Spec[R any] struct {
	Labels                  Labels
	ManifestPath            string
	ExpectedManifestVersion *int
	// ActiveSlot reads the live slot from the current manifest body, "" when the
	// document there predates slots. Its error refuses the publication.
	ActiveSlot func(body string) (string, error)
	// Shards builds the generation for the slot it will occupy.
	Shards func(slot string) ([]Shard[R], error)
	// VerifyShard checks a fetched shard against its description.
	VerifyShard func(ref R, resp protocol.Response) error
	// Manifest renders the manifest over the staged shards.
	Manifest func(slot string, refs []R) (string, error)
}

// Result is the committed generation.
type Result[R any] struct {
	Slot            string
	Refs            []R
	Manifest        protocol.Response // the manifest as read back at its version
	ManifestVersion int
	ShardsPublished int
	ShardsReused    int
}

// Publish stages every shard in the inactive slot, then commits the manifest
// with CAS. A failure before the manifest leaves the old generation live.
func Publish[R any](ctx context.Context, spec Spec[R], io IO) (Result[R], error) { //nolint:gocritic // a spec is built once and passed once
	p := publisher{io: io, labels: spec.Labels}
	currentVersion, activeSlot, err := p.currentManifest(ctx, spec.ManifestPath, spec.ActiveSlot)
	if err != nil {
		return Result[R]{}, err
	}
	if spec.ExpectedManifestVersion != nil && *spec.ExpectedManifestVersion != currentVersion {
		return Result[R]{}, fmt.Errorf("%s version changed: got %d, expected %d", spec.Labels.Manifest, currentVersion, *spec.ExpectedManifestVersion)
	}
	result := Result[R]{Slot: InactiveSlot(activeSlot)}
	shards, err := spec.Shards(result.Slot)
	if err != nil {
		return Result[R]{}, err
	}
	result.Refs = make([]R, 0, len(shards))
	for i := range shards {
		ref, published, err := stage(ctx, &p, &shards[i], spec.VerifyShard)
		if err != nil {
			return Result[R]{}, err
		}
		result.Refs = append(result.Refs, ref)
		if published {
			result.ShardsPublished++
		} else {
			result.ShardsReused++
		}
	}
	manifest, err := spec.Manifest(result.Slot, result.Refs)
	if err != nil {
		return Result[R]{}, err
	}
	result.Manifest, result.ManifestVersion, err = p.publishVerified(ctx, document{path: spec.ManifestPath, body: manifest, expected: currentVersion})
	if err != nil {
		return Result[R]{}, err
	}
	return result, nil
}

// publisher is the part of a publication that does not depend on the format.
type publisher struct {
	io     IO
	labels Labels
}

// document is one write: body at path, over the version it expects to replace.
type document struct {
	path, body string
	expected   int
}

// currentManifest reads the live manifest: its version and slot, zero and ""
// when there is none. A body that does not hash to its metadata is refused.
func (p *publisher) currentManifest(ctx context.Context, path string, activeSlot func(string) (string, error)) (version int, slot string, err error) {
	current, err := p.io.Fetch(ctx, path)
	if err != nil {
		return 0, "", fmt.Errorf("fetch %s %s: %w", p.labels.Manifest, path, err)
	}
	switch current.Status {
	case protocol.StatusNotFound:
		return 0, "", nil
	case protocol.StatusOK:
	default:
		return 0, "", fmt.Errorf("fetch %s %s returned %s", p.labels.Manifest, path, current.Status)
	}
	if version, err = responseVersion(path, current); err != nil {
		return 0, "", err
	}
	if current.Metadata["content-hash"] != BodyHash(current.Body) {
		return 0, "", fmt.Errorf("current %s %s content hash mismatch", p.labels.Manifest, path)
	}
	if slot, err = activeSlot(current.Body); err != nil {
		return 0, "", err
	}
	return version, slot, nil
}

// stage puts one shard in place: reused when the slot already holds exactly
// this body, published and read back otherwise.
func stage[R any](ctx context.Context, p *publisher, shard *Shard[R], verify func(R, protocol.Response) error) (ref R, published bool, err error) {
	var none R
	current, err := p.io.Fetch(ctx, shard.Path)
	if err != nil {
		return none, false, fmt.Errorf("fetch %s %s: %w", p.labels.Shard, shard.Path, err)
	}
	expected := 0
	switch current.Status {
	case protocol.StatusNotFound:
	case protocol.StatusOK:
		if expected, err = responseVersion(shard.Path, current); err != nil {
			return none, false, err
		}
		if current.Metadata["content-hash"] == shard.ContentHash && current.Body == shard.Body {
			ref = shard.Ref(expected)
			return ref, false, verify(ref, current)
		}
	default:
		return none, false, fmt.Errorf("fetch %s %s returned %s", p.labels.Shard, shard.Path, current.Status)
	}
	verified, version, err := p.publishVerified(ctx, document{path: shard.Path, body: shard.Body, expected: expected})
	if err != nil {
		return none, false, err
	}
	ref = shard.Ref(version)
	return ref, true, verify(ref, verified)
}

// publishVerified writes doc once and reads it back at its version. A refusal,
// or a response that was lost, is settled by content: the head may already be
// this exact document. It is never resent.
func (p *publisher) publishVerified(ctx context.Context, doc document) (protocol.Response, int, error) {
	published, publishErr := p.io.Publish(ctx, doc.path, doc.body, doc.expected)
	// A write that never left cannot have landed.
	if publishErr != nil && !errors.Is(publishErr, protocol.ErrOutcomeUnknown) {
		return protocol.Response{}, 0, fmt.Errorf("publish %s: %w", doc.path, publishErr)
	}
	if publishErr == nil && !protocol.IsWriteSuccess(published.Status) {
		publishErr = fmt.Errorf("publish returned %s", published.Status)
	}
	version := 0
	if publishErr == nil {
		version, publishErr = responseVersion(doc.path, published)
	}
	if publishErr != nil {
		head, err := p.io.Fetch(ctx, doc.path)
		if err != nil {
			return protocol.Response{}, 0, fmt.Errorf("publish %s: %v; reconcile: %w", doc.path, publishErr, err)
		}
		if version, err = verifyBody(doc, head); err != nil {
			return protocol.Response{}, 0, fmt.Errorf("publish %s: %w; reconcile: %w", doc.path, publishErr, err)
		}
	}
	versioned := protocol.VersionPath(doc.path, version)
	verified, err := p.io.Fetch(ctx, versioned)
	if err != nil {
		return protocol.Response{}, 0, fmt.Errorf("verify %s: %w", versioned, err)
	}
	if _, err := verifyBody(doc, verified); err != nil {
		return protocol.Response{}, 0, err
	}
	return verified, version, nil
}

// verifyBody checks that resp is doc as written and returns its version.
func verifyBody(doc document, resp protocol.Response) (int, error) {
	if resp.Status != protocol.StatusOK {
		return 0, fmt.Errorf("verify %s returned %s", doc.path, resp.Status)
	}
	if resp.Body != doc.body || resp.Metadata["content-hash"] != BodyHash(doc.body) {
		return 0, fmt.Errorf("verify %s content mismatch", doc.path)
	}
	return responseVersion(doc.path, resp)
}

func responseVersion(docPath string, resp protocol.Response) (int, error) {
	version, err := strconv.Atoi(resp.Metadata["version"])
	if err != nil || version < 1 {
		return 0, fmt.Errorf("document %s has invalid version %q", docPath, resp.Metadata["version"])
	}
	return version, nil
}
