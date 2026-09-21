package marktools

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/index"
	"github.com/latebit-io/demarkus/client/listwalk"
	"github.com/latebit-io/demarkus/protocol"
)

// maxIndexDocuments caps the documents one mark_index run fetches.
const maxIndexDocuments = 1000

var errIndexTruncated = errors.New("document limit reached, index is truncated")

// IndexArgs are mark_index's arguments. ExpectedVersion above zero merges into
// the index already published at Target.
type IndexArgs struct {
	Source, Target  string
	DryRun, Force   bool
	ExpectedVersion int
}

// Index answers mark_index: hash every document under Source and publish the
// index at Target. A crawl that skipped anything is never published.
func (t *Tools) Index(ctx context.Context, args IndexArgs) Result {
	source, err := t.hooks.Resolve(ctx, args.Source)
	if err != nil {
		return failure("invalid source URL: %v", err)
	}
	if source.Path == "" {
		source.Path = "/"
	}
	target, err := t.hooks.Resolve(ctx, args.Target)
	if err != nil {
		return failure("invalid target URL: %v", err)
	}
	if args.ExpectedVersion < 0 {
		return failure("expected_version must be non-negative")
	}
	// A dry run publishes nothing, so it asks nobody; anything else is
	// authorized here, before the first request of the crawl.
	var write WriteFunc
	if !args.DryRun {
		var refused *Result
		if write, refused = t.writer(ctx, target, "publishing"); refused != nil {
			return *refused
		}
	}
	warnings, bad := t.checkManifests(ctx, &manifestCheck{source: source, target: target, dryRun: args.DryRun, force: args.Force})
	if bad != nil {
		return *bad
	}

	entries, crawlWarnings, err := t.collectEntries(ctx, source)
	warnings = append(warnings, crawlWarnings...)
	truncated := errors.Is(err, errIndexTruncated)
	if err != nil && !truncated {
		return failure("crawl failed: %v", err)
	}
	if truncated {
		warnings = append(warnings, fmt.Sprintf("warning: index truncated at %d documents, some content may not be indexed", maxIndexDocuments))
	}
	indexedAt := t.now()

	var b strings.Builder
	for _, w := range warnings {
		b.WriteString(w + "\n")
	}
	if args.DryRun {
		fmt.Fprintf(&b, "Indexed %d documents from %s (dry run, logical entry preview only; publication writes a v2 manifest and shards)\n\n", len(entries), source.Authority)
		b.WriteString(index.Build(source.Authority, indexedAt, entries))
		return text(b.String())
	}
	if truncated || len(crawlWarnings) > 0 {
		return failure("crawl incomplete; refusing to publish an authoritative index")
	}
	published, bad := t.publishIndex(ctx, &indexRun{source: source, target: target, entries: entries, indexedAt: indexedAt, expectedVersion: args.ExpectedVersion, write: write})
	if bad != nil {
		return *bad
	}
	fmt.Fprintf(&b, "Indexed %d documents from %s\n", len(entries), source.Authority)
	fmt.Fprintf(&b, "status: ok\nversion: %d\nshards-published: %d\nshards-reused: %d\n",
		published.ManifestVersion, published.ShardsPublished, published.ShardsReused)
	return text(b.String())
}

// indexRun is one crawl on its way to publication.
type indexRun struct {
	source, target  Target
	entries         []index.Entry
	indexedAt       time.Time
	expectedVersion int
	write           WriteFunc
}

// publishIndex merges into an existing index when asked, and publishes one
// generation.
func (t *Tools) publishIndex(ctx context.Context, run *indexRun) (index.PublishResult, *Result) {
	read := func(ctx context.Context, path string) (protocol.Response, error) {
		result, err := t.fetch(ctx, run.target, path)
		return result.Response, err
	}
	manifestSource, entries := run.source.Authority, run.entries
	if run.expectedVersion > 0 {
		existing, err := read(ctx, run.target.Path)
		if err != nil {
			bad := t.failed(SiteIndexExisting, run.target.Host, err)
			return index.PublishResult{}, &bad
		}
		if existing.Status != protocol.StatusOK {
			bad := failure("failed to fetch existing index: %s", existing.Status)
			return index.PublishResult{}, &bad
		}
		existingEntries, err := index.LoadEntries(run.target.Path, existing.Body, func(shardPath string) (protocol.Response, error) {
			return read(ctx, shardPath)
		})
		if err != nil {
			bad := failure("failed to read existing index: %v", err)
			return index.PublishResult{}, &bad
		}
		entries = index.Merge(existingEntries, run.source.Authority, run.entries)
		manifestSource = run.target.Authority
	}
	meta := t.agentMeta(ctx)
	published, err := index.PublishGeneration(ctx, index.PublishOptions{
		ManifestPath:            run.target.Path,
		Source:                  manifestSource,
		Indexed:                 run.indexedAt,
		Entries:                 entries,
		ExpectedManifestVersion: &run.expectedVersion,
	}, read, func(ioCtx context.Context, docPath, body string, expected int) (protocol.Response, error) {
		result, err := run.write(ioCtx, func(token string) (fetch.Result, error) {
			return t.backend.Publish(ioCtx, fetch.WriteRequest{
				Host: run.target.Host, Path: docPath, Token: token,
				Body: body, ExpectedVersion: expected, Metadata: meta,
			})
		})
		return result.Response, err
	})
	if err != nil {
		bad := t.failed(SiteIndexPublish, run.target.Host, err)
		return index.PublishResult{}, &bad
	}
	return published, nil
}

