package retrievalbench

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

type fakeToolPager struct {
	calls int
	page  func(int, mcp.Cursor) (*mcp.ListToolsResult, error)
}

func (p *fakeToolPager) ListToolsByPage(_ context.Context, req mcp.ListToolsRequest) (*mcp.ListToolsResult, error) {
	p.calls++
	return p.page(p.calls, req.Params.Cursor)
}

func TestToolPaginationBoundsAndCompletion(t *testing.T) {
	for _, tc := range []struct {
		name  string
		pages int
		err   bool
		page  func(int, mcp.Cursor) (*mcp.ListToolsResult, error)
	}{
		{"normal", 2, false, func(n int, cursor mcp.Cursor) (*mcp.ListToolsResult, error) {
			if n == 2 && cursor != "next" {
				return nil, fmt.Errorf("cursor not passed: %q", cursor)
			}
			r := &mcp.ListToolsResult{Tools: []mcp.Tool{mcp.NewTool(fmt.Sprint(n))}}
			if n == 1 {
				r.NextCursor = "next"
			}
			return r, nil
		}},
		{"cycle", 3, true, func(n int, _ mcp.Cursor) (*mcp.ListToolsResult, error) {
			r := &mcp.ListToolsResult{}
			r.NextCursor = mcp.Cursor(fmt.Sprint(n % 2))
			return r, nil
		}},
		{"endless-pages", 64, true, func(n int, _ mcp.Cursor) (*mcp.ListToolsResult, error) {
			r := &mcp.ListToolsResult{}
			r.NextCursor = mcp.Cursor(fmt.Sprint(n))
			return r, nil
		}},
		{"terminal-at-page-bound", 64, false, func(n int, _ mcp.Cursor) (*mcp.ListToolsResult, error) {
			r := &mcp.ListToolsResult{}
			if n < 64 {
				r.NextCursor = mcp.Cursor(fmt.Sprint(n))
			}
			return r, nil
		}},
		{"tool-bound", 1, true, func(int, mcp.Cursor) (*mcp.ListToolsResult, error) {
			return &mcp.ListToolsResult{Tools: make([]mcp.Tool, 4097)}, nil
		}},
		{"terminal-at-tool-bound", 1, false, func(int, mcp.Cursor) (*mcp.ListToolsResult, error) {
			return &mcp.ListToolsResult{Tools: make([]mcp.Tool, 4096)}, nil
		}},
		{"fetch-error", 1, true, func(int, mcp.Cursor) (*mcp.ListToolsResult, error) {
			return nil, errors.New("unavailable")
		}},
		{"nil-page", 1, true, func(int, mcp.Cursor) (*mcp.ListToolsResult, error) { return nil, nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pager := &fakeToolPager{page: tc.page}
			tools, err := listToolPages(t.Context(), pager)
			if (err != nil) != tc.err || pager.calls != tc.pages {
				t.Fatalf("calls=%d tools=%d error=%v", pager.calls, len(tools), err)
			}
			if tc.name == "normal" && (len(tools) != 2 || tools[0].Name != "1" || tools[1].Name != "2") {
				t.Fatalf("pages not accumulated: %v", tools)
			}
		})
	}
}
