package merge

import (
	"fmt"
	"strings"
)

// on_conflict values a publish accepts.
const (
	OnConflictMerge = "merge"
	OnConflictFail  = "fail"
)

// ParseOnConflict normalizes a tool's on_conflict argument: blank means
// merge, which MCP clients commonly send for an omitted optional field.
func ParseOnConflict(raw string) (string, error) {
	mode := strings.TrimSpace(raw)
	if mode == "" {
		return OnConflictMerge, nil
	}
	if mode != OnConflictMerge && mode != OnConflictFail {
		return "", fmt.Errorf("invalid on_conflict %q: expected \"merge\" or \"fail\"", mode)
	}
	return mode, nil
}
