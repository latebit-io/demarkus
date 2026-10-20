package mcpprovider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

// The test binary doubles as the provider when fakeEnv names a behavior.
const fakeEnv = "MCPPROVIDER_FAKE"

func TestMain(m *testing.M) {
	mode := os.Getenv(fakeEnv)
	if mode == "" {
		os.Exit(m.Run())
	}
	args := os.Args[1:]
	switch {
	case len(args) == 2 && args[0] == "mcp" && args[1] == "tools":
		fakeTools(mode)
	case len(args) == 3 && args[0] == "mcp" && args[1] == "call":
		fakeCall(args[2])
	default:
		fmt.Fprintln(os.Stderr, "fake provider: unexpected args", args)
		os.Exit(3)
	}
}

func fakeTools(mode string) {
	switch mode {
	case "garbage":
		fmt.Print("not json")
	case "bad-name":
		fmt.Print(`[{"name":"mark host","inputSchema":{"type":"object"}}]`)
	case "shadow":
		fmt.Print(`[{"name":"mark_fetch","inputSchema":{"type":"object"}}]`)
	case "slow":
		time.Sleep(2 * time.Second)
	default:
		tools := []mcp.Tool{
			mcp.NewTool("fake_echo", mcp.WithDescription("echoes"),
				mcp.WithString("text", mcp.Required(), mcp.Description("what to echo")),
				mcp.WithReadOnlyHintAnnotation(true)),
			mcp.NewTool("fake_fail", mcp.WithDescription("refuses")),
		}
		if err := json.NewEncoder(os.Stdout).Encode(tools); err != nil {
			os.Exit(3)
		}
	}
}

func fakeCall(name string) {
	in, err := io.ReadAll(os.Stdin)
	if err != nil {
		os.Exit(3)
	}
	switch name {
	case "fake_echo":
		var args map[string]any
		if err := json.Unmarshal(in, &args); err != nil {
			fmt.Fprintln(os.Stderr, "bad arguments:", err)
			os.Exit(3)
		}
		fmt.Fprintln(os.Stderr, "echoing")
		fmt.Printf("echo: %v\n", args["text"])
	case "fake_fail":
		fmt.Println("FAIL: refused")
		fmt.Fprintln(os.Stderr, "refused: no such thing")
		os.Exit(1)
	default:
		os.Exit(3)
	}
}

func load(t *testing.T, mode string, served map[string]bool) ([]mcpserver.ServerTool, error) {
	t.Helper()
	t.Setenv(fakeEnv, mode)
	return Load(context.Background(), os.Args[0], served)
}

func TestLoadBindsProvidedToolsWithTheirSchemas(t *testing.T) {
	bound, err := load(t, "ok", map[string]bool{"mark_fetch": true})
	if err != nil {
		t.Fatal(err)
	}
	if len(bound) != 2 || bound[0].Tool.Name != "fake_echo" || bound[1].Tool.Name != "fake_fail" {
		t.Fatalf("tools = %+v", bound)
	}
	echo := bound[0].Tool
	if ro := echo.Annotations.ReadOnlyHint; ro == nil || !*ro {
		t.Errorf("read-only hint lost in transit: %+v", echo.Annotations)
	}
	if _, ok := echo.InputSchema.Properties["text"]; !ok || echo.InputSchema.Required[0] != "text" {
		t.Errorf("schema lost in transit: %+v", echo.InputSchema)
	}

	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{"text": "hi"}
	res, err := bound[0].Handler(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError || len(res.Content) != 1 {
		t.Fatalf("result = %+v", res)
	}
	if text := res.Content[0].(mcp.TextContent).Text; text != "echo: hi" {
		t.Errorf("text = %q (trailing newline must go)", text)
	}

	res, err = bound[1].Handler(context.Background(), mcp.CallToolRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatalf("a non-zero exit is a tool error: %+v", res)
	}
	if text := res.Content[0].(mcp.TextContent).Text; text != "fake_fail: refused: no such thing" {
		t.Errorf("error text = %q (stderr, not the FAIL line)", text)
	}
}

func TestLoadRefusesWhatItCannotServe(t *testing.T) {
	tests := map[string]string{
		"garbage":  "decode tool list",
		"bad-name": "not a tool name",
		"shadow":   "already served",
	}
	for mode, want := range tests {
		t.Run(mode, func(t *testing.T) {
			_, err := load(t, mode, map[string]bool{"mark_fetch": true})
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("err = %v, want %q", err, want)
			}
		})
	}
}

func TestLoadFailsWhenTheProviderCannotRun(t *testing.T) {
	_, err := Load(context.Background(), "/nonexistent/provider", nil)
	if err == nil || !strings.Contains(err.Error(), "list tools") {
		t.Fatalf("err = %v", err)
	}
}

func TestRunReportsATimeoutNotAnExit(t *testing.T) {
	t.Setenv(fakeEnv, "slow")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := run(ctx, os.Args[0], nil, "mcp", "tools")
	var failed *toolError
	if err == nil || errors.As(err, &failed) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the deadline", err)
	}
}
