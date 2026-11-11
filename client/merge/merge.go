package merge

import (
	"fmt"
	"strings"
)

// OnConflict is what a publish does when the document moved past the
// version it was edited from.
type OnConflict string

// The two modes, by the name every surface gives them.
const (
	// OnConflictMerge answers with a three way merge candidate to review.
	OnConflictMerge OnConflict = "merge"
	// OnConflictFail answers with the server's conflict.
	OnConflictFail OnConflict = "fail"
)

// ModeNamed is the mode a surface's option names, surrounding space
// ignored; ok is false for anything else.
func ModeNamed(name string) (mode OnConflict, ok bool) {
	switch mode = OnConflict(strings.TrimSpace(name)); mode {
	case OnConflictMerge, OnConflictFail:
		return mode, true
	}
	return "", false
}

// ModeOf reads a surface's option: blank is merge, which an omitted option
// sends. option is the surface's own name for it, so the refusal names what
// the caller typed: on_conflict for a tool, -on-conflict for the CLI.
func ModeOf(option, raw string) (OnConflict, error) {
	name := strings.TrimSpace(raw)
	if name == "" {
		return OnConflictMerge, nil
	}
	mode, ok := ModeNamed(name)
	if !ok {
		return "", fmt.Errorf("invalid %s %q: expected %q or %q", option, name, OnConflictMerge, OnConflictFail)
	}
	return mode, nil
}
