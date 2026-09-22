// Package mcpbind binds an mcp-go tool call to the shared tool bodies:
// arguments in, tool result out. demarkus-mcp and the broker gateway share
// it, so an argument is read and refused the same way on both surfaces.
package mcpbind

import (
	"errors"

	"github.com/latebit-io/demarkus/client/lookupexpand"
	"github.com/latebit-io/demarkus/client/marktools"
	"github.com/latebit-io/demarkus/client/mcpfmt"
	"github.com/mark3labs/mcp-go/mcp"
)

// defaultGraphRetention bounds the published graph's version history: it is
// a generated artifact republished wholesale, and 20 versions is enough to
// debug a bad crawl.
const defaultGraphRetention = 20

// Required is a string argument the tool cannot run without; a missing or non
// string value is refused as "<name> is required".
func Required(req *mcp.CallToolRequest, name string) (string, error) {
	value, err := req.RequireString(name)
	if err != nil {
		return "", errors.New(name + " is required")
	}
	return value, nil
}

// URL is the required url argument.
func URL(req *mcp.CallToolRequest) (string, error) {
	return Required(req, "url")
}

// OptionalURL is the url argument, "" when absent: the body then reads it as
// the surface's default server, or refuses it.
func OptionalURL(req *mcp.CallToolRequest) string {
	return req.GetString("url", "")
}

// metadata is a tool call's optional "metadata" object. PUBLISH replaces the
// metadata map, so a mistyped argument is refused, never read as none.
func metadata(args map[string]any) (map[string]any, error) {
	raw, given := args["metadata"]
	if !given || raw == nil {
		return nil, nil
	}
	meta, ok := raw.(map[string]any)
	if !ok {
		return nil, errors.New("metadata must be an object of key/value pairs")
	}
	return meta, nil
}

// Fetch binds mark_fetch.
func Fetch(req *mcp.CallToolRequest) (marktools.FetchArgs, error) {
	url, err := Required(req, "url")
	if err != nil {
		return marktools.FetchArgs{}, err
	}
	return marktools.FetchArgs{URL: url, Force: req.GetBool("force", false), Render: mcpfmt.Fetch.Options(req)}, nil
}

// Explore binds mark_explore; the relations page is read only when asked for.
func Explore(req *mcp.CallToolRequest) (marktools.ExploreArgs, error) {
	url, err := Required(req, "url")
	if err != nil {
		return marktools.ExploreArgs{}, err
	}
	args := marktools.ExploreArgs{URL: url, Render: mcpfmt.Fetch.Options(req)}
	if mcpfmt.NeighborhoodRequested(req) {
		args.Relations = &marktools.RelationsArgs{
			Options:    mcpfmt.NeighborhoodOptions(req),
			Revalidate: mcpfmt.RevalidatesBacklinks(req),
		}
	}
	return args, nil
}

// List binds mark_list. page_size stays the raw value; the body validates it.
func List(req *mcp.CallToolRequest) (marktools.ListArgs, error) {
	url, err := Required(req, "url")
	if err != nil {
		return marktools.ListArgs{}, err
	}
	return marktools.ListArgs{
		URL:             url,
		IncludeArchived: req.GetBool("include_archived", false),
		Cursor:          req.GetString("cursor", ""),
		PageSize:        req.GetArguments()["page_size"],
	}, nil
}

// Lookup binds mark_lookup.
func Lookup(req *mcp.CallToolRequest) (marktools.LookupArgs, error) {
	url, err := Required(req, "url")
	if err != nil {
		return marktools.LookupArgs{}, err
	}
	query, err := Required(req, "query")
	if err != nil {
		return marktools.LookupArgs{}, err
	}
	return marktools.LookupArgs{
		URL:    url,
		Query:  query,
		Filter: req.GetString("filter", ""),
		Limit:  req.GetInt("limit", 0),
		Match:  req.GetString("match", ""),
		Budget: lookupexpand.Budget(req),
		Render: mcpfmt.Lookup.Options(req),
	}, nil
}

// Publish binds mark_publish. A missing or mistyped expected_version stays
// nil and the body refuses it; a metadata value that is not an object is
// refused here, before anything is sent.
func Publish(req *mcp.CallToolRequest) (marktools.PublishArgs, error) {
	url, err := Required(req, "url")
	if err != nil {
		return marktools.PublishArgs{}, err
	}
	body, err := Required(req, "body")
	if err != nil {
		return marktools.PublishArgs{}, err
	}
	args := marktools.PublishArgs{URL: url, Body: body, OnConflict: req.GetString("on_conflict", "")}
	if version, err := req.RequireInt("expected_version"); err == nil {
		args.ExpectedVersion = &version
	}
	if args.Metadata, err = metadata(req.GetArguments()); err != nil {
		return marktools.PublishArgs{}, err
	}
	return args, nil
}

// Append binds mark_append; an absent expected_version is 0, resolved by the body.
func Append(req *mcp.CallToolRequest) (marktools.AppendArgs, error) {
	url, err := Required(req, "url")
	if err != nil {
		return marktools.AppendArgs{}, err
	}
	body, err := Required(req, "body")
	if err != nil {
		return marktools.AppendArgs{}, err
	}
	return marktools.AppendArgs{URL: url, Body: body, ExpectedVersion: req.GetInt("expected_version", 0)}, nil
}

// Resolve binds mark_resolve; the index is checked by the body, after the hash.
func Resolve(req *mcp.CallToolRequest) (marktools.ResolveArgs, error) {
	hash, err := Required(req, "hash")
	if err != nil {
		return marktools.ResolveArgs{}, err
	}
	return marktools.ResolveArgs{Hash: hash, Index: req.GetString("index", "")}, nil
}

// Index binds mark_index.
func Index(req *mcp.CallToolRequest) (marktools.IndexArgs, error) {
	source, err := Required(req, "source")
	if err != nil {
		return marktools.IndexArgs{}, err
	}
	target, err := Required(req, "target")
	if err != nil {
		return marktools.IndexArgs{}, err
	}
	return marktools.IndexArgs{
		Source: source, Target: target,
		DryRun:          req.GetBool("dry_run", false),
		Force:           req.GetBool("force", false),
		ExpectedVersion: req.GetInt("expected_version", 0),
	}, nil
}

// Graph binds mark_graph; the depth is the body's default when absent, and
// an explicit 0 is the shallowest crawl, not the default.
func Graph(req *mcp.CallToolRequest) (marktools.GraphArgs, error) {
	url, err := Required(req, "url")
	if err != nil {
		return marktools.GraphArgs{}, err
	}
	depth := req.GetInt("depth", marktools.DefaultGraphDepth)
	return marktools.GraphArgs{URL: url, Depth: max(1, depth)}, nil
}

// GraphPublish binds mark_graph_publish; the url is read as given and the
// body refuses an empty one.
func GraphPublish(req *mcp.CallToolRequest) marktools.GraphPublishArgs {
	args := marktools.GraphPublishArgs{URL: OptionalURL(req), Retention: req.GetInt("retention", defaultGraphRetention)}
	if version, err := req.RequireInt("expected_version"); err == nil {
		args.ExpectedVersion = &version
	}
	return args
}

// Refused is the tool result for an argument the surface refused.
func Refused(err error) *mcp.CallToolResult {
	return mcp.NewToolResultError(err.Error())
}

// Result is the tool result for a shared body's answer.
func Result(r marktools.Result) *mcp.CallToolResult {
	if r.IsError {
		return mcp.NewToolResultError(r.Text)
	}
	return mcp.NewToolResultText(r.Text)
}
