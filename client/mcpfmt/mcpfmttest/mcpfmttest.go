// Package mcpfmttest pins the tools/list contract of the MCP surfaces in
// golden files, so a changed name, parameter or description shows in review.
package mcpfmttest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

// ToolsGolden fails when tools differ from the golden file at path, or
// rewrites the file under UPDATE_GOLDEN=1, the knowledge module's convention.
// Tools sort by name: registration order is not part of the contract.
func ToolsGolden(t testing.TB, path string, tools []mcp.Tool) {
	t.Helper()
	got, err := render(tools)
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(path, got, 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden (create it with UPDATE_GOLDEN=1): %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s: %s\nthe served tool list changed: review it, then rerun with UPDATE_GOLDEN=1", path, firstDiff(want, got))
	}
}

// render writes the tools as the agent reads them: HTML characters such as
// <dir> stay literal, unlike the escaped wire bytes. mcp.Tool marshals
// itself with escaping, so the wire form is decoded and encoded again.
func render(tools []mcp.Tool) ([]byte, error) {
	sorted := slices.Clone(tools)
	slices.SortFunc(sorted, func(a, b mcp.Tool) int { return strings.Compare(a.Name, b.Name) })
	wire, err := json.Marshal(sorted)
	if err != nil {
		return nil, fmt.Errorf("marshal tool schemas: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(wire))
	dec.UseNumber()
	var plain any
	if err := dec.Decode(&plain); err != nil {
		return nil, fmt.Errorf("decode tool schemas: %w", err)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(plain); err != nil {
		return nil, fmt.Errorf("encode tool schemas: %w", err)
	}
	return buf.Bytes(), nil
}

func firstDiff(want, got []byte) string {
	wantLines := strings.Split(string(want), "\n")
	gotLines := strings.Split(string(got), "\n")
	for i := range max(len(wantLines), len(gotLines)) {
		w, g := lineAt(wantLines, i), lineAt(gotLines, i)
		if w != g {
			return fmt.Sprintf("line %d: golden %q, served %q", i+1, w, g)
		}
	}
	return "contents differ"
}

func lineAt(lines []string, i int) string {
	if i < len(lines) {
		return lines[i]
	}
	return "<end of file>"
}
