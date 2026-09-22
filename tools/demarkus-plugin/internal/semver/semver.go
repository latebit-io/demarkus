// Package semver orders plain x.y.z release versions, the one shape every
// demarkus release tag carries.
package semver

import (
	"regexp"
	"strconv"
	"strings"
)

var releaseRe = regexp.MustCompile(`^\d+\.\d+\.\d+$`)

// Version is a parsed x.y.z release.
type Version struct{ Major, Minor, Patch int }

// Parse reads a plain x.y.z release. ok is false for anything else (a dev
// build, a tagged prerelease, an empty probe), where no ordering is meaningful.
func Parse(v string) (Version, bool) {
	if !releaseRe.MatchString(v) {
		return Version{}, false
	}
	var out [3]int
	for i, part := range strings.Split(v, ".") {
		n, err := strconv.Atoi(part)
		if err != nil {
			return Version{}, false // beyond int range: no usable order
		}
		out[i] = n
	}
	return Version{out[0], out[1], out[2]}, true
}

// Less reports whether v orders before other.
func (v Version) Less(other Version) bool {
	if v.Major != other.Major {
		return v.Major < other.Major
	}
	if v.Minor != other.Minor {
		return v.Minor < other.Minor
	}
	return v.Patch < other.Patch
}
