package storetest

import (
	"bytes"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/latebit-io/demarkus/protocol"
)

// WireGoldenTime replaces every wall-clock value in a golden, so the fixture
// still parses as a real response.
const WireGoldenTime = "2026-01-01T00:00:00Z"

// WireGolden is one named response of the fixed wire scenario.
type WireGolden struct {
	Name     string
	Response protocol.Response
}

var wireTimestamp = regexp.MustCompile(`\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(\.\d+)?Z`)

// WireScenario drives a fixed request sequence through a handler over b and
// returns each response with wall-clock values pinned to WireGoldenTime.
func WireScenario(t *testing.T, b LookupBackend) []WireGolden {
	t.Helper()
	h := NewHandler(b)
	meta := map[string]string{"tags": "alpha, wire", "importance": "0.7", "type": "Note"}
	// A step without a name only prepares state.
	steps := []struct {
		name string
		req  protocol.Request
	}{
		{"publish-created", request(protocol.VerbPublish, "/docs/a.md", withExpected(meta, "0"), "# Alpha\n\nFirst body.\n")},
		{"publish-updated", request(protocol.VerbPublish, "/docs/a.md", withExpected(meta, "1"), "# Alpha\n\nSecond body.\n\n## Details\n\nSee [beta](b.md).\n")},
		{"publish-conflict", request(protocol.VerbPublish, "/docs/a.md", withExpected(meta, "1"), "# Alpha\n\nStale.\n")},
		{"append", request(protocol.VerbAppend, "/docs/a.md", map[string]string{"expected-version": "2"}, "\n## Appended\n\nMore.\n")},
		{"", request(protocol.VerbPublish, "/docs/b.md", withExpected(map[string]string{"tags": "beta", "importance": "0.4"}, "0"), "# Beta | piped\n\nBody of beta.\n")},
		{"", request(protocol.VerbPublish, "/docs/sub/c d.md", withExpected(nil, "0"), "# Gamma\n")},
		{"fetch", request(protocol.VerbFetch, "/docs/a.md", nil, "")},
		{"fetch-version", request(protocol.VerbFetch, "/docs/a.md/v1", nil, "")},
		{"fetch-not-found", request(protocol.VerbFetch, "/docs/missing.md", nil, "")},
		{"list", request(protocol.VerbList, "/docs", nil, "")},
		{"list-first-page", request(protocol.VerbList, "/docs", map[string]string{"page-size": "1"}, "")},
		{"list-not-found", request(protocol.VerbList, "/nowhere", nil, "")},
		{"versions", request(protocol.VerbVersions, "/docs/a.md", nil, "")},
		{"lookup", request(protocol.VerbLookup, "/", map[string]string{"query": "alpha beta"}, "")},
		{"lookup-body", request(protocol.VerbLookup, "/", map[string]string{"query": "appended", "match": "body"}, "")},
		{"lookup-empty", request(protocol.VerbLookup, "/", map[string]string{"query": "zzznothing"}, "")},
		{"archive", request(protocol.VerbArchive, "/docs/b.md", nil, "")},
		{"fetch-archived", request(protocol.VerbFetch, "/docs/b.md", nil, "")},
	}
	goldens := make([]WireGolden, 0, len(steps))
	for _, step := range steps {
		resp := pinWallClock(Send(t, h, step.req))
		if step.name == "" {
			if resp.Status != protocol.StatusCreated {
				t.Fatalf("setup %s %s: status %s", step.req.Verb, step.req.Path, resp.Status)
			}
			continue
		}
		goldens = append(goldens, WireGolden{Name: step.name, Response: resp})
	}
	return goldens
}

func withExpected(meta map[string]string, version string) map[string]string {
	out := map[string]string{"expected-version": version}
	maps.Copy(out, meta)
	return out
}

func pinWallClock(resp protocol.Response) protocol.Response {
	for k, v := range resp.Metadata {
		resp.Metadata[k] = wireTimestamp.ReplaceAllString(v, WireGoldenTime)
	}
	resp.Body = wireTimestamp.ReplaceAllString(resp.Body, WireGoldenTime)
	return resp
}

// RunWireGoldens compares the scenario's responses with the goldens in dir,
// or rewrites them when update is set. Consumers read the same files.
func RunWireGoldens(t *testing.T, b LookupBackend, dir string, update bool) {
	t.Helper()
	for _, golden := range WireScenario(t, b) {
		var wire bytes.Buffer
		if _, err := golden.Response.WriteTo(&wire); err != nil {
			t.Fatalf("%s: encode response: %v", golden.Name, err)
		}
		file := filepath.Join(dir, golden.Name+".golden")
		if update {
			if err := os.WriteFile(file, wire.Bytes(), 0o600); err != nil {
				t.Fatalf("%s: write golden: %v", golden.Name, err)
			}
			continue
		}
		want, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("%s: read golden (regenerate with -update): %v", golden.Name, err)
		}
		if !bytes.Equal(wire.Bytes(), want) {
			t.Errorf("%s: wire response drifted from golden (regenerate with -update)\ngot:\n%s\nwant:\n%s", golden.Name, wire.Bytes(), want)
		}
	}
}
