// Package mcpprovider adds tools an external binary provides to an MCP
// server. The contract: `<bin> mcp tools` prints the tool list as a JSON
// array; `<bin> mcp call <name>` reads the arguments as JSON on stdin and
// prints the result text, a non-zero exit being a tool error whose message
// is stderr. The server stays ignorant of what the tools do.
package mcpprovider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"maps"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

const (
	listTimeout = 30 * time.Second
	// A call may provision a server, which downloads binaries.
	callTimeout = 5 * time.Minute
	maxTools    = 32
)

var toolName = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

// Load lists the provider's tools and binds each to a call through bin.
// A tool named like one in served is refused, so a provider can never
// shadow the server's own surface.
func Load(ctx context.Context, bin string, served map[string]bool) ([]mcpserver.ServerTool, error) {
	ctx, cancel := context.WithTimeout(ctx, listTimeout)
	defer cancel()
	out, err := run(ctx, bin, nil, "mcp", "tools")
	if err != nil {
		return nil, fmt.Errorf("list tools: %w", err)
	}
	var tools []mcp.Tool
	if err := json.Unmarshal(out, &tools); err != nil {
		return nil, fmt.Errorf("decode tool list: %w", err)
	}
	if len(tools) > maxTools {
		return nil, fmt.Errorf("%d tools, cap %d", len(tools), maxTools)
	}
	taken := maps.Clone(served)
	if taken == nil {
		taken = map[string]bool{}
	}
	bound := make([]mcpserver.ServerTool, 0, len(tools))
	for i := range tools {
		name := tools[i].Name
		switch {
		case !toolName.MatchString(name):
			return nil, fmt.Errorf("tool name %q is not a tool name", name)
		case taken[name]:
			return nil, fmt.Errorf("tool %s is already served", name)
		}
		taken[name] = true
		bound = append(bound, mcpserver.ServerTool{Tool: tools[i], Handler: handler(bin, name)})
	}
	return bound, nil
}

// handler binds one provided tool to `<bin> mcp call <name>`.
func handler(bin, name string) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		input, err := json.Marshal(req.GetArguments())
		if err != nil {
			return nil, fmt.Errorf("%s: encode arguments: %w", name, err)
		}
		ctx, cancel := context.WithTimeout(ctx, callTimeout)
		defer cancel()
		out, err := run(ctx, bin, input, "mcp", "call", name)
		var failed *toolError
		switch {
		case errors.As(err, &failed):
			return mcp.NewToolResultError(name + ": " + failed.msg), nil
		case err != nil:
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		return mcp.NewToolResultText(strings.TrimRight(string(out), "\n")), nil
	}
}

// toolError is a provider exit status other than zero: the tool ran and
// refused, as distinct from a provider that could not run.
type toolError struct{ msg string }

func (e *toolError) Error() string { return e.msg }

// run executes the provider with stdin and returns its stdout. Stderr on
// success is relayed to the log so a provider's warning is never dropped.
func run(ctx context.Context, bin string, stdin []byte, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	if len(stdin) > 0 {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case ctx.Err() != nil: // the deadline killed it; not the tool's refusal
		return nil, fmt.Errorf("provider %s %s: %w", bin, strings.Join(args, " "), ctx.Err())
	case errors.As(err, &exit):
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, &toolError{msg: msg}
	case err != nil:
		return nil, fmt.Errorf("provider %s: %w", bin, err)
	}
	if warn := strings.TrimSpace(stderr.String()); warn != "" {
		log.Printf("tool provider %s %s: %s", bin, strings.Join(args, " "), warn)
	}
	return stdout.Bytes(), nil
}
