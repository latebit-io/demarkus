package main

import (
	"fmt"
	"iter"
	"slices"
	"strings"
)

// allowed names, for each target layer, the layers its packages may import
// besides their own: dependencies point down only. Changing a row changes the
// architecture and takes an ADR.
var allowed = map[string][]string{
	// the wire format alone: requests, responses, verbs, statuses
	"wire": nil,
	// what both sides share above the wire: stored format, policy, contracts
	"protocol": {"wire"},
	// server side of the memory store; never imports the client (ADR 0026)
	"store": {"wire", "protocol"},
	// the store client, the SDK to be
	"storeclient": {"wire", "protocol"},
	// document kinds, hubs, write gates, journal, promote, doctor
	"model": {"wire", "protocol", "storeclient"},
	// the two tiers are siblings: neither imports the other
	"soul":      {"wire", "protocol", "store", "storeclient", "model"},
	"knowledge": {"wire", "protocol", "store", "storeclient", "model"},
	// MCP tools, plugins, gateway, CLI, TUI and the binaries
	"surface": {"wire", "protocol", "store", "storeclient", "model", "soul", "knowledge"},
	// dev and ops utilities
	"tool": {"wire", "protocol", "store", "storeclient", "model", "soul", "knowledge", "surface"},
}

// Outside modules that belong to one layer and stay out of the ones below.
const (
	mcpSDK    = "github.com/mark3labs/mcp-go"
	tuiKit    = "charm.land/"
	judgeSDK  = "github.com/anthropics/anthropic-sdk-go"
	kubeAPI   = "k8s.io/"
	oidcSDK   = "github.com/coreos/go-oidc"
	gcpClient = "cloud.google.com/"
)

// external names, for each layer, the outside modules its production code
// must not import. The store keeps gcpClient: server/blob/gcs is a backend.
var external = map[string][]string{
	"wire":        {mcpSDK, tuiKit, judgeSDK, kubeAPI, oidcSDK, gcpClient},
	"protocol":    {mcpSDK, tuiKit, judgeSDK, kubeAPI, oidcSDK, gcpClient},
	"store":       {mcpSDK, tuiKit, judgeSDK, kubeAPI, oidcSDK},
	"storeclient": {mcpSDK, tuiKit, judgeSDK, kubeAPI, oidcSDK, gcpClient},
	"model":       {mcpSDK, tuiKit, judgeSDK, kubeAPI, oidcSDK, gcpClient},
	"soul":        {mcpSDK, tuiKit, judgeSDK, kubeAPI, oidcSDK, gcpClient},
	"knowledge":   {mcpSDK, tuiKit},
}

func mayImport(from, to string) bool {
	return from == to || slices.Contains(allowed[from], to)
}

// mayUse reports whether a layer may import the outside package path.
func mayUse(layer, path string) bool {
	for _, prefix := range external[layer] {
		if path == prefix || strings.HasPrefix(path, strings.TrimSuffix(prefix, "/")+"/") {
			return false
		}
	}
	return true
}

// entry maps one package to a layer, or with a "/..." suffix that package and
// every package below it no more specific entry names.
type entry struct {
	pattern string
	layer   string
	line    int
}

type layerMap []entry

func parseMap(text string) (layerMap, error) {
	var m layerMap
	seen := map[string]int{}
	for n, line := range dataLines(text) {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return nil, fmt.Errorf("line %d: want <package> <layer>, got %q", n, line)
		}
		pattern, layer := fields[0], fields[1]
		if _, ok := allowed[layer]; !ok {
			return nil, fmt.Errorf("line %d: unknown layer %q", n, layer)
		}
		if prev, dup := seen[pattern]; dup {
			return nil, fmt.Errorf("line %d: %s already mapped on line %d", n, pattern, prev)
		}
		seen[pattern] = n
		m = append(m, entry{pattern: pattern, layer: layer, line: n})
	}
	return m, nil
}

// dataLines yields the numbered lines of a map or baseline file that hold
// data once a '#' comment is cut.
func dataLines(text string) iter.Seq2[int, string] {
	return func(yield func(int, string) bool) {
		n := 0
		for line := range strings.Lines(text) {
			n++
			data, _, _ := strings.Cut(line, "#")
			if data = strings.TrimSpace(data); data != "" && !yield(n, data) {
				return
			}
		}
	}
}

// resolve returns the entry that gives pkg its layer: an exact entry, else
// the longest "/..." entry covering it.
func (m layerMap) resolve(pkg string) (entry, bool) {
	var best entry
	found := false
	for _, e := range m {
		if e.pattern == pkg {
			return e, true
		}
		base, ok := strings.CutSuffix(e.pattern, "/...")
		if !ok || (pkg != base && !strings.HasPrefix(pkg, base+"/")) {
			continue
		}
		if !found || len(e.pattern) > len(best.pattern) {
			best, found = e, true
		}
	}
	return best, found
}
