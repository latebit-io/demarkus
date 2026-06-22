package okf

import (
	"path"
	"sort"
	"strings"
	"time"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/store"
)

// ExportDoc is a document fetched from a demarkus world, ready to render back
// into an OKF bundle file.
type ExportDoc struct {
	Path     string            // mark path, e.g. /vendor/tables/orders.md
	Body     string            // served markdown body (store frontmatter already stripped)
	Metadata map[string]string // publisher metadata from FETCH (bare keys)
	Modified string            // RFC 3339 modification time, used to synthesize timestamp
}

// BundleFile is a rendered OKF bundle file: a bundle-relative path and its bytes.
type BundleFile struct {
	Path    string
	Content []byte
}

// recognizedOrder is the canonical leading order of OKF frontmatter fields.
var recognizedOrder = []string{"type", "title", "description", "resource", "tags", "timestamp"}

var recognizedSet = map[string]bool{
	"type": true, "title": true, "description": true,
	"resource": true, "tags": true, "timestamp": true,
}

// BuildExport renders fetched documents into OKF bundle files, stripping the
// world prefix from both paths and bundle-absolute links. Concept documents get
// an OKF frontmatter block reconstructed from their metadata (a required `type`
// is synthesized when absent, and `timestamp` falls back to the document's
// modification time). Reserved files (index.md, log.md) are written body-only,
// except a root index.md that carries an okf-version, which is re-emitted as
// okf_version frontmatter. It performs no network I/O.
func BuildExport(docs []ExportDoc, prefix string) []BundleFile {
	pfx := strings.Trim(prefix, "/")
	files := make([]BundleFile, 0, len(docs))
	for _, d := range docs {
		rel := relFromPath(d.Path, pfx)
		body := stripAbsLinks(d.Body, pfx)
		var content string
		if IsReserved(path.Base(rel)) {
			content = renderReserved(rel, d.Metadata, body)
		} else {
			content = renderConcept(d.Metadata, body, d.Modified)
		}
		files = append(files, BundleFile{Path: rel, Content: []byte(content)})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files
}

func relFromPath(markPath, pfx string) string {
	p := strings.TrimPrefix(markPath, "/")
	if pfx != "" {
		p = strings.TrimPrefix(p, pfx+"/")
	}
	return p
}

func renderConcept(meta map[string]string, body, modified string) string {
	var b strings.Builder
	b.WriteString("---\n")

	typ := strings.TrimSpace(meta["type"])
	if typ == "" {
		typ = protocol.OKFDefaultType // OKF requires a non-empty type
	}
	writeScalar(&b, "type", typ)

	for _, k := range recognizedOrder {
		switch k {
		case "type":
			continue // already written
		case "tags":
			if v := meta["tags"]; v != "" {
				b.WriteString("tags: " + store.FormatTagsList(v) + "\n")
			}
		case "timestamp":
			if ts := normalizeTimestamp(meta["timestamp"], modified); ts != "" {
				writeScalar(&b, "timestamp", ts)
			}
		default:
			if v := meta[k]; v != "" {
				writeScalar(&b, k, v)
			}
		}
	}

	// Producer-defined keys (including demarkus's importance) preserved bare,
	// sorted for stable output.
	for _, k := range sortedKeys(meta) {
		if !recognizedSet[k] {
			writeScalar(&b, k, meta[k])
		}
	}

	b.WriteString("---\n")
	b.WriteString(body)
	return b.String()
}

// normalizeTimestamp returns an RFC 3339 timestamp. A declared value is parsed
// (accepting RFC 3339 or a date-only form) and re-emitted in canonical RFC 3339;
// if it is empty or unparseable, the document's modification time (already
// RFC 3339) is used instead.
func normalizeTimestamp(declared, modified string) string {
	if declared != "" {
		for _, layout := range []string{time.RFC3339, "2006-01-02"} {
			if t, err := time.Parse(layout, declared); err == nil {
				return t.UTC().Format(time.RFC3339)
			}
		}
	}
	return modified
}

func renderReserved(rel string, meta map[string]string, body string) string {
	// OKF reserved files carry no frontmatter, except the root index.md which
	// may declare okf_version (round-tripped from the okf-version metadata key
	// the importer stashes).
	if rel == IndexFile {
		if v := meta["okf-version"]; v != "" {
			return "---\nokf_version: " + yamlScalar(v) + "\n---\n" + body
		}
	}
	return body
}

func writeScalar(b *strings.Builder, key, val string) {
	b.WriteString(key + ": " + yamlScalar(val) + "\n")
}

// yamlScalar renders a value as a YAML scalar, double-quoting only when a bare
// scalar would be ambiguous or reparse differently (a "key: value" collision, a
// trailing comment, surrounding whitespace, an empty value, or a leading YAML
// indicator character).
func yamlScalar(v string) string {
	need := v == "" ||
		strings.Contains(v, ": ") || strings.HasSuffix(v, ":") ||
		strings.Contains(v, " #") ||
		v != strings.TrimSpace(v) ||
		strings.ContainsAny(v[:1], `[]{}#&*!|>'"%@`+"`,")
	if !need {
		return v
	}
	v = strings.ReplaceAll(v, `\`, `\\`)
	v = strings.ReplaceAll(v, `"`, `\"`)
	return `"` + v + `"`
}

// stripAbsLinks removes the "/<pfx>" prefix from every bundle-absolute markdown
// link destination, the inverse of import's rewriteAbsLinks. Relative links are
// untouched. With no prefix, the body is returned unchanged.
func stripAbsLinks(body, pfx string) string {
	if pfx == "" {
		return body
	}
	p := "/" + pfx
	return mapAbsLinks(body, func(dest string) string {
		switch {
		case dest == p:
			return "/"
		case strings.HasPrefix(dest, p+"/"):
			return strings.TrimPrefix(dest, p)
		default:
			return dest
		}
	})
}
