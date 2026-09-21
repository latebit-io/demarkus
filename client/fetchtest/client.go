package fetchtest

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"sync"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/graphstore"
	"github.com/latebit-io/demarkus/client/index"
	"github.com/latebit-io/demarkus/protocol"
)

type (
	// ReadFn answers FETCH, LIST, VERSIONS and ARCHIVE.
	ReadFn func(host, path, token string) (fetch.Result, error)
	// CondFn answers a conditional FETCH.
	CondFn func(host, path, token, etag string) (fetch.Result, error)
	// WriteFn answers PUBLISH and APPEND.
	WriteFn func(host, path, body, token string, expectedVersion int, meta map[string]string) (fetch.Result, error)
	// LookupFn answers LOOKUP.
	LookupFn func(host, scope, query, token string, opts fetch.LookupOptions) (fetch.Result, error)
)

// Call records one read style request.
type Call struct {
	Host, Path, Token string
}

// CondCall records one conditional FETCH.
type CondCall struct {
	Host, Path, Token, Etag string
}

// LookupCall records one LOOKUP.
type LookupCall struct {
	Host, Scope, Query, Token string
	Opts                      fetch.LookupOptions
}

// WriteCall records one PUBLISH or APPEND; Meta is a copy.
type WriteCall struct {
	Host, Path, Body, Token string
	ExpectedVersion         int
	Meta                    map[string]string
}

// Client is a scriptable fetch client that records every call. With no
// function a read finds nothing and a write succeeds. A successful PUBLISH that
// reports a version is stored and served by later FETCHes, with conflicts.
type Client struct {
	mu sync.Mutex

	FetchFn     ReadFn
	FetchCtxFn  func(ctx context.Context, host, path, token string) (fetch.Result, error)
	FetchCondFn CondFn
	// SnapshotFn, when set, answers conditional FETCHes of the graph snapshot
	// manifest in place of FetchCondFn.
	SnapshotFn  CondFn
	ListFn      ReadFn
	ListOptsFn  func(host, path, token string, opts fetch.ListOptions) (fetch.Result, error)
	VersionsFn  ReadFn
	LookupFn    LookupFn
	LookupCtxFn func(ctx context.Context, host, scope, query, token string, opts fetch.LookupOptions) (fetch.Result, error)
	PublishFn   WriteFn
	AppendFn    WriteFn
	ArchiveFn   ReadFn
	Published   map[string]fetch.Result

	FetchCalls     []Call
	FetchCondCalls []CondCall
	ListCalls      []Call
	ListOpts       []fetch.ListOptions
	VersionsCalls  []Call
	LookupCalls    []LookupCall
	PublishCalls   []WriteCall
	AppendCalls    []WriteCall
	ArchiveCalls   []Call
}

func status(code string, meta map[string]string) fetch.Result {
	return fetch.Result{Response: protocol.Response{Status: code, Metadata: meta}}
}

// Fetch records and answers a FETCH.
func (c *Client) Fetch(host, path, token string) (fetch.Result, error) {
	c.mu.Lock()
	c.FetchCalls = append(c.FetchCalls, Call{host, path, token})
	stored, ok := c.Published[host+path]
	fn := c.FetchFn
	c.mu.Unlock()
	if ok {
		return stored, nil
	}
	if fn == nil {
		return status(protocol.StatusNotFound, nil), nil
	}
	return fn(host, path, token)
}

// FetchContext is Fetch, failing first when ctx is done.
func (c *Client) FetchContext(ctx context.Context, host, path, token string) (fetch.Result, error) {
	if err := ctx.Err(); err != nil {
		return fetch.Result{}, err
	}
	if c.FetchCtxFn != nil {
		return c.FetchCtxFn(ctx, host, path, token)
	}
	return c.Fetch(host, path, token)
}

