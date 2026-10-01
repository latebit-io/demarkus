package marktools

import (
	"context"
	"errors"
	"fmt"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/index"
	"github.com/latebit-io/demarkus/client/mcpfmt"
	"github.com/latebit-io/demarkus/protocol"
)

// ResolveArgs are mark_resolve's arguments.
type ResolveArgs struct {
	Hash string // sha256-<hex>
	// Index is the url of a hash index, checked after the hash. Empty asks
	// the surface's HashSources, and is refused by a surface without them.
	Index string
}

// ResolveHash answers mark_resolve: find hash in an index, or ask every
// server the surface reads, then fetch it from the first that really holds
// that content.
func (t *Tools) ResolveHash(ctx context.Context, args ResolveArgs) Result {
	hash, ok := protocol.IsHashPath(args.Hash)
	if !ok {
		return failure("invalid hash format: expected sha256-<64 lowercase hex characters>")
	}
	if args.Index == "" {
		if t.hooks.HashSources == nil {
			return failure("index is required")
		}
		servers := t.hooks.HashSources(ctx)
		if res, absent := t.firstHolder(ctx, hash, servers); !absent {
			return res
		}
		return failure("hash %s is held by none of the %d servers asked", hash, len(servers))
	}
	indexAt, err := t.hooks.Resolve(ctx, args.Index)
	if err != nil {
		return failure("invalid index URL: %v", err)
	}
	read := func(path string) (fetch.Result, error) { return t.fetch(ctx, indexAt, path) }
	indexDoc, err := read(indexAt.Path)
	if err != nil {
		return t.failed(SiteResolveIndex, indexAt.Host, err)
	}
	if indexDoc.Response.Status != protocol.StatusOK {
		return failure("index fetch returned: %s", indexDoc.Response.Status)
	}
	// A v2 manifest fetches only the matching prefix shards; a legacy index is inline.
	matches, err := index.EntriesForHash(indexAt.Path, indexDoc.Response.Body, hash, func(shardPath string) (protocol.Response, error) {
		shard, err := read(shardPath)
		return shard.Response, err
	})
	if err != nil {
		return failure("invalid index: %v", err)
	}
	if len(matches) == 0 {
		return failure("hash %s not found in index", hash)
	}
	servers := make([]string, len(matches))
	for i := range matches {
		servers[i] = matches[i].Server
	}
	res, _ := t.firstHolder(ctx, hash, servers) // the index named them: absent is stale, not settled
	return res
}

// errNotHeld is a server's answer that it has no document with the hash.
var errNotHeld = errors.New(protocol.StatusNotFound)

// firstHolder fetches hash from the first server that really holds it.
// absent is whether every server said it holds no such document; any other
// failure leaves the answer inconclusive.
func (t *Tools) firstHolder(ctx context.Context, hash string, servers []string) (res Result, absent bool) {
	var lastErr error
	absent = true
	for _, server := range servers {
		result, err := t.fetchByHash(ctx, server, hash)
		if err == nil {
			return text(mcpfmt.Full(result, "version", "modified", "content-hash")), false
		}
		absent = absent && errors.Is(err, errNotHeld)
		lastErr = err
	}
	return failure("could not resolve hash from any server: %v", lastErr), absent
}

// fetchByHash reads hash from one candidate server and checks that the server
// returned that content; the error says why the candidate was passed over.
func (t *Tools) fetchByHash(ctx context.Context, server, hash string) (fetch.Result, error) {
	at, err := t.hooks.Resolve(ctx, server+"/")
	if err != nil {
		return fetch.Result{}, fmt.Errorf("invalid server URL %s: %v", server, err)
	}
	result, err := t.fetch(ctx, at, "/"+hash)
	if err != nil {
		return fetch.Result{}, fmt.Errorf("%s: %s", server, t.errText(SiteResolveCandidate, at.Host, err))
	}
	if result.Response.Status == protocol.StatusNotFound {
		return fetch.Result{}, fmt.Errorf("%s: %w", server, errNotHeld)
	}
	if result.Response.Status != protocol.StatusOK {
		return fetch.Result{}, fmt.Errorf("%s: %s", server, result.Response.Status)
	}
	if got := result.Response.Metadata["content-hash"]; got != hash {
		return fetch.Result{}, fmt.Errorf("%s: hash mismatch (got %s)", server, got)
	}
	return result, nil
}
