package marktools

import (
	"context"
	"fmt"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/index"
	"github.com/latebit-io/demarkus/client/mcpfmt"
	"github.com/latebit-io/demarkus/protocol"
)

// ResolveArgs are mark_resolve's arguments.
type ResolveArgs struct {
	Hash  string // sha256-<hex>
	Index string // url of a hash index; required, checked after the hash
}

// ResolveHash answers mark_resolve: find hash in an index, then fetch it from
// the first listed server that really holds that content.
func (t *Tools) ResolveHash(ctx context.Context, args ResolveArgs) Result {
	hash, ok := protocol.IsHashPath(args.Hash)
	if !ok {
		return failure("invalid hash format: expected sha256-<64 lowercase hex characters>")
	}
	if args.Index == "" {
		return failure("index is required")
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

	var lastErr error
	for _, m := range matches {
		result, err := t.fetchByHash(ctx, m.Server, hash)
		if err == nil {
			return text(mcpfmt.Full(result, "version", "modified", "content-hash"))
		}
		lastErr = err
	}
	return failure("could not resolve hash from any server: %v", lastErr)
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
	if result.Response.Status != protocol.StatusOK {
		return fetch.Result{}, fmt.Errorf("%s: %s", server, result.Response.Status)
	}
	if got := result.Response.Metadata["content-hash"]; got != hash {
		return fetch.Result{}, fmt.Errorf("%s: hash mismatch (got %s)", server, got)
	}
	return result, nil
}