// manifestCheck is one run's question: may source be indexed, does target
// accept indexes.
type manifestCheck struct {
	source, target Target
	dryRun, force  bool
}

// checkManifests reads both agent manifests. An unread manifest is not a
// missing one: the source only warns, and force overrides a missing target
// manifest, never an unreachable target.
func (t *Tools) checkManifests(ctx context.Context, c *manifestCheck) ([]string, *Result) {
	manifest := func(at Target) (fetch.Result, error) { return t.fetch(ctx, at, protocol.WellKnownManifestPath) }
	var warnings []string
	src, err := manifest(c.source)
	switch {
	case err != nil:
		warnings = append(warnings, fmt.Sprintf("warning: could not check source agent manifest: %v", err))
	case src.Response.Status != protocol.StatusOK:
		warnings = append(warnings, "warning: source server has no agent manifest")
	}
	if c.dryRun {
		return warnings, nil
	}
	tgt, err := manifest(c.target)
	if err != nil {
		bad := failure("could not check target agent manifest: %v", err)
		return warnings, &bad
	}
	switch tgt.Response.Status {
	case protocol.StatusOK:
	case protocol.StatusNotFound:
		if !c.force {
			bad := failure("target server has no agent manifest; cannot verify it accepts index publications. " +
				"Use force=true to override, or publish a manifest at /.well-known/agent-manifest.md on the target.")
			return warnings, &bad
		}
		warnings = append(warnings, "warning: target server has no agent manifest (force=true override)")
	default:
		// Unauthorized or a server fault says nothing about the manifest.
		bad := failure("could not check target agent manifest: status %s", tgt.Response.Status)
		return warnings, &bad
	}
	return warnings, nil
}

// collectEntries walks source and hashes each document. Every skip is a
// warning, since a partial index must never read as a whole one.
func (t *Tools) collectEntries(ctx context.Context, source Target) ([]index.Entry, []string, error) {
	var entries []index.Entry
	var warnings []string
	skipped := func(path string, why any) {
		warnings = append(warnings, fmt.Sprintf("warning: skipped %s: %v", path, why))
	}
	token := t.readToken(ctx, source.Host)
	attempts := 0
	walker := listwalk.Walker{
		Client: t.backend, Host: source.Host, Token: token,
		OnProblem: listwalk.Skip(func(path, reason string) { skipped(path, reason) }),
	}
	err := walker.Walk(ctx, source.Path, func(docPath string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if attempts >= maxIndexDocuments {
			return errIndexTruncated
		}
		attempts++
		doc, err := t.backend.Fetch(ctx, fetch.FetchRequest{Host: source.Host, Path: docPath, Token: token})
		switch {
		case err != nil:
			skipped(docPath, err)
		case doc.Response.Status != protocol.StatusOK:
			skipped(docPath, doc.Response.Status)
		default:
			hash := doc.Response.Metadata["content-hash"]
			if _, valid := protocol.IsHashPath(hash); !valid {
				skipped(docPath, "missing or invalid content-hash")
				return nil
			}
			entries = append(entries, index.Entry{Hash: hash, Server: source.Authority, Path: docPath})
		}
		return nil
	})
	if errors.Is(err, listwalk.ErrListBudget) {
		warnings = append(warnings, "warning: directory budget exhausted, index is incomplete")
		err = nil
	}
	return entries, warnings, err
}

func (t *Tools) now() time.Time {
	if t.hooks.Now != nil {
		return t.hooks.Now()
	}
	return time.Now()
}
