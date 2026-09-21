package fetchtest

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"sync"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/generation"
	"github.com/latebit-io/demarkus/client/graphstore"
	"github.com/latebit-io/demarkus/protocol"
)

// Client is a scriptable fetch client that records every call. With no
// function a read finds nothing and a write succeeds. A successful PUBLISH that
// reports a version is stored and served by later FETCHes, with conflicts.
type Client struct {
	mu sync.Mutex

	FetchFn func(context.Context, fetch.FetchRequest) (fetch.Result, error)
	// SeedFn answers the graph seed documents: the legacy export and the snapshot
	// manifest. FetchFn never sees either; unscripted, a seed is whatever was
	// Published, else not found.
	SeedFn func(context.Context, fetch.FetchRequest) (fetch.Result, error)
	// SnapshotFn, when set, answers the snapshot manifest in place of SeedFn.
	SnapshotFn func(context.Context, fetch.FetchRequest) (fetch.Result, error)
	ListFn     func(context.Context, fetch.ListRequest) (fetch.Result, error)
	VersionsFn func(context.Context, fetch.VersionsRequest) (fetch.Result, error)
	LookupFn   func(context.Context, fetch.LookupRequest) (fetch.Result, error)
	PublishFn  func(context.Context, fetch.WriteRequest) (fetch.Result, error)
	AppendFn   func(context.Context, fetch.WriteRequest) (fetch.Result, error)
	ArchiveFn  func(context.Context, fetch.ArchiveRequest) (fetch.Result, error)
	Published  map[string]fetch.Result

	// Every call in order; a write's Metadata is a copy. SeedCalls holds the
	// seed document FETCHes, which FetchCalls leaves out.
	FetchCalls    []fetch.FetchRequest
	SeedCalls     []fetch.FetchRequest
	ListCalls     []fetch.ListRequest
	VersionsCalls []fetch.VersionsRequest
	LookupCalls   []fetch.LookupRequest
	PublishCalls  []fetch.WriteRequest
	AppendCalls   []fetch.WriteRequest
	ArchiveCalls  []fetch.ArchiveRequest
}

func status(code string, meta map[string]string) fetch.Result {
	return fetch.Result{Response: protocol.Response{Status: code, Metadata: meta}}
}

func isSeedPath(path string) bool {
	return path == graphstore.LegacyExportPath || path == graphstore.SnapshotManifestPath
}

// Fetch records and answers a FETCH. A done context fails first, as it does
// for every verb, so no callback runs for a request a real client never sends.
func (c *Client) Fetch(ctx context.Context, r fetch.FetchRequest) (fetch.Result, error) {
	if err := ctx.Err(); err != nil {
		return fetch.Result{}, err
	}
	c.mu.Lock()
	fn := c.FetchFn
	stored, ok := c.Published[r.Host+r.Path]
	if isSeedPath(r.Path) {
		c.SeedCalls = append(c.SeedCalls, r)
		fn = c.seedFn(r.Path)
		ok = ok && fn == nil
	} else {
		c.FetchCalls = append(c.FetchCalls, r)
	}
	c.mu.Unlock()
	if ok {
		return stored, nil
	}
	if fn == nil {
		return status(protocol.StatusNotFound, nil), nil
	}
	return fn(ctx, r)
}

// List records and answers a LIST.
func (c *Client) List(ctx context.Context, r fetch.ListRequest) (fetch.Result, error) {
	if err := ctx.Err(); err != nil {
		return fetch.Result{}, err
	}
	c.mu.Lock()
	c.ListCalls = append(c.ListCalls, r)
	fn := c.ListFn
	c.mu.Unlock()
	if fn == nil {
		return ListPage(r.Path, ""), nil
	}
	return fn(ctx, r)
}

// Versions records and answers a VERSIONS.
func (c *Client) Versions(ctx context.Context, r fetch.VersionsRequest) (fetch.Result, error) {
	if err := ctx.Err(); err != nil {
		return fetch.Result{}, err
	}
	c.mu.Lock()
	c.VersionsCalls = append(c.VersionsCalls, r)
	fn := c.VersionsFn
	c.mu.Unlock()
	if fn == nil {
		return status(protocol.StatusOK, nil), nil
	}
	return fn(ctx, r)
}

// Lookup records and answers a LOOKUP.
func (c *Client) Lookup(ctx context.Context, r fetch.LookupRequest) (fetch.Result, error) {
	if err := ctx.Err(); err != nil {
		return fetch.Result{}, err
	}
	c.mu.Lock()
	c.LookupCalls = append(c.LookupCalls, r)
	fn := c.LookupFn
	c.mu.Unlock()
	if fn == nil {
		return Lookup(r.Query, r.Scope, ""), nil
	}
	return fn(ctx, r)
}

