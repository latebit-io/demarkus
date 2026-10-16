package main

import (
	"bufio"
	"fmt"
	"io"
	"slices"
	"strings"
)

// allowed names, for each target layer, the layers its packages may import
// besides their own: dependencies point down only. Changing a row changes the
// architecture and takes an ADR.
var allowed = map[string][]string{
	// wire types, stored format, shared contracts
	"protocol": nil,
	// server side of the memory store; never imports the client (ADR 0026)
	"store": {"protocol"},
	// the store client, the SDK to be
	"storeclient": {"protocol"},
	// document kinds, hubs, write gates, journal, promote, doctor
	"model": {"protocol", "storeclient"},
	// the two tiers are siblings: neither imports the other
	"soul":      {"protocol", "store", "storeclient", "model"},
	"knowledge": {"protocol", "store", "storeclient", "model"},
	// MCP tools, plugins, gateway, CLI, TUI and the binaries
	"surface": {"protocol", "store", "storeclient", "model", "soul", "knowledge"},
	// dev and ops utilities
	"tool": {"protocol", "store", "storeclient", "model", "soul", "knowledge", "surface"},
}

func mayImport(from, to string) bool {
	return from == to || slices.Contains(allowed[from], to)
}

// entry maps one package to a layer, or with a "/..." suffix that package and
// every package below it no more specific entry names.
type entry struct {
	pattern string
	layer   string
	line    int
}

type layerMap []entry

func parseMap(r io.Reader) (layerMap, error) {
	var m layerMap
	seen := map[string]int{}
	sc := bufio.NewScanner(r)
	for n := 1; sc.Scan(); n++ {
		text, _, _ := strings.Cut(sc.Text(), "#")
		fields := strings.Fields(text)
		if len(fields) == 0 {
			continue
		}
		if len(fields) != 2 {
			return nil, fmt.Errorf("line %d: want <package> <layer>, got %q", n, sc.Text())
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
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return m, nil
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
