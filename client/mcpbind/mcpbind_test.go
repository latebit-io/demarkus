package mcpbind

import (
	"reflect"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/client/marktools"
	"github.com/mark3labs/mcp-go/mcp"
)

func call(args map[string]any) *mcp.CallToolRequest {
	return &mcp.CallToolRequest{Params: mcp.CallToolParams{Arguments: args}}
}

func text(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if len(res.Content) != 1 {
		t.Fatalf("content = %+v, want one item", res.Content)
	}
	tc, ok := res.Content[0].(mcp.TextContent)
	if !ok {
		t.Fatalf("content[0] = %T, want text", res.Content[0])
	}
	return tc.Text
}

// Every binding refuses its required arguments by name, in argument order.
func TestRequiredArgumentsAreNamed(t *testing.T) {
	tests := []struct {
		name string
		bind func(*mcp.CallToolRequest) error
		args map[string]any
		want string
	}{
		{"url", func(r *mcp.CallToolRequest) error { _, err := URL(r); return err }, nil, "url is required"},
		{"fetch", func(r *mcp.CallToolRequest) error { _, err := Fetch(r); return err }, nil, "url is required"},
		{"explore", func(r *mcp.CallToolRequest) error { _, err := Explore(r); return err }, nil, "url is required"},
		{"list", func(r *mcp.CallToolRequest) error { _, err := List(r); return err }, nil, "url is required"},
		{"lookup url", func(r *mcp.CallToolRequest) error { _, err := Lookup(r); return err }, map[string]any{"query": "q"}, "url is required"},
		{"lookup query", func(r *mcp.CallToolRequest) error { _, err := Lookup(r); return err }, map[string]any{"url": "/"}, "query is required"},
		{"publish url", func(r *mcp.CallToolRequest) error { _, err := Publish(r); return err }, map[string]any{"body": "b"}, "url is required"},
		{"publish body", func(r *mcp.CallToolRequest) error { _, err := Publish(r); return err }, map[string]any{"url": "/x"}, "body is required"},
		{"append body", func(r *mcp.CallToolRequest) error { _, err := Append(r); return err }, map[string]any{"url": "/x"}, "body is required"},
		{"resolve", func(r *mcp.CallToolRequest) error { _, err := Resolve(r); return err }, nil, "hash is required"},
		{"index source", func(r *mcp.CallToolRequest) error { _, err := Index(r); return err }, map[string]any{"target": "/t"}, "source is required"},
		{"index target", func(r *mcp.CallToolRequest) error { _, err := Index(r); return err }, map[string]any{"source": "/s"}, "target is required"},
		{"graph", func(r *mcp.CallToolRequest) error { _, err := Graph(r); return err }, map[string]any{"url": 3}, "url is required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.bind(call(tt.args))
			if err == nil || err.Error() != tt.want {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
			if got := text(t, Refused(err)); got != tt.want {
				t.Errorf("Refused = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPublishVersionAndMetadata(t *testing.T) {
	args, err := Publish(call(map[string]any{"url": "/d.md", "body": "b", "expected_version": float64(3),
		"on_conflict": "fail", "metadata": map[string]any{"tags": "go", "importance": 0.9}}))
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if args.ExpectedVersion == nil || *args.ExpectedVersion != 3 || args.OnConflict != "fail" {
		t.Errorf("args = %+v, want version 3 and fail mode", args)
	}
	if args.Metadata["tags"] != "go" || args.Metadata["importance"] != 0.9 {
		t.Errorf("metadata = %v", args.Metadata)
	}

	// A missing or mistyped version stays nil for the body to refuse.
	for _, version := range []any{nil, "three"} {
		a := map[string]any{"url": "/d.md", "body": "b"}
		if version != nil {
			a["expected_version"] = version
		}
		args, err := Publish(call(a))
		if err != nil || args.ExpectedVersion != nil {
			t.Errorf("expected_version=%v: args = %+v, err = %v; want nil version", version, args, err)
		}
	}

	_, err = Publish(call(map[string]any{"url": "/d.md", "body": "b", "expected_version": float64(1), "metadata": "tags: go"}))
	if err == nil || !strings.Contains(err.Error(), "metadata must be an object") {
		t.Errorf("non object metadata err = %v", err)
	}
}

func TestDefaults(t *testing.T) {
	list, err := List(call(map[string]any{"url": "/", "page_size": float64(7)}))
	if err != nil || list.IncludeArchived || list.Cursor != "" || list.PageSize != float64(7) {
		t.Errorf("List = %+v, %v", list, err)
	}
	appendArgs, err := Append(call(map[string]any{"url": "/d.md", "body": "b"}))
	if err != nil || appendArgs.ExpectedVersion != 0 {
		t.Errorf("Append = %+v, %v; want version 0 for the body to resolve", appendArgs, err)
	}
	graph, err := Graph(call(map[string]any{"url": "/i.md"}))
	if err != nil || graph.Depth != marktools.DefaultGraphDepth {
		t.Errorf("Graph = %+v, %v; want depth %d", graph, err, marktools.DefaultGraphDepth)
	}
	graph, err = Graph(call(map[string]any{"url": "/i.md", "depth": float64(0)}))
	if err != nil || graph.Depth != 1 {
		t.Errorf("Graph = %+v, %v; an explicit 0 is the shallowest crawl on both surfaces", graph, err)
	}
	publish := GraphPublish(call(nil))
	if publish.URL != "" || publish.Retention != defaultGraphRetention || publish.ExpectedVersion != nil {
		t.Errorf("GraphPublish = %+v; want empty url, default retention, nil version", publish)
	}
	publish = GraphPublish(call(map[string]any{"url": "/g.md", "retention": float64(5), "expected_version": float64(2)}))
	if publish.URL != "/g.md" || publish.Retention != 5 || publish.ExpectedVersion == nil || *publish.ExpectedVersion != 2 {
		t.Errorf("GraphPublish = %+v", publish)
	}
	explore, err := Explore(call(map[string]any{"url": "/d.md"}))
	if err != nil || explore.Relations != nil {
		t.Errorf("Explore = %+v, %v; want no relations page unless asked", explore, err)
	}
	lookup, err := Lookup(call(map[string]any{"url": "/", "query": "q", "limit": float64(4), "match": "body", "budget": float64(1)}))
	if err != nil || lookup.Limit != 4 || lookup.Match != "body" || lookup.Budget <= 0 {
		t.Errorf("Lookup = %+v, %v", lookup, err)
	}
}

func TestResultMapsErrorAndText(t *testing.T) {
	if res := Result(marktools.Result{Text: "ok"}); res.IsError || text(t, res) != "ok" {
		t.Errorf("Result(ok) = %+v", res)
	}
	if res := Result(marktools.Result{Text: "no", IsError: true}); !res.IsError || text(t, res) != "no" {
		t.Errorf("Result(error) = %+v", res)
	}
}

func TestMetadataArgument(t *testing.T) {
	object := map[string]any{"tags": "go"}
	tests := []struct {
		name    string
		args    map[string]any
		want    map[string]any
		wantErr bool
	}{
		{"absent", map[string]any{}, nil, false},
		{"null", map[string]any{"metadata": nil}, nil, false},
		{"object", map[string]any{"metadata": object}, object, false},
		{"string", map[string]any{"metadata": "tags: go"}, nil, true},
		{"array", map[string]any{"metadata": []any{"go"}}, nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := metadata(tt.args)
			if (err != nil) != tt.wantErr || !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("metadata = %v, %v; want %v, error=%v", got, err, tt.want, tt.wantErr)
			}
		})
	}
}