// Publish records and answers a PUBLISH.
func (c *Client) Publish(ctx context.Context, r fetch.WriteRequest) (fetch.Result, error) {
	if err := ctx.Err(); err != nil {
		return fetch.Result{}, err
	}
	c.mu.Lock()
	c.PublishCalls = append(c.PublishCalls, recorded(r))
	fn := c.PublishFn
	c.mu.Unlock()
	result := status(protocol.StatusOK, map[string]string{"version": "1"})
	if fn != nil {
		var err error
		if result, err = fn(ctx, r); err != nil {
			return result, err
		}
	}
	if s := result.Response.Status; s != protocol.StatusOK && s != protocol.StatusCreated {
		return result, nil
	}
	version, err := strconv.Atoi(result.Response.Metadata["version"])
	if err != nil || version <= 0 {
		return result, nil
	}
	return c.store(r, version, result)
}

// recorded copies the metadata so a caller's later change cannot rewrite the record.
func recorded(r fetch.WriteRequest) fetch.WriteRequest {
	r.Metadata = maps.Clone(r.Metadata)
	return r
}

// store keeps a published body as the current and the numbered version.
func (c *Client) store(r fetch.WriteRequest, version int, result fetch.Result) (fetch.Result, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	current := 0
	if existing, ok := c.Published[r.Host+r.Path]; ok {
		var err error
		if current, err = strconv.Atoi(existing.Response.Metadata["version"]); err != nil {
			return fetch.Result{}, fmt.Errorf("invalid stored version: %w", err)
		}
	}
	if r.ExpectedVersion >= 0 && r.ExpectedVersion != current {
		return status(protocol.StatusConflict, nil), nil
	}
	// As the server does: publisher keys survive, server owned keys are its own.
	metadata := make(map[string]string, len(r.Metadata)+2)
	for key, value := range r.Metadata {
		if !protocol.IsReservedMetadataKey(key) {
			metadata[key] = value
		}
	}
	metadata["version"] = strconv.Itoa(version)
	metadata["content-hash"] = generation.BodyHash(r.Body)
	stored := fetch.Result{Response: protocol.Response{Status: protocol.StatusOK, Body: r.Body, Metadata: metadata}}
	if c.Published == nil {
		c.Published = make(map[string]fetch.Result)
	}
	c.Published[r.Host+r.Path] = stored
	c.Published[r.Host+generation.VersionPath(r.Path, version)] = stored
	return result, nil
}

// Append records and answers an APPEND.
func (c *Client) Append(ctx context.Context, r fetch.WriteRequest) (fetch.Result, error) {
	if err := ctx.Err(); err != nil {
		return fetch.Result{}, err
	}
	c.mu.Lock()
	c.AppendCalls = append(c.AppendCalls, recorded(r))
	fn := c.AppendFn
	c.mu.Unlock()
	if fn == nil {
		return status(protocol.StatusOK, map[string]string{"version": "2"}), nil
	}
	return fn(ctx, r)
}

// Archive records and answers an ARCHIVE.
func (c *Client) Archive(ctx context.Context, r fetch.ArchiveRequest) (fetch.Result, error) {
	if err := ctx.Err(); err != nil {
		return fetch.Result{}, err
	}
	c.mu.Lock()
	c.ArchiveCalls = append(c.ArchiveCalls, r)
	fn := c.ArchiveFn
	c.mu.Unlock()
	if fn == nil {
		return status(protocol.StatusOK, map[string]string{"version": "3", "archived": "true"}), nil
	}
	return fn(ctx, r)
}

// FetchCallCount is the number of document FETCHes so far, safe during concurrent use.
func (c *Client) FetchCallCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.FetchCalls)
}

// Calls is a copy of every recorded call.
type Calls struct {
	Fetch, Seed     []fetch.FetchRequest
	List            []fetch.ListRequest
	Versions        []fetch.VersionsRequest
	Lookup          []fetch.LookupRequest
	Publish, Append []fetch.WriteRequest
	Archive         []fetch.ArchiveRequest
}

// Calls copies the record under the lock, for reads that race the client.
func (c *Client) Calls() Calls {
	c.mu.Lock()
	defer c.mu.Unlock()
	return Calls{
		Fetch:    slices.Clone(c.FetchCalls),
		Seed:     slices.Clone(c.SeedCalls),
		List:     slices.Clone(c.ListCalls),
		Versions: slices.Clone(c.VersionsCalls),
		Lookup:   slices.Clone(c.LookupCalls),
		Publish:  slices.Clone(c.PublishCalls),
		Append:   slices.Clone(c.AppendCalls),
		Archive:  slices.Clone(c.ArchiveCalls),
	}
}

// Lock guards direct reads of the call record while the client is in use.
func (c *Client) Lock() { c.mu.Lock() }

// Unlock releases Lock.
func (c *Client) Unlock() { c.mu.Unlock() }

// seedFn is the scripted answer for a seed document, nil when none is set.
func (c *Client) seedFn(path string) func(context.Context, fetch.FetchRequest) (fetch.Result, error) {
	if path == graphstore.SnapshotManifestPath && c.SnapshotFn != nil {
		return c.SnapshotFn
	}
	return c.SeedFn
}
