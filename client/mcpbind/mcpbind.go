// Package mcpbind binds an mcp-go tool call to the shared tool bodies:
// arguments in, tool result out. demarkus-mcp and the broker gateway share
// it, so an argument is read and refused the same way on both surfaces.
package mcpbind

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/latebit-io/demarkus/client/lookupexpand"
	"github.com/latebit-io/demarkus/client/marktools"
	"github.com/latebit-io/demarkus/client/mcpfmt"
	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

// ClientName is the MCP client's self-reported name from the session on ctx,
// "unknown" without one: the agent identity key on every surface.
func ClientName(ctx context.Context) string {
	if session, ok := mcpserver.ClientSessionFromContext(ctx).(mcpserver.SessionWithClientInfo); ok {
		if name := session.GetClientInfo().Name; name != "" {
			return name
		}
	}
	return "unknown"
}

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

// metadata is a tool call's optional "metadata" object as the strings the
// store keeps; numbers in plain decimal. PUBLISH replaces the metadata map,
// so a mistyped argument is refused, never read as none.
func metadata(args map[string]any) (map[string]string, error) {
	raw, given := args["metadata"]
	if !given || raw == nil {
		return nil, nil
	}
	obj, ok := raw.(map[string]any)
	if !ok {
		return nil, errors.New("metadata must be an object of key/value pairs")
	}
	meta := make(map[string]string, len(obj))
	for k, v := range obj {
		switch x := v.(type) {
		case string:
			meta[k] = x
		case float64:
			meta[k] = strconv.FormatFloat(x, 'f', -1, 64)
		case bool:
			meta[k] = strconv.FormatBool(x)
		default:
			return nil, fmt.Errorf("metadata.%s must be a string, number or boolean", k)
		}
	}
	return meta, nil
}

// Fetch binds mark_fetch.
func Fetch(req *mcp.CallToolRequest) (marktools.FetchArgs, error) {
	url, err := Required(req, "url")
	if err != nil {
		return marktools.FetchArgs{}, err
	}
	return marktools.FetchArgs{URL: url, Force: req.GetBool("force", false)}, nil
}

// Explore binds mark_explore; the relations page is read only when asked for.
func Explore(req *mcp.CallToolRequest) (marktools.ExploreArgs, error) {
	url, err := Required(req, "url")
	if err != nil {
		return marktools.ExploreArgs{}, err
	}
	args := marktools.ExploreArgs{URL: url}
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
	return marktools.AppendArgs{
		URL: url, Body: body, ExpectedVersion: req.GetInt("expected_version", 0),
		Route: req.GetString("route", ""), Tags: req.GetString("tags", ""),
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

// Archive binds mark_archive's arguments.
func Archive(req *mcp.CallToolRequest) (marktools.ArchiveArgs, error) {
	url, err := URL(req)
	if err != nil {
		return marktools.ArchiveArgs{}, err
	}
	return marktools.ArchiveArgs{URL: url, Recursive: req.GetBool("recursive", false)}, nil
}

// Curate binds mark_curate's arguments.
func Curate(req *mcp.CallToolRequest) (marktools.CurateArgs, error) {
	url, err := URL(req)
	if err != nil {
		return marktools.CurateArgs{}, err
	}
	return marktools.CurateArgs{URL: url, Apply: req.GetBool("apply", false), Proposal: req.GetString("proposal", "")}, nil
}

// Promote binds mark_promote's arguments; a phase outside phases is refused.
func Promote(req *mcp.CallToolRequest, phases []string) (marktools.PromoteArgs, error) {
	url, err := URL(req)
	if err != nil {
		return marktools.PromoteArgs{}, err
	}
	phase, err := Required(req, "phase")
	if err != nil {
		return marktools.PromoteArgs{}, err
	}
	if !slices.Contains(phases, phase) {
		return marktools.PromoteArgs{}, fmt.Errorf("phase %q is not served here (%s)", phase, strings.Join(phases, ", "))
	}
	args := marktools.PromoteArgs{
		URL: url, Phase: phase, Body: req.GetString("body", ""), Source: req.GetString("source", ""),
		Target: req.GetString("target", ""), Via: req.GetString("via", ""), Stub: req.GetBool("stub", false), Packet: req.GetString("packet", ""),
	}
	if args.Metadata, err = metadata(req.GetArguments()); err != nil {
		return marktools.PromoteArgs{}, err
	}
	return args, nil
}

// Doctor binds mark_doctor's arguments.
func Doctor(req *mcp.CallToolRequest) (marktools.DoctorArgs, error) {
	url, err := URL(req)
	if err != nil {
		return marktools.DoctorArgs{}, err
	}
	return marktools.DoctorArgs{URL: url, Deep: req.GetBool("deep", false), Promotion: req.GetBool("promotion", false)}, nil
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
