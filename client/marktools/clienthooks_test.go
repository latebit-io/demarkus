package marktools_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/client/docwrite"
	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/graph"
	"github.com/latebit-io/demarkus/client/graphstore"
	"github.com/latebit-io/demarkus/client/links"
	"github.com/latebit-io/demarkus/client/marktools"
)

// clientSurface is a direct client's binding of the hooks, as demarkus-mcp
// wires them, for tests that were written against that server.
type clientSurface struct {
	DefaultHost string            // "mark://host:6309"; a bare path resolves against it
	Token       string            // sent on every read and write; empty refuses writes
	Agent       string            // "" writes as "unknown"
	Store       *graphstore.Store // nil: no graph store
	Now         time.Time         // zero: the real clock
	Seeded      []string          // hosts the graph hook was asked to seed
	seen        mapSeen
}

// tools binds backend the way the stdio server does.
func (c *clientSurface) tools(t *testing.T, backend marktools.Backend) *marktools.Tools {
	t.Helper()
	hooks := marktools.Hooks{
		Resolve:   c.resolve,
		ReadToken: func(context.Context, string) string { return c.Token },
		Seen:      &c.seen,
		Agent: func(context.Context) string {
			if c.Agent == "" {
				return "unknown"
			}
			return c.Agent
		},
		Writer: func(_ context.Context, _ marktools.Target, verb string) (marktools.WriteFunc, error) {
			if c.Token == "" {
				return nil, errors.New(verb + " requires a token (-token flag, DEMARKUS_AUTH env var, or stored via 'demarkus token add')")
			}
			return docwrite.SendOnce(c.Token), nil
		},
		Graph: func(context.Context) (*marktools.GraphScope, error) {
			if c.Store == nil {
				return nil, errors.New("graph store not available")
			}
			return &marktools.GraphScope{
				Store: c.Store,
				Fetch: func(ctx context.Context, target links.Target) (graph.FetchResult, error) {
					r, err := backend.Fetch(ctx, fetch.FetchRequest{Host: target.DialHost(), Path: target.Path, Token: c.Token})
					return graph.FetchResult{Status: r.Response.Status, Body: r.Response.Body, Metadata: r.Response.Metadata}, err
				},
				Seed:      func(_ context.Context, target marktools.Target) { c.Seeded = append(c.Seeded, target.Host) },
				EmptyHint: "Run mark_graph to populate the graph store.",
			}, nil
		},
	}
	if !c.Now.IsZero() {
		hooks.Now = func() time.Time { return c.Now }
	}
	return newTools(t, backend, hooks)
}

// resolve is the stdio server's rule: a bare path needs -host, identity omits
// the default port.
func (c *clientSurface) resolve(_ context.Context, raw string) (marktools.Target, error) {
	if raw == "" || raw[0] == '/' {
		if c.DefaultHost == "" {
			if raw == "" {
				return marktools.Target{}, marktools.Verbatim("no server specified: provide a URL or set -host")
			}
			return marktools.Target{}, errors.New("bare path \"" + raw + "\" requires -host flag")
		}
		raw = c.DefaultHost + raw
	}
	target, err := links.ParseMark(raw)
	if err != nil {
		return marktools.Target{}, err
	}
	host := target.DialHost()
	return marktools.Target{Host: host, Path: target.Path, NodeURL: target.NodeURL(), Authority: links.AuthorityURL(host)}, nil
}
