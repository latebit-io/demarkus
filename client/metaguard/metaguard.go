// Package metaguard is the warn-only narrowing gate on mark_publish: PUBLISH
// replaces the metadata map, so a publish built from a lean fetch silently
// drops tags and keys. Both MCP surfaces report what the write dropped.
package metaguard

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// preReadTimeout bounds the pre-read when the tool call carries no deadline,
// so a slow server cannot stall a publish on a warn-only check.
const preReadTimeout = 5 * time.Second

// PreReadContext derives the context for the pre-read: the caller's deadline
// when it has one, else a short one of its own.
func PreReadContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, preReadTimeout)
}

// Server-owned or deliberately uncarried keys never count as dropped.
var ignoredKeys = map[string]bool{
	"version": true, "modified": true, "etag": true, "content-hash": true,
	"agent": true, "retention": true,
}

// maxValue bounds a dropped key's value in the note.
const maxValue = 80

// Narrowing lists what a publish drops relative to the current version.
type Narrowing struct {
	Tags []string // tags present before, absent now
	Keys []string // "key=value" for keys present before, absent now
}

// Empty reports that nothing was dropped.
func (n Narrowing) Empty() bool { return len(n.Tags) == 0 && len(n.Keys) == 0 }

// Compare diffs the current version's metadata against the incoming map.
func Compare(current, incoming map[string]string) Narrowing {
	var n Narrowing
	have := splitTags(incoming["tags"])
	for tag := range splitTags(current["tags"]) {
		if !have[tag] {
			n.Tags = append(n.Tags, tag)
		}
	}
	for key, value := range current {
		if ignoredKeys[key] || key == "tags" {
			continue
		}
		if _, ok := incoming[key]; !ok {
			n.Keys = append(n.Keys, key+"="+truncate(value))
		}
	}
	sort.Strings(n.Tags)
	sort.Strings(n.Keys)
	return n
}

// Note renders the warning appended to a successful publish result, or ""
// when nothing was dropped. version is the replaced version's number.
func (n Narrowing) Note(version string) string {
	if n.Empty() {
		return ""
	}
	var parts []string
	if len(n.Tags) > 0 {
		parts = append(parts, "tags "+strings.Join(n.Tags, ", "))
	}
	if len(n.Keys) > 0 {
		parts = append(parts, "keys "+strings.Join(n.Keys, "; "))
	}
	return fmt.Sprintf("\nnote: this publish dropped %s carried by v%s; fetch with verbose: true and republish the complete metadata map to restore them\n",
		strings.Join(parts, " and "), version)
}

// truncate cuts value at maxValue bytes on a rune boundary.
func truncate(value string) string {
	if len(value) <= maxValue {
		return value
	}
	cut := maxValue
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut] + "..."
}

func splitTags(s string) map[string]bool {
	set := map[string]bool{}
	for raw := range strings.SplitSeq(s, ",") {
		if t := strings.TrimSpace(raw); t != "" {
			set[t] = true
		}
	}
	return set
}
