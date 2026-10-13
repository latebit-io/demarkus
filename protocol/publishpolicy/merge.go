package publishpolicy

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/latebit-io/demarkus/protocol"
)

// strictnessRank orders strictness so Merge can take the stricter; absent
// and invalid values rank as Warn, which EffectiveStrictness also yields.
func strictnessRank(s Strictness) int {
	switch s {
	case Ask:
		return 1
	case Block:
		return 2
	default:
		return 0
	}
}

// Merge is the policy a world is held to under a floor: the stricter
// strictness, the union of required axes and fields, per axis the
// intersection of allowed values. A world only tightens the floor.
func Merge(floor, world Policy) Policy {
	merged := Policy{Strictness: world.EffectiveStrictness()}
	if strictnessRank(floor.EffectiveStrictness()) > strictnessRank(merged.Strictness) {
		merged.Strictness = floor.EffectiveStrictness()
	}
	merged.RequiredTagAxes = protocol.MergeTags(floor.RequiredTagAxes, world.RequiredTagAxes)
	merged.RequiredFields = protocol.MergeTags(floor.RequiredFields, world.RequiredFields)
	if len(floor.AllowedTagValues) > 0 || len(world.AllowedTagValues) > 0 {
		merged.AllowedTagValues = make(map[string][]string)
		for axis, values := range floor.AllowedTagValues {
			merged.AllowedTagValues[axis] = slices.Clone(values)
		}
		for axis, values := range world.AllowedTagValues {
			if floorValues, both := floor.AllowedTagValues[axis]; both {
				merged.AllowedTagValues[axis] = intersect(floorValues, values)
				continue
			}
			merged.AllowedTagValues[axis] = slices.Clone(values)
		}
	}
	return merged
}

// Loosens lists the lines of world that contradict floor: a strictness set
// below it, an allowed value it refuses. Omitting a floor rule is not one;
// the merge carries it. Reporting only, the floor applies either way.
func Loosens(floor, world Policy) []string {
	var out []string
	if world.Strictness != "" && strictnessRank(world.EffectiveStrictness()) < strictnessRank(floor.EffectiveStrictness()) {
		out = append(out, fmt.Sprintf("strictness %s is below the floor's %s", world.EffectiveStrictness(), floor.EffectiveStrictness()))
	}
	for _, axis := range slices.Sorted(maps.Keys(floor.AllowedTagValues)) {
		worldValues, listed := world.AllowedTagValues[axis]
		if !listed {
			continue
		}
		if extra := difference(worldValues, floor.AllowedTagValues[axis]); len(extra) > 0 {
			out = append(out, fmt.Sprintf("allow_tags.%s adds %s, which the floor does not allow", axis, strings.Join(extra, ", ")))
		}
	}
	return out
}

// intersect keeps a's entries that b also holds, in a's order.
func intersect(a, b []string) []string {
	return slices.DeleteFunc(slices.Clone(a), func(s string) bool { return !slices.Contains(b, s) })
}

// difference is a minus b, in a's order.
func difference(a, b []string) []string {
	return slices.DeleteFunc(slices.Clone(a), func(s string) bool { return slices.Contains(b, s) })
}
