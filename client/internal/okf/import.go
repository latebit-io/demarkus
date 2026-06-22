package okf

import (
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/latebit-io/demarkus/client/links"
	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/store"
)

// PublishItem is a single document ready to publish into a demarkus world.
type PublishItem struct {
	Path     string            // mark path, e.g. /prefix/tables/orders.md
	Body     string            // markdown body: frontmatter stripped, links rewritten
	Metadata map[string]string // demarkus publisher metadata (sanitized, within caps)
	Warnings []string          // non-fatal transforms applied to this document
}

// BuildImport walks an OKF bundle and produces the publish items that mirror it
// into a demarkus world under prefix (the path within the world; "" or "/"
// imports at the world root). It performs no network I/O. Per-document issues —
// a missing type, an unparseable block, dropped or sanitized metadata — are
// attached to the item's Warnings rather than failing the whole import, matching
// the spec's permissive consumption model.
func BuildImport(root, prefix string) ([]PublishItem, error) {
	info, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("stat bundle: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("bundle path is not a directory: %s", root)
	}
	pfx := strings.Trim(prefix, "/")

	var items []PublishItem
	walkErr := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// Reject symlinks: a crafted bundle could point a *.md symlink at local
		// content outside the bundle and have it ingested/published.
		if d.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("bundle contains a symlink, which is not allowed: %s", p)
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".md") {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		items = append(items, buildItem(filepath.ToSlash(rel), data, pfx))
		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("walk bundle: %w", walkErr)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Path < items[j].Path })
	return items, nil
}

func buildItem(rel string, data []byte, pfx string) PublishItem {
	fm, body, hasFM := SplitFrontmatter(data)
	meta := map[string]string{}
	var warns []string

	if IsReserved(path.Base(rel)) {
		// Reserved files carry no document metadata. The root index.md may have
		// an okf_version frontmatter block; strip it (publishing it verbatim
		// would store it as literal body) and keep the value as metadata.
		if hasFM {
			if parsed, err := ParseFrontmatter(fm); err == nil {
				if v := parsed["okf_version"]; v != "" {
					meta["okf-version"] = v
				}
			}
		}
	} else if !hasFM {
		warns = append(warns, "concept document has no frontmatter (OKF requires a `type`)")
	} else if parsed, err := ParseFrontmatter(fm); err != nil {
		warns = append(warns, fmt.Sprintf("unparseable frontmatter, importing body only: %v", err))
	} else {
		var mapWarns, capWarns []string
		meta, mapWarns = mapMetadata(parsed)
		if strings.TrimSpace(meta["type"]) == "" {
			warns = append(warns, "source document has no `type` field (OKF requires one)")
		}
		meta, capWarns = enforceCaps(meta)
		warns = append(warns, mapWarns...)
		warns = append(warns, capWarns...)
	}

	return PublishItem{
		Path:     joinPrefix(pfx, rel),
		Body:     rewriteAbsLinks(string(body), pfx),
		Metadata: meta,
		Warnings: warns,
	}
}

// mapMetadata converts parsed OKF frontmatter into demarkus publisher metadata.
// Recognized OKF field names already match demarkus's bare keys, so the mapping
// is mostly identity; the work is sanitizing keys/values that demarkus's wire
// rules reject and dropping the bundle-level okf_version (not a document field).
func mapMetadata(fm map[string]string) (meta map[string]string, warnings []string) {
	meta = map[string]string{}
	var warns []string
	for _, k := range sortedKeys(fm) {
		v := fm[k]
		if k == "okf_version" {
			continue // bundle-level marker, not a document field
		}
		key := k
		if store.IsReservedMetaKey(key) || !protocol.IsValidMetaKey(key) {
			sk := sanitizeKey(key)
			switch {
			case sk == "" || store.IsReservedMetaKey(sk) || !protocol.IsValidMetaKey(sk):
				warns = append(warns, fmt.Sprintf("dropped metadata key %q (cannot be made a valid demarkus key)", k))
				continue
			case keyPresent(meta, sk):
				warns = append(warns, fmt.Sprintf("dropped metadata key %q (sanitized form %q collides)", k, sk))
				continue
			default:
				warns = append(warns, fmt.Sprintf("sanitized metadata key %q → %q", k, sk))
				key = sk
			}
		}
		if !protocol.IsValidMetaValue(v) {
			v = strings.NewReplacer("\r", " ", "\n", " ").Replace(v)
			warns = append(warns, fmt.Sprintf("flattened newlines in metadata value for %q", key))
		}
		meta[key] = v
	}
	return meta, warns
}

