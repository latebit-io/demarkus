package catalog

import (
	"maps"
	"path"
	"strconv"
	"strings"
	"time"
)

// defaultImportance is used when a document declares no importance, or an
// unparseable or out-of-range value, per the LOOKUP spec.
const defaultImportance = 0.5

// FromDocument builds a catalog Entry from a document's declared metadata,
// markdown body, and modification time. metadata is the publisher metadata
// with the store's "meta." prefix already stripped; body is the document
// content with store frontmatter already removed. The metadata map is copied,
// so the caller may reuse or mutate its own map afterward.
func FromDocument(docPath string, metadata map[string]string, body []byte, modified time.Time) *Entry {
	meta := make(map[string]string, len(metadata))
	maps.Copy(meta, metadata)
	return &Entry{
		Path:       docPath,
		Tags:       parseTags(meta["tags"]),
		Importance: parseImportance(meta["importance"]),
		Title:      resolveTitle(meta["title"], body, docPath),
		Modified:   modified,
		Metadata:   meta,
	}
}

// parseTags splits a comma-separated tag string into trimmed, non-empty tags.
func parseTags(s string) []string {
	if s == "" {
		return nil
	}
	var tags []string
	for raw := range strings.SplitSeq(s, ",") {
		if t := strings.TrimSpace(raw); t != "" {
			tags = append(tags, t)
		}
	}
	return tags
}

// parseImportance parses a float in [0,1]. Absent, unparseable, or
// out-of-range values fall back to defaultImportance.
func parseImportance(s string) float64 {
	if strings.TrimSpace(s) == "" {
		return defaultImportance
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || v < 0 || v > 1 {
		return defaultImportance
	}
	return v
}

// resolveTitle returns the declared title, else the document's first level-1
// heading, else the path's base name.
func resolveTitle(declared string, body []byte, docPath string) string {
	if t := strings.TrimSpace(declared); t != "" {
		return t
	}
	if h := firstH1(body); h != "" {
		return h
	}
	return path.Base(docPath)
}

// firstH1 returns the text of the first level-1 ATX heading ("# ...") in body,
// or "" if none. Only a single leading '#' followed by a space qualifies, so
// "## Sub" and "#NoSpace" do not match.
func firstH1(body []byte) string {
	for line := range strings.SplitSeq(string(body), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "# ") {
			return strings.TrimSpace(trimmed[2:])
		}
	}
	return ""
}
