package marktools

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/lookupexpand"
	"github.com/latebit-io/demarkus/client/mcpfmt"
	"github.com/latebit-io/demarkus/protocol"
)

// ListArgs are mark_list's arguments as the surface read them.
type ListArgs struct {
	URL             string
	IncludeArchived bool
	Cursor          string
	PageSize        any // the raw JSON value; nil when the argument is absent
}

// List answers mark_list: one page of a directory listing.
func (t *Tools) List(ctx context.Context, args ListArgs) Result {
	target, bad := t.resolve(ctx, args.URL)
	if bad != nil {
		return *bad
	}
	pageSize, err := listPageSize(args.PageSize)
	if err != nil {
		return failure("%v", err)
	}
	list := fetch.ListRequest{
		Host: target.Host, Path: target.Path, Token: t.readToken(ctx, target.Host),
		IncludeArchived: args.IncludeArchived,
		Cursor:          args.Cursor,
		PageSize:        pageSize,
	}
	result, err := t.backend.List(ctx, list)
	if err != nil {
		return t.failed(SiteList, target.Host, err)
	}
	// An agent that follows a cursor which does not move would loop forever.
	if result.Response.Metadata["complete"] == "false" {
		if next := result.Response.Metadata["next-cursor"]; next == "" || next == list.Cursor {
			return failure("list failed: continuation cursor is missing or did not advance")
		}
	}
	return text(mcpfmt.Full(result, "modified"))
}

// listPageSize reads page_size from its JSON value: a whole number in range.
func listPageSize(raw any) (int, error) {
	var size int
	switch value := raw.(type) {
	case nil:
		return 0, nil
	case int:
		size = value
	case float64:
		if value != math.Trunc(value) {
			return 0, errors.New("page_size must be an integer")
		}
		size = int(value)
	default:
		return 0, errors.New("page_size must be an integer")
	}
	if size < 1 || size > protocol.MaxListPageSize {
		return 0, fmt.Errorf("page_size must be between 1 and %d", protocol.MaxListPageSize)
	}
	return size, nil
}

// LookupArgs are mark_lookup's arguments. Budget above zero appends the matched
// sections, in rank order, until that many result tokens are spent.
type LookupArgs struct {
	URL, Query    string
	Filter, Match string
	Limit, Budget int
	Render        mcpfmt.Options
}

// Lookup answers mark_lookup: the ranked catalog table, then any expansion.
func (t *Tools) Lookup(ctx context.Context, args LookupArgs) Result {
	target, bad := t.resolve(ctx, args.URL)
	if bad != nil {
		return *bad
	}
	lookup := fetch.LookupRequest{
		Host: target.Host, Scope: target.Path, Token: t.readToken(ctx, target.Host),
		Query: args.Query, Filter: args.Filter, Limit: args.Limit, Match: args.Match,
	}
	result, err := t.backend.Lookup(ctx, lookup)
	if err != nil {
		return t.failed(SiteLookup, target.Host, err)
	}
	out := mcpfmt.Format(result, args.Render) + mcpfmt.CatalogFallback(lookup, result)
	if args.Budget > 0 && result.Response.Status == protocol.StatusOK {
		out += lookupexpand.Expand(ctx, result.Response.Body, args.Query, args.Budget, func(ctx context.Context, path string) (string, error) {
			return FetchBody(t.backend.Fetch(ctx, fetch.FetchRequest{Host: target.Host, Path: path, Token: lookup.Token}))
		})
	}
	return text(out)
}

// FetchBody adapts a fetch result to lookupexpand's body or error contract.
func FetchBody(r fetch.Result, err error) (string, error) {
	if err != nil {
		return "", err
	}
	if r.Response.Status != protocol.StatusOK {
		return "", errors.New(r.Response.Status)
	}
	return r.Response.Body, nil
}

// Discover answers mark_discover: the server's agent manifest, whatever
// document the URL names. An empty URL is the surface's default server.
func (t *Tools) Discover(ctx context.Context, rawURL string) Result {
	target, bad := t.resolve(ctx, rawURL)
	if bad != nil {
		return *bad
	}
	result, err := t.fetch(ctx, target, protocol.WellKnownManifestPath)
	if err != nil {
		return t.failed(SiteDiscover, target.Host, err)
	}
	return text(mcpfmt.Full(result, "version", "modified"))
}
