// Package provenance reads a document's pins: the rel-based-on values naming
// the exact version of each source it was written from. A source moved past
// its pin is drift. Every tier reads pins here so all agree on them.
package provenance

import (
	"fmt"
	"path"
	"strconv"
	"strings"
	"unicode"

	"github.com/latebit-io/demarkus/protocol"
)

// Key is the metadata key pins live under, a typed relation (ADR 0004)
// whose values are permalinks.
const Key = "rel-based-on"

// PinForms names the permalinks that pin, for a fix that asks for one.
const PinForms = "/doc.md/vN or a GitHub commit URL"

type kind uint8

const (
	// opaque is a URL of no form known here, taken as the writer's
	// permalink; nothing judges its drift.
	opaque kind = iota
	document
	commit
	// malformed has whitespace in it and names nothing.
	malformed
)

// Pin is one rel-based-on value.
type Pin struct {
	Ref  string // as written
	kind kind
	// Source is the pinned thing without its version: a document path or
	// mark:// URL, a repository URL (with a tree or blob path), or Ref for
	// an opaque pin.
	Source string
	// Version is a document's decimal version or a commit; "" when the
	// value names none.
	Version string
}

// Parse reads the pins in a rel-based-on value, in written order.
func Parse(value string) []Pin {
	refs := protocol.SplitTags(value)
	pins := make([]Pin, len(refs))
	for i, ref := range refs {
		pins[i] = ParsePin(ref)
	}
	return pins
}

// Unpinned lists the refs in a rel-based-on value that are no versioned
// permalink: a moving target whose drift no reader can measure.
func Unpinned(value string) []string {
	var out []string
	for _, pin := range Parse(value) {
		if !pin.Pinned() {
			out = append(out, pin.Ref)
		}
	}
	return out
}

// ParsePin reads one value. A fragment or query names part of a version,
// never another one, so it is dropped.
func ParsePin(ref string) Pin {
	if strings.IndexFunc(ref, unicode.IsSpace) >= 0 {
		return Pin{Ref: ref, kind: malformed, Source: ref}
	}
	target, _, _ := strings.Cut(ref, "#")
	target, _, _ = strings.Cut(target, "?")
	scheme, _, hasScheme := strings.Cut(target, "://")
	switch {
	case !hasScheme || strings.EqualFold(scheme, "mark"):
		source, version := cutVersion(target)
		return Pin{Ref: ref, kind: document, Source: source, Version: version}
	case strings.HasPrefix(strings.ToLower(target), githubPrefix):
		return githubPin(ref, target)
	}
	return Pin{Ref: ref, kind: opaque, Source: ref}
}

// Pinned reports whether the value names an immutable version.
func (p Pin) Pinned() bool { return p.kind == opaque || p.Version != "" }

// DocumentVersion is a document pin's version, 0 for any other value.
func (p Pin) DocumentVersion() int {
	if p.kind != document {
		return 0
	}
	n, err := strconv.Atoi(p.Version)
	if err != nil {
		return 0
	}
	return n
}

// Local is the store path a document pin written as a path names, resolved
// against the pinning document's path; "" for every other pin.
func (p Pin) Local(docPath string) string {
	if p.kind != document || strings.Contains(p.Source, "://") {
		return ""
	}
	if strings.HasPrefix(p.Source, "/") {
		return path.Clean(p.Source)
	}
	return path.Join(path.Dir(docPath), p.Source)
}

// Target is what the pin is on, at any version: Local for a path pin, else
// Source.
func (p Pin) Target(docPath string) string {
	if local := p.Local(docPath); local != "" {
		return local
	}
	return p.Source
}

// Under reports whether target is source or something under it: a
// directory, a repository URL covering its commits. It allocates nothing,
// since a catalog filter calls it for every pin in a world.
func Under(target, source string) bool {
	if target == source {
		return true
	}
	dir := strings.TrimSuffix(source, "/")
	return len(target) > len(dir) && target[len(dir)] == '/' && strings.HasPrefix(target, dir)
}

// Drift words a document pin against its source's current version, one
// wording for every tier; both "" when the pin is current.
func Drift(pinned, current int) (problem, fix string) {
	switch {
	case pinned < current:
		return fmt.Sprintf("is at v%d, past the pinned v%d", current, pinned), fmt.Sprintf("read what changed, update, pin v%d", current)
	case pinned > current:
		return fmt.Sprintf("has no v%d; it is at v%d", pinned, current), "pin the version it was written from"
	}
	return "", ""
}

// cutVersion splits a document version path (SPEC 9.2, /doc.md/v5) into the
// document and its version; anything else is a document with no version.
func cutVersion(target string) (source, version string) {
	i := strings.LastIndexByte(target, '/')
	if i < 0 || !strings.HasSuffix(target[:i], ".md") || !strings.HasPrefix(target[i+1:], "v") {
		return target, ""
	}
	n, err := strconv.ParseUint(target[i+2:], 10, 31)
	if err != nil || n < 1 {
		return target, ""
	}
	return target[:i], strconv.FormatUint(n, 10)
}

const githubPrefix = "https://github.com/"

// githubPin reads https://github.com/<owner>/<repo>[/(tree|blob|commit)/<ref>[/<path>]].
// Only a commit hash pins: a branch or tag moves. Source keeps a tree or
// blob path without its ref, so it matches at any version.
func githubPin(ref, target string) Pin {
	segments := strings.Split(strings.Trim(target[len(githubPrefix):], "/"), "/")
	if len(segments) < 2 || segments[0] == "" || segments[1] == "" {
		return Pin{Ref: ref, kind: opaque, Source: ref}
	}
	pin := Pin{Ref: ref, kind: commit, Source: githubPrefix + segments[0] + "/" + strings.TrimSuffix(segments[1], ".git")}
	if len(segments) >= 4 && (segments[2] == "tree" || segments[2] == "blob" || segments[2] == "commit") && isCommit(segments[3]) {
		pin.Version = segments[3]
	}
	if len(segments) > 4 && (segments[2] == "tree" || segments[2] == "blob") {
		pin.Source += "/" + strings.Join(segments[4:], "/")
	}
	return pin
}

// isCommit accepts an abbreviated or full hex commit hash.
func isCommit(s string) bool {
	if len(s) < 7 || len(s) > 64 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}
