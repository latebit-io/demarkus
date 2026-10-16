// Package layout is the machine readable part of a world's template
// document: where its journal, plans and roadmap live, which directories are
// staging, and what marks shared knowledge. Mechanics read names from here
// instead of carrying them; a world that lays itself out differently says so.
package layout

import (
	"context"
	"fmt"
	"math"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/directives"
)

// DocumentPath is the conventional template document, per world.
const DocumentPath = "/.well-known/demarkus/template.md"

// JournalDate names a journal entry: one document per day, "<date>.md".
const JournalDate = "2006-01-02"

// IsJournalEntry reports whether a journal directory's file name is a dated entry.
func IsJournalEntry(name string) bool {
	day, ok := strings.CutSuffix(name, ".md")
	if !ok || len(day) != len(JournalDate) {
		return false
	}
	_, err := time.Parse(JournalDate, day)
	return err == nil
}

// Layout is one world's layout; Default when the template says nothing.
type Layout struct {
	Journal      string   // dated entries, a directory with its slash
	Plans        string   // plan documents, a directory with its slash
	Roadmap      string   // active work, a document path relative to the scope
	Staging      []string // directory names whose content stays personal
	PromoteTypes []string // document types that mark shared knowledge
	PromoteFloor float64  // importance at which any typed document is shared
	RootProject  string   // slug of the one project at "/"; "" when every project has /<slug>/
}

// Default is the layout every seeded world starts from.
func Default() Layout {
	return Layout{
		Journal:      "journal/",
		Plans:        "plans/",
		Roadmap:      "roadmap.md",
		Staging:      []string{"journal", "thoughts", "debt", "notes", "scratch"},
		PromoteTypes: []string{"Decision", "Architecture", "Guideline", "Guide", "Reference", "Pattern"},
		PromoteFloor: 0.8,
	}
}

var segment = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// IsSegment reports whether s can be one store path segment: no delimiters,
// no dot-only names. Prompts quote the same rule for user-typed slugs.
func IsSegment(s string) bool { return segment.MatchString(s) }

// Parse overlays the template's directives on Default. The error names every
// key whose value was rejected; those keys keep the default, so a reader can
// still refuse to act on a template it cannot trust.
func Parse(body string) (Layout, error) {
	l := Default()
	var bad []string
	set := func(key string, ok bool) {
		if !ok {
			bad = append(bad, key)
		}
	}
	found := directives.First(body)
	if v, ok := found["journal"]; ok {
		set("journal", assignDir(&l.Journal, v))
	}
	if v, ok := found["plans"]; ok {
		set("plans", assignDir(&l.Plans, v))
	}
	if v, ok := found["roadmap"]; ok {
		set("roadmap", assign(&l.Roadmap, v, IsSegment))
	}
	if v, ok := found["staging"]; ok {
		set("staging", assignList(&l.Staging, v, IsSegment))
	}
	if v, ok := found["promote_types"]; ok {
		set("promote_types", assignList(&l.PromoteTypes, v, word))
	}
	if v, ok := found["promote_floor"]; ok {
		f, err := strconv.ParseFloat(v, 64)
		set("promote_floor", err == nil && f >= 0 && f <= 1)
		if err == nil && f >= 0 && f <= 1 {
			l.PromoteFloor = f
		}
	}
	if v, ok := found["root_project"]; ok {
		set("root_project", assign(&l.RootProject, v, IsSegment))
	}
	if len(bad) > 0 {
		return l, fmt.Errorf("template %s: invalid %s", DocumentPath, strings.Join(bad, ", "))
	}
	return l, nil
}

// Root is the project's directory with both slashes: "/" for the root
// project, "/<slug>/" for every other.
func (l *Layout) Root(slug string) string {
	if slug == l.RootProject {
		return "/"
	}
	return "/" + slug + "/"
}

// Read fetches the world's template through fetch: absent or archived is
// Default, a readable one parses, anything else is the reader's failure.
func Read(ctx context.Context, fetch func(ctx context.Context, path string) (protocol.Response, error)) (Layout, error) {
	resp, err := fetch(ctx, DocumentPath)
	switch {
	case err != nil:
		return Layout{}, fmt.Errorf("fetch %s: %w", DocumentPath, err)
	case resp.Status == protocol.StatusOK:
		return Parse(resp.Body)
	case resp.Status == protocol.StatusNotFound, resp.Status == protocol.StatusArchived:
		return Default(), nil
	}
	return Layout{}, fmt.Errorf("fetch %s: %s", DocumentPath, resp.Status)
}

func assign(dst *string, value string, ok func(string) bool) bool {
	if !ok(value) {
		return false
	}
	*dst = value
	return true
}

// assignDir accepts one directory name, slash optional, stored with it.
func assignDir(dst *string, value string) bool {
	name := strings.TrimSuffix(value, "/")
	if !IsSegment(name) {
		return false
	}
	*dst = name + "/"
	return true
}

// assignList accepts a comma list whose every item ok admits.
func assignList(dst *[]string, value string, ok func(string) bool) bool {
	var out []string
	for item := range strings.SplitSeq(value, ",") {
		item = strings.TrimSpace(item)
		if !ok(item) {
			return false
		}
		out = append(out, item)
	}
	*dst = slices.Clip(out)
	return true
}

// word is one token: non-empty, no inner whitespace.
func word(s string) bool { return s != "" && !strings.ContainsAny(s, " \t") }

// Section is the hub heading for a directory: "journal/" reads "Journal".
func Section(directory string) string {
	name := strings.TrimSuffix(directory, "/")
	if name == "" {
		return ""
	}
	return strings.ToUpper(name[:1]) + name[1:]
}

// Qualifies says why a document is shared knowledge under the layout's
// rules, or "" when it is not. An adr directory above it makes it a record;
// a staging directory keeps it personal.
func (l *Layout) Qualifies(p string, meta map[string]string) string {
	adr := false
	for seg := range strings.SplitSeq(path.Dir(p), "/") {
		if slices.Contains(l.Staging, seg) {
			return ""
		}
		adr = adr || seg == "adr"
	}
	kind := meta["type"]
	importance, parseErr := strconv.ParseFloat(meta["importance"], 64)
	if parseErr != nil || math.IsNaN(importance) || math.IsInf(importance, 0) {
		importance = 0 // unparsable, absent or not a number: below the floor
	}
	if !adr && !slices.Contains(l.PromoteTypes, kind) && importance < l.PromoteFloor {
		return ""
	}
	var parts []string
	if adr {
		parts = append(parts, "decision record")
	}
	if kind != "" {
		parts = append(parts, "type "+kind)
	}
	if meta["importance"] != "" {
		parts = append(parts, "importance "+meta["importance"])
	}
	return strings.Join(parts, ", ")
}