// FetchConditional records and answers a FETCHCONDITIONAL.
func (c *Client) FetchConditional(host, path, token, etag string) (fetch.Result, error) {
	c.mu.Lock()
	c.FetchCondCalls = append(c.FetchCondCalls, CondCall{host, path, token, etag})
	fn := c.FetchCondFn
	if path == graphstore.SnapshotManifestPath && c.SnapshotFn != nil {
		fn = c.SnapshotFn
	}
	c.mu.Unlock()
	if fn == nil {
		return status(protocol.StatusNotFound, nil), nil
	}
	return fn(host, path, token, etag)
}

// FetchConditionalContext is FetchConditional, failing first when ctx is done.
func (c *Client) FetchConditionalContext(ctx context.Context, host, path, token, etag string) (fetch.Result, error) {
	if err := ctx.Err(); err != nil {
		return fetch.Result{}, err
	}
	return c.FetchConditional(host, path, token, etag)
}

// List is ListWithOptions with no options.
func (c *Client) List(host, path, token string) (fetch.Result, error) {
	return c.ListWithOptions(host, path, token, fetch.ListOptions{})
}

// ListWithOptions records and answers a LIST.
func (c *Client) ListWithOptions(host, path, token string, opts fetch.ListOptions) (fetch.Result, error) {
	c.mu.Lock()
	c.ListCalls = append(c.ListCalls, Call{host, path, token})
	c.ListOpts = append(c.ListOpts, opts)
	fn, optsFn := c.ListFn, c.ListOptsFn
	c.mu.Unlock()
	switch {
	case optsFn != nil:
		return optsFn(host, path, token, opts)
	case fn != nil:
		return fn(host, path, token)
	}
	return ListPage(path, ""), nil
}

// Versions records and answers a VERSIONS.
func (c *Client) Versions(host, path, token string) (fetch.Result, error) {
	c.mu.Lock()
	c.VersionsCalls = append(c.VersionsCalls, Call{host, path, token})
	fn := c.VersionsFn
	c.mu.Unlock()
	if fn == nil {
		return status(protocol.StatusOK, nil), nil
	}
	return fn(host, path, token)
}

// Lookup records and answers a LOOKUP.
func (c *Client) Lookup(host, scope, query, token string, opts fetch.LookupOptions) (fetch.Result, error) {
	return c.lookup(context.Background(), &LookupCall{host, scope, query, token, opts}, false)
}

// LookupContext is Lookup, failing first when ctx is done.
func (c *Client) LookupContext(ctx context.Context, host, scope, query, token string, opts fetch.LookupOptions) (fetch.Result, error) {
	if err := ctx.Err(); err != nil {
		return fetch.Result{}, err
	}
	return c.lookup(ctx, &LookupCall{host, scope, query, token, opts}, true)
}

func (c *Client) lookup(ctx context.Context, call *LookupCall, contextAware bool) (fetch.Result, error) {
	c.mu.Lock()
	c.LookupCalls = append(c.LookupCalls, *call)
	fn, ctxFn := c.LookupFn, c.LookupCtxFn
	c.mu.Unlock()
	switch {
	case contextAware && ctxFn != nil:
		return ctxFn(ctx, call.Host, call.Scope, call.Query, call.Token, call.Opts)
	case fn != nil:
		return fn(call.Host, call.Scope, call.Query, call.Token, call.Opts)
	}
	return Lookup(call.Query, call.Scope, ""), nil
}

