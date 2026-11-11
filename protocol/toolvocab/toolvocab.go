// Package toolvocab names the mark_* tools the surfaces serve and the
// profiles that select them. It holds names only, so the tiers can validate a
// profile or classify a call without linking an MCP library.
package toolvocab

import (
	"fmt"
	"strings"
)

// Tool profiles. Lean is the plugin-hosted surface: no operator or federation
// tools. Full is the complete surface for direct consumers.
const (
	ProfileLean = "lean"
	ProfileFull = "full"
)

// Prefix starts every tool name.
const Prefix = "mark_"

// Class says what a tool does to a store and where it is served.
type Class struct {
	Write    bool // changes a store or the graph store
	Store    bool // changes a document store: the write gate's set
	Advanced bool // absent from the lean profile
}

// classes is every mark_* tool any surface serves, the plugin binary's host
// tools included. mark_archive stays in lean: /soul-archive is the only
// archive path a plugin user has.
var classes = map[string]Class{
	"mark_fetch": {}, "mark_explore": {}, "mark_list": {}, "mark_versions": {},
	"mark_lookup": {}, "mark_lookup_all": {}, "mark_backlinks": {}, "mark_worlds": {},
	"mark_doctor": {}, "mark_discover": {Advanced: true}, "mark_graph_export": {Advanced: true},
	"mark_publish": {Write: true, Store: true}, "mark_append": {Write: true, Store: true}, "mark_archive": {Write: true, Store: true},
	"mark_curate": {Write: true, Store: true}, "mark_promote": {Write: true, Store: true}, "mark_graph": {Write: true},
	"mark_graph_publish": {Write: true, Store: true, Advanced: true},
	"mark_host":          {}, "mark_host_setup": {Write: true},
}

// Known, Advanced and StoreWrite are views of the class table. StoreWrite is
// the set the write gate holds; every host's hook matcher names exactly these.
var (
	Known      = set(func(Class) bool { return true })
	Advanced   = set(func(c Class) bool { return c.Advanced })
	StoreWrite = set(func(c Class) bool { return c.Store })
)

func set(keep func(Class) bool) map[string]bool {
	out := map[string]bool{}
	for name, class := range classes {
		if keep(class) {
			out[name] = true
		}
	}
	return out
}

// ClassOf is the class of a known tool.
func ClassOf(name string) (Class, bool) {
	class, ok := classes[name]
	return class, ok
}

// ValidProfile rejects anything but the two named profiles. Surfaces call it
// at their input boundary (flag, config); ProfileIncludes trusts the value.
func ValidProfile(profile string) error {
	if profile != ProfileLean && profile != ProfileFull {
		return fmt.Errorf("unknown tool profile %q (lean or full)", profile)
	}
	return nil
}

// ProfileIncludes reports whether the profile exposes the named tool.
func ProfileIncludes(profile, name string) bool {
	return profile != ProfileLean || !Advanced[name]
}

// Suffix finds the known tool a harness-qualified name ends in and returns
// it with what precedes it. The longest name wins and must start the string
// or follow an underscore, so a server named "bookmark" hides no tool.
func Suffix(qualified string) (name, prefix string, ok bool) {
	for known := range classes {
		before, found := strings.CutSuffix(qualified, known)
		if !found || len(known) <= len(name) {
			continue
		}
		if before == "" || strings.HasSuffix(before, "_") {
			name, prefix, ok = known, before, true
		}
	}
	return name, prefix, ok
}
