// Package index provides parsing and building of markdown hash index documents.
// These documents map content hashes to server locations, enabling cross-server
// content resolution in the Mark Protocol federation model.
package index

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/latebit/demarkus/protocol"
)

// Entry maps a content hash to a server location.
type Entry struct {
	Hash   string // sha256-<64hex>
	Server string // mark://host:port
	Path   string // /doc.md
}

// rowRe matches a markdown table row with three pipe-separated columns.
var rowRe = regexp.MustCompile(`^\|\s*(\S+)\s*\|\s*(\S+)\s*\|\s*(\S+)\s*\|$`)

// Parse extracts entries from a markdown hash index document body.
// It reads the markdown table, skipping the header and separator rows.
func Parse(body string) []Entry {
	var entries []Entry
	inTable := false
	headerSeen := false

	for line := range strings.SplitSeq(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "|") {
			inTable = false
			headerSeen = false
			continue
		}

		if !inTable {
			// First row of a table block — this is the header.
			inTable = true
			headerSeen = false
			continue
		}
		if !headerSeen {
			// Second row — separator (|---|---|---|).
			headerSeen = true
			continue
		}

		m := rowRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		entries = append(entries, Entry{
			Hash:   m[1],
			Server: m[2],
			Path:   m[3],
		})
	}
	return entries
}

// Build creates a markdown hash index document body.
func Build(source string, indexed time.Time, entries []Entry) string {
	var b strings.Builder
	b.WriteString("# Content Index\n\n")
	b.WriteString(fmt.Sprintf("> Source: %s\n", source))
	b.WriteString(fmt.Sprintf("> Indexed: %s\n", indexed.UTC().Format(time.RFC3339)))
	b.WriteString(fmt.Sprintf("> Documents: %d\n", len(entries)))
	b.WriteString("\n| Hash | Server | Path |\n")
	b.WriteString("|------|--------|------|\n")
	for _, e := range entries {
		b.WriteString(fmt.Sprintf("| %s | %s | %s |\n", e.Hash, e.Server, e.Path))
	}
	return b.String()
}

// Merge combines entries: removes all entries matching sourceServer,
// then appends newEntries. This allows a single index document to
// aggregate entries from many servers, with each server's entries
// independently refreshable.
// Server URLs are canonicalized before comparison to avoid duplicates
// from representation differences (trailing slash, default port).
func Merge(existing []Entry, sourceServer string, newEntries []Entry) []Entry {
	canonical := canonicalServer(sourceServer)
	var result []Entry
	for _, e := range existing {
		if canonicalServer(e.Server) != canonical {
			result = append(result, e)
		}
	}
	return append(result, newEntries...)
}

// canonicalServer normalizes a mark:// server URL for comparison.
// It strips trailing slashes, removes the default port, and lowercases.
func canonicalServer(s string) string {
	s = strings.TrimRight(s, "/")
	defaultSuffix := fmt.Sprintf(":%d", protocol.DefaultPort)
	s = strings.TrimSuffix(s, defaultSuffix)
	return strings.ToLower(s)
}