// Publish records and answers a PUBLISH.
func (c *Client) Publish(host, path, body, token string, expectedVersion int, meta map[string]string) (fetch.Result, error) {
	c.mu.Lock()
	c.PublishCalls = append(c.PublishCalls, WriteCall{host, path, body, token, expectedVersion, maps.Clone(meta)})
	fn := c.PublishFn
	c.mu.Unlock()
	result := status(protocol.StatusOK, map[string]string{"version": "1"})
	if fn != nil {
		var err error
		if result, err = fn(host, path, body, token, expectedVersion, meta); err != nil {
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
	return c.store(host, path, body, storeRequest{expected: expectedVersion, version: version, meta: meta, result: result})
}

type storeRequest struct {
	expected, version int
	meta              map[string]string
	result            fetch.Result
}

// store keeps a published body as the current and the numbered version.
func (c *Client) store(host, path, body string, req storeRequest) (fetch.Result, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	current := 0
	if existing, ok := c.Published[host+path]; ok {
		var err error
		if current, err = strconv.Atoi(existing.Response.Metadata["version"]); err != nil {
			return fetch.Result{}, fmt.Errorf("invalid stored version: %w", err)
		}
	}
	if req.expected >= 0 && req.expected != current {
		return status(protocol.StatusConflict, nil), nil
	}
	// As the server does: publisher keys survive, server owned keys are its own.
	metadata := make(map[string]string, len(req.meta)+2)
	for key, value := range req.meta {
		if !protocol.IsReservedMetadataKey(key) {
			metadata[key] = value
		}
	}
	metadata["version"] = strconv.Itoa(req.version)
	metadata["content-hash"] = index.BodyHash(body)
	stored := fetch.Result{Response: protocol.Response{Status: protocol.StatusOK, Body: body, Metadata: metadata}}
	if c.Published == nil {
		c.Published = make(map[string]fetch.Result)
	}
	c.Published[host+path] = stored
	c.Published[host+index.VersionPath(path, req.version)] = stored
	return req.result, nil
}

// PublishContext is Publish, failing first when ctx is done.
func (c *Client) PublishContext(ctx context.Context, host, path, body, token string, expectedVersion int, meta map[string]string) (fetch.Result, error) {
	if err := ctx.Err(); err != nil {
		return fetch.Result{}, err
	}
	return c.Publish(host, path, body, token, expectedVersion, meta)
}

// Append records and answers a APPEND.
func (c *Client) Append(host, path, body, token string, expectedVersion int, meta map[string]string) (fetch.Result, error) {
	c.mu.Lock()
	c.AppendCalls = append(c.AppendCalls, WriteCall{host, path, body, token, expectedVersion, maps.Clone(meta)})
	fn := c.AppendFn
	c.mu.Unlock()
	if fn == nil {
		return status(protocol.StatusOK, map[string]string{"version": "2"}), nil
	}
	return fn(host, path, body, token, expectedVersion, meta)
}

// Archive records and answers a ARCHIVE.
func (c *Client) Archive(host, path, token string) (fetch.Result, error) {
	c.mu.Lock()
	c.ArchiveCalls = append(c.ArchiveCalls, Call{host, path, token})
	fn := c.ArchiveFn
	c.mu.Unlock()
	if fn == nil {
		return status(protocol.StatusArchived, map[string]string{"version": "3"}), nil
	}
	return fn(host, path, token)
}

// FetchCallCount is the number of FETCHes so far, safe during concurrent use.
func (c *Client) FetchCallCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.FetchCalls)
}

// Calls is a copy of every recorded call.
type Calls struct {
	Fetch, List, Versions, Archive []Call
	FetchCond                      []CondCall
	Lookup                         []LookupCall
	Publish, Append                []WriteCall
}

// Calls copies the record under the lock, for reads that race the client.
func (c *Client) Calls() Calls {
	c.mu.Lock()
	defer c.mu.Unlock()
	return Calls{
		Fetch:     slices.Clone(c.FetchCalls),
		List:      slices.Clone(c.ListCalls),
		Versions:  slices.Clone(c.VersionsCalls),
		Archive:   slices.Clone(c.ArchiveCalls),
		FetchCond: slices.Clone(c.FetchCondCalls),
		Lookup:    slices.Clone(c.LookupCalls),
		Publish:   slices.Clone(c.PublishCalls),
		Append:    slices.Clone(c.AppendCalls),
	}
}

// Lock guards direct reads of the call record while the client is in use.
func (c *Client) Lock() { c.mu.Lock() }

// Unlock releases Lock.
func (c *Client) Unlock() { c.mu.Unlock() }
