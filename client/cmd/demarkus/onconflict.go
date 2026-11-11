package main

import (
	"fmt"
	"strings"

	"github.com/latebit-io/demarkus/client/merge"
)

// parseOnConflict reads -on-conflict; no value means merge.
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
