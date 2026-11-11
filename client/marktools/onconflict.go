package marktools

import (
	"fmt"
	"strings"

	"github.com/latebit-io/demarkus/client/merge"
)

// parseOnConflict reads a tool's on_conflict argument: blank means merge,
// which MCP clients commonly send for an omitted optional field.
func parseOnConflict(raw string) (merge.OnConflict, error) {
	name := strings.TrimSpace(raw)
	if name == "" {
		return merge.OnConflictMerge, nil
	}
	mode, ok := merge.ModeNamed(name)
	if !ok {
		return "", fmt.Errorf("invalid on_conflict %q: expected \"merge\" or \"fail\"", name)
	}
	return mode, nil
}