// enforceCaps drops the lowest-priority metadata keys until the map fits within
// MaxMetaKeys and MaxMetaBytes (counted at on-disk serialized size, the same
// accounting the store enforces). Producer-defined keys are shed before the
// recognized OKF fields. Every drop is reported.
func enforceCaps(meta map[string]string) (capped map[string]string, warnings []string) {
	var warns []string
	for len(meta) > protocol.MaxMetaKeys || metaSize(meta) > protocol.MaxMetaBytes {
		drop := lowestPriorityKey(meta)
		if drop == "" {
			break
		}
		warns = append(warns, fmt.Sprintf("dropped metadata key %q to fit caps (max %d keys / %d bytes)",
			drop, protocol.MaxMetaKeys, protocol.MaxMetaBytes))
		delete(meta, drop)
	}
	return meta, warns
}

func metaSize(meta map[string]string) int {
	n := 0
	for k, v := range meta {
		n += store.SerializedMetaSize(k, v)
	}
	return n
}

// metaPriority ranks keys for cap-driven dropping: lower rank is kept longer.
// Catalog-visible fields (title, tags, type, importance) rank highest;
// producer-defined keys rank lowest and are dropped first.
func metaPriority(key string) int {
	switch key {
	case "title":
		return 0
	case "tags":
		return 1
	case "type":
		return 2
	case "importance":
		return 3
	case "description":
		return 4
	case "resource":
		return 5
	case "timestamp":
		return 6
	default:
		return 100
	}
}

func lowestPriorityKey(meta map[string]string) string {
	worst := ""
	worstRank := -1
	for k := range meta {
		r := metaPriority(k)
		// Tie-break on name (descending) so dropping is deterministic.
		if r > worstRank || (r == worstRank && k > worst) {
			worst, worstRank = k, r
		}
	}
	return worst
}

// sanitizeKey lowercases a key and replaces every character demarkus disallows
// with '-', collapsing runs and trimming leading/trailing '-'.
func sanitizeKey(key string) string {
	var b strings.Builder
	lastDash := false
	for _, c := range strings.ToLower(key) {
		ok := (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-'
		if !ok {
			c = '-'
		}
		if c == '-' {
			if lastDash {
				continue
			}
			lastDash = true
		} else {
			lastDash = false
		}
		b.WriteRune(c)
	}
	return strings.Trim(b.String(), "-")
}

// rewriteAbsLinks prepends "/<pfx>" to every bundle-absolute markdown link
// destination (one beginning with "/"). Relative links are left untouched: the
// bundle's directory structure is preserved under the prefix, so they still
// resolve. With no prefix, the body is returned unchanged.
func rewriteAbsLinks(body, pfx string) string {
	if pfx == "" {
		return body
	}
	p := "/" + pfx
	return mapAbsLinks(body, func(dest string) string { return p + dest })
}

// mapAbsLinks rewrites every bundle-absolute inline markdown link destination
// (one beginning with "/") by passing it through fn. Relative and external links
// and the surrounding text are left untouched. Only inline "[text](dest)" links
// with non-empty text are handled (the link extractor reports no bracket span
// for empty-text links).
func mapAbsLinks(body string, fn func(dest string) string) string {
	src := []byte(body)
	type edit struct {
		start, end int
		dest       string
	}
	var edits []edit
	for _, li := range links.ExtractWithPositions(body) {
		if li.CloseBracket < 0 || !strings.HasPrefix(li.Dest, "/") {
			continue
		}
		open := li.CloseBracket + 1
		if open >= len(src) || src[open] != '(' {
			continue // not an inline "[text](dest)" link
		}
		start := open + 1
		end := start
		for end < len(src) && src[end] != ')' && src[end] != ' ' && src[end] != '\t' && src[end] != '#' {
			end++
		}
		orig := string(src[start:end])
		if nw := fn(orig); nw != orig {
			edits = append(edits, edit{start, end, nw})
		}
	}
	if len(edits) == 0 {
		return body
	}
	// Apply right-to-left so earlier byte offsets stay valid.
	sort.Slice(edits, func(i, j int) bool { return edits[i].start > edits[j].start })
	out := body
	for _, e := range edits {
		out = out[:e.start] + e.dest + out[e.end:]
	}
	return out
}

func joinPrefix(pfx, rel string) string {
	if pfx == "" {
		return "/" + rel
	}
	return "/" + pfx + "/" + rel
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func keyPresent(m map[string]string, k string) bool {
	_, ok := m[k]
	return ok
}
