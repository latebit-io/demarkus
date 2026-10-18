// Command evalmocks renders the memory plugin eval's MCP mocks from the real
// code: the tool list from the demarkus-mcp lean golden, and each canned
// response by marktools over a fixture store that answers with the server's
// wire goldens. A mock then drifts only when the code does; check fails on a
// stale, unlisted or hand edited file.
//
// Usage: evalmocks [check|write]
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/latebit-io/demarkus/tools/internal/reporoot"
)

const (
	evalsDir   = "tools/demarkus-plugin/evals"
	toolsGold  = "client/cmd/demarkus-mcp/testdata/tools-host-lean.json"
	mocksLeaf  = "mocks/demarkus-memory"
	runnerFile = "run.sh"
)

// handWritten are the mocks no fixture renders yet: doctor and curate reports
// over a store large enough to show their findings.
var handWritten = []string{
	"doctor-fixable/" + mocksLeaf + "/mark_doctor.md",
	"curate-scope-gate/" + mocksLeaf + "/mark_curate.md",
}

// artifact is one generated file under the evals directory.
type artifact struct {
	Rel     string // slash path under evalsDir
	Content []byte
}

func main() {
	mode := "check"
	if len(os.Args) > 1 {
		mode = os.Args[1]
	}
	if len(os.Args) > 2 || (mode != "check" && mode != "write") {
		fmt.Fprintln(os.Stderr, "usage: evalmocks [check|write]")
		os.Exit(2)
	}
	n, err := run(mode)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("eval mocks %s: %d artifacts\n", mode, n)
}

func run(mode string) (int, error) {
	root, err := reporoot.Find(filepath.Join(evalsDir, runnerFile))
	if err != nil {
		return 0, err
	}
	artifacts, err := renderAll(root)
	if err != nil {
		return 0, err
	}
	if mode == "write" {
		err = writeAll(root, artifacts)
	} else {
		err = checkAll(root, artifacts)
	}
	return len(artifacts), err
}

// mockPath is the file a mock's slash path names.
func mockPath(root, rel string) string {
	return filepath.Join(root, evalsDir, filepath.FromSlash(rel))
}

// renderAll renders the tool list and every mock in the table.
func renderAll(root string) ([]artifact, error) {
	tools, err := renderTools(root)
	if err != nil {
		return nil, err
	}
	artifacts := []artifact{{Rel: mocksLeaf + "/_tools.json", Content: tools}}
	for i := range mocks {
		content, err := mocks[i].render()
		if err != nil {
			return nil, fmt.Errorf("render %s: %w", mocks[i].Rel, err)
		}
		artifacts = append(artifacts, artifact{Rel: mocks[i].Rel, Content: content})
	}
	return artifacts, nil
}

// renderTools wraps the lean tools/list golden as the runner's saved result.
// The entries pass through untouched, so the golden's text and key order hold.
func renderTools(root string) ([]byte, error) {
	raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(toolsGold)))
	if err != nil {
		return nil, fmt.Errorf("read tools golden: %w", err)
	}
	var tools []json.RawMessage
	if err := json.Unmarshal(raw, &tools); err != nil {
		return nil, fmt.Errorf("decode tools golden: %w", err)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(map[string][]json.RawMessage{"tools": tools}); err != nil {
		return nil, fmt.Errorf("encode tools: %w", err)
	}
	return buf.Bytes(), nil
}

func writeAll(root string, artifacts []artifact) error {
	for i := range artifacts {
		path := mockPath(root, artifacts[i].Rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, artifacts[i].Content, 0o644); err != nil { //nolint:gosec // eval fixtures are read by the runner
			return fmt.Errorf("write %s: %w", path, err)
		}
	}
	return nil
}

// checkAll fails on a rendered mock that differs from its file and on a mock
// file the table does not know, so every mock is rendered or listed by hand.
func checkAll(root string, artifacts []artifact) error {
	var drift []string
	known := make(map[string]struct{}, len(artifacts)+len(handWritten))
	for i := range artifacts {
		known[artifacts[i].Rel] = struct{}{}
		actual, err := os.ReadFile(mockPath(root, artifacts[i].Rel))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("read %s: %w", artifacts[i].Rel, err)
		}
		if err != nil || !bytes.Equal(actual, artifacts[i].Content) {
			drift = append(drift, artifacts[i].Rel)
		}
	}
	for _, rel := range handWritten {
		known[rel] = struct{}{}
	}
	var unlisted []string
	dir := filepath.Join(root, evalsDir)
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if entry.IsDir() {
			if rel == "results" {
				return fs.SkipDir
			}
			return nil
		}
		if _, ok := known[rel]; !ok && strings.Contains("/"+rel, "/mocks/") {
			unlisted = append(unlisted, rel)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("scan eval mocks: %w", err)
	}
	var msgs []string
	if len(drift) > 0 {
		slices.Sort(drift)
		msgs = append(msgs, "stale eval mocks (run: cd tools && go run ./evalmocks write):\n  "+strings.Join(drift, "\n  "))
	}
	if len(unlisted) > 0 {
		slices.Sort(unlisted)
		msgs = append(msgs, "eval mocks neither rendered nor listed in handWritten (tools/evalmocks/main.go):\n  "+strings.Join(unlisted, "\n  "))
	}
	if len(msgs) == 0 {
		return nil
	}
	return errors.New(strings.Join(msgs, "\n"))
}
