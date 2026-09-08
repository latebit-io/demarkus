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

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/mcpfmt"
	"github.com/latebit-io/demarkus/protocol"
)

// uncarried keys never count as dropped: the surface stamps agent, the store
// never inherits retention, and tags are compared as a set.
var uncarried = map[string]bool{"agent": true, "retention": true, "tags": true}

// maxValue bounds a dropped key's value in the note.
const maxValue = 80

// readTimeout bounds the read of the replaced version when the tool call
// carries no deadline of its own.
const readTimeout = 5 * time.Second

// Gate runs after a successful update: read fetches the replaced version and
// the note names what the new one dropped. A create or a pruned version yields
// nothing; any other failed read is the caller's to log, never a blocker.
func Gate(ctx context.Context, expectedVersion int, meta map[string]string, read func(context.Context) (fetch.Result, error)) (string, error) {
	if expectedVersion == 0 {
		return "", nil
	}
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	replaced, err := read(ctx)
	if err != nil {
		return "", err
	}
	switch replaced.Response.Status {
	case protocol.StatusOK:
	case protocol.StatusNotFound:
		return "", nil // pruned by retention: nothing to compare
	default:
		return "", fmt.Errorf("read v%d: status %s", expectedVersion, replaced.Response.Status)
	}
	return Compare(replaced.Response.Metadata, meta).Note(replaced.Response.Metadata["version"]), nil
}

// Narrowing lists what a publish drops relative to the version it replaced.
type Narrowing struct {
	Tags []string // tags present before, absent now
	Keys []string // "key=value" for keys present before, absent now
}

// Compare diffs the replaced version's metadata against the incoming map.
func Compare(current, incoming map[string]string) Narrowing {
	var n Narrowing
	have := splitTags(incoming["tags"])
	for tag := range splitTags(current["tags"]) {
		if !have[tag] {
			n.Tags = append(n.Tags, tag)
		}
	}
	for key, value := range current {
		if protocol.ReservedMetadataKeys[key] || uncarried[key] {
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
	var parts []string
	if len(n.Tags) > 0 {
		parts = append(parts, "tags "+strings.Join(n.Tags, ", "))
	}
	if len(n.Keys) > 0 {
		parts = append(parts, "keys "+strings.Join(n.Keys, "; "))
	}
	if len(parts) == 0 {
		return ""
	}
	return mcpfmt.Note(fmt.Sprintf("this publish dropped %s carried by v%s; fetch with verbose: true and republish the complete metadata map to restore them",
		strings.Join(parts, " and "), version))
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
	for _, t := range protocol.SplitTags(s) {
		set[t] = true
	}
	return set
}
