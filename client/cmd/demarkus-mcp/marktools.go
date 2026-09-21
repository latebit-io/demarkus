package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/latebit-io/demarkus/client/docwrite"
	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/fetchdedup"
	"github.com/latebit-io/demarkus/client/graphstore"
	"github.com/latebit-io/demarkus/client/links"
	"github.com/latebit-io/demarkus/client/marktools"
	"github.com/latebit-io/demarkus/protocol"
)

// bodies binds the shared tool bodies to this server: URLs resolve against
// -host, reads carry the token stored for their host, writes need one.
func (h *handler) bodies() (*marktools.Tools, error) {
	return marktools.New(h.client, marktools.Hooks{
		Resolve:   h.resolveTarget,
		ReadToken: func(_ context.Context, host string) string { return h.resolveToken(host) },
		Writer:    h.writer,
		Seen:      processSeen{h},
		Agent:     agentName,
		Graph:     h.graphScope,
		Now:       func() time.Time { return timeNow() },
	})
}

// resolveTarget resolves a tool url; an empty one is the -host server.
func (h *handler) resolveTarget(_ context.Context, raw string) (marktools.Target, error) {
	if raw == "" {
		if h.defaultHost == "" {
			return marktools.Target{}, marktools.Verbatim("no server specified: provide a URL or set -host")
		}
		target, err := h.resolveURL(protocol.WellKnownManifestPath)
		if err != nil {
			return marktools.Target{}, marktools.Verbatim(fmt.Sprintf("invalid host: %v", err))
		}
		return toolTarget(target), nil
	}
	target, err := h.resolveURL(raw)
	if err != nil {
		return marktools.Target{}, err
	}
	return toolTarget(target), nil
}

// Identity omits the default port (ADR 0005), so keys match seeded rows.
func toolTarget(t links.Target) marktools.Target {
	return marktools.Target{Host: t.DialHost(), Path: t.Path, NodeURL: t.NodeURL(), Authority: t.AuthorityURL()}
}

// graphScope is this server's one graph store, seeded from the target's host.
func (h *handler) graphScope(context.Context) (*marktools.GraphScope, error) {
	if h.graphStore == nil {
		return nil, errors.New("graph store not available")
	}
	return &marktools.GraphScope{
		Store:     h.graphStore,
		Fetch:     graphstore.NewFetchFunc(h.client, fetch.TokenResolverFunc(h.resolveToken)),
		Seed:      func(ctx context.Context, target marktools.Target) { h.seedGraph(ctx, target.Host) },
		EmptyHint: "Run mark_graph to populate the graph store.",
	}, nil
}

// writer refuses a write to a host with no token; this server sends a write once.
func (h *handler) writer(_ context.Context, target marktools.Target, verb string) (marktools.WriteFunc, error) {
	token := h.resolveToken(target.Host)
	if token == "" {
		return nil, fmt.Errorf("%s requires a token (-token flag, DEMARKUS_AUTH env var, or stored via 'demarkus token add')", verb)
	}
	return docwrite.SendOnce(token), nil
}

// run answers a tool call with a shared tool body.
func (h *handler) run(call func(*marktools.Tools) marktools.Result) (*mcp.CallToolResult, error) {
	tools, err := h.bodies()
	if err != nil {
		return mcp.NewToolResultError("internal: " + err.Error()), nil
	}
	result := call(tools)
	if result.IsError {
		return mcp.NewToolResultError(result.Text), nil
	}
	return mcp.NewToolResultText(result.Text), nil
}

// processSeen is this server's seen store: one agent per stdio process.
type processSeen struct{ h *handler }

func (s processSeen) Lookup(_ context.Context, key string) (fetchdedup.Doc, bool) {
	return s.h.seenLookup(key)
}

func (s processSeen) Record(_ context.Context, key string, d fetchdedup.Doc) { s.h.seenRecord(key, d) }
