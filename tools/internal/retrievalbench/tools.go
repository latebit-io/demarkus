package retrievalbench

import (
	"context"
	"fmt"
	"strings"

	"github.com/latebit-io/demarkus/client/mdoutline"
	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
)

// ToolResult is the text an agent would see from one tool call.
type ToolResult struct {
	Text    string
	IsError bool
}

// Tools calls MCP tools by name. Transport failures are errors; tool-level
// failures (not-found, bad section) come back as IsError results.
type Tools interface {
	Call(ctx context.Context, name string, args map[string]any) (ToolResult, error)
}

// Session is one MCP server process: a fresh agent session per question, so
// the server's in-session fetch dedup cannot leak savings between questions.
type Session interface {
	Tools
	Close() error
}

// SessionOpener starts a new Session.
type SessionOpener func(ctx context.Context) (Session, error)

// StdioConfig describes how to spawn an MCP server over stdio.
type StdioConfig struct {
	Command string
	Args    []string
	Env     []string
}

type stdioSession struct {
	client *mcpclient.Client
}

// OpenStdioSession spawns cfg.Command and completes the MCP handshake.
func OpenStdioSession(ctx context.Context, cfg StdioConfig) (Session, error) {
	c, err := mcpclient.NewStdioMCPClient(cfg.Command, cfg.Env, cfg.Args...)
	if err != nil {
		return nil, fmt.Errorf("spawn %s: %w", cfg.Command, err)
	}
	req := mcp.InitializeRequest{}
	req.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	req.Params.ClientInfo = mcp.Implementation{Name: "demarkus-retrieval-bench", Version: "dev"}
	if _, err := c.Initialize(ctx, req); err != nil {
		closeErr := c.Close()
		return nil, fmt.Errorf("initialize %s: %w (close: %v)", cfg.Command, err, closeErr)
	}
	return &stdioSession{client: c}, nil
}

func (s *stdioSession) Call(ctx context.Context, name string, args map[string]any) (ToolResult, error) {
	req := mcp.CallToolRequest{}
	req.Params.Name = name
	req.Params.Arguments = args
	res, err := s.client.CallTool(ctx, req)
	if err != nil {
		return ToolResult{}, fmt.Errorf("call %s: %w", name, err)
	}
	var b strings.Builder
	for i, c := range res.Content {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(mcp.GetTextFromContent(c))
	}
	return ToolResult{Text: b.String(), IsError: res.IsError}, nil
}

func (s *stdioSession) Close() error {
	return s.client.Close()
}

// fetchResponse is a parsed mark_fetch result: the "key: value" header lines
// and the body after the first blank line.
type fetchResponse struct {
	Header map[string]string
	Body   string
}

func parseFetchResponse(text string) fetchResponse {
	header, body, _ := strings.Cut(text, "\n\n")
	out := fetchResponse{Header: map[string]string{}, Body: body}
	for line := range strings.SplitSeq(header, "\n") {
		if k, v, ok := strings.Cut(line, ": "); ok {
			out.Header[k] = v
		}
	}
	return out
}

func (r fetchResponse) isOutline() bool {
	return r.Header["mode"] == "outline"
}

// hasSection reports whether the body carries the heading anchor names.
func (r fetchResponse) hasSection(anchor string) bool {
	_, ok := mdoutline.Section(r.Body, anchor)
	return ok
}

// lookupRow is one table row's location: the document path and, in body
// mode, the section anchor after '#'.
type lookupRow struct {
	Path   string
	Anchor string
}

// URL is what an agent passes to mark_fetch next.
func (r lookupRow) URL() string {
	if r.Anchor == "" {
		return r.Path
	}
	return r.Path + "#" + r.Anchor
}

// parseLookupRows returns the rows of a mark_lookup table in rank order.
// Only rows whose first cell is an absolute path count.
func parseLookupRows(text string) []lookupRow {
	var rows []lookupRow
	for line := range strings.SplitSeq(text, "\n") {
		cells := strings.Split(line, "|")
		if len(cells) < 3 {
			continue
		}
		location := strings.TrimSpace(cells[1])
		if !strings.HasPrefix(location, "/") {
			continue
		}
		path, anchor, _ := strings.Cut(location, "#")
		rows = append(rows, lookupRow{Path: path, Anchor: anchor})
	}
	return rows
}
