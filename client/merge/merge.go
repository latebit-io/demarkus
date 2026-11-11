package merge

import "strings"

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
// ignored; ok is false for anything else. What an omitted option means, and
// how a bad one is refused, are the surface's.
func ModeNamed(name string) (mode OnConflict, ok bool) {
	switch mode = OnConflict(strings.TrimSpace(name)); mode {
	case OnConflictMerge, OnConflictFail:
		return mode, true
	}
	return "", false
}
