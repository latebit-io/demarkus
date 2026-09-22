// Package promote keeps the plain remote promote targets and lists every
// promote destination, brokered or plain.
package promote

import (
	"fmt"
	pathpkg "path"
	"strings"
	"unicode"

	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/registry/statefile"

	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/config"
)

// AddTarget registers a plain remote promote target "<slug> <path> [label]".
// It returns the canonical stored path.
func AddTarget(slug, path, label string) (string, error) {
	if slug == "" {
		return "", fmt.Errorf("add: missing <slug>")
	}
	path, err := canonicalPromotePath(path)
	if err != nil {
		return "", err
	}
	if !statefile.ValidSlug(slug) {
		return "", fmt.Errorf("add: <slug> '%s' has unexpected characters", slug)
	}
	if err := statefile.ValidateField("promote label", label); err != nil {
		return "", err
	}
	p, err := config.StatePath("promote-targets")
	if err != nil {
		return "", err
	}
	err = statefile.WithLock(p, func() error {
		rows, err := config.Records("promote-targets")
		if err != nil {
			return err
		}
		for i, row := range rows {
			fields := strings.SplitN(row, " ", 3)
			if len(fields) < 2 || fields[0] != slug {
				continue
			}
			existingPath, pathErr := canonicalPromotePath(fields[1])
			if pathErr != nil || existingPath != path {
				continue
			}
			if fields[1] == path {
				return nil // idempotent on slug+canonical path
			}
			rows[i] = slug + " " + path
			if len(fields) == 3 {
				rows[i] += " " + fields[2]
			}
			return statefile.Write(p, []byte(strings.Join(rows, "\n")+"\n"))
		}
		line := slug + " " + path
		if label != "" {
			line += " " + label
		}
		rows = append(rows, line)
		return statefile.Write(p, []byte(strings.Join(rows, "\n")+"\n"))
	})
	return path, err
}

func canonicalPromotePath(raw string) (string, error) {
	if !strings.HasPrefix(raw, "/") {
		return "", fmt.Errorf("add: <path> must start with / (got '%s')", raw)
	}
	if strings.ContainsAny(raw, "\\\x00") || strings.ContainsFunc(raw, unicode.IsSpace) {
		return "", fmt.Errorf("add: <path> must not contain whitespace, backslashes, or NUL")
	}
	for segment := range strings.SplitSeq(raw, "/") {
		if segment == "." || segment == ".." {
			return "", fmt.Errorf("add: <path> must not contain traversal segments")
		}
	}
	return pathpkg.Clean(raw), nil
}

// Detect lists every promote destination (brokered + plain), or nil when
// none. "knowledge <slug>" / "target <slug> <path> [label]".
func Detect() ([]string, error) {
	var out []string
	systems, err := config.ListKnowledgeSystems()
	if err != nil {
		return nil, err
	}
	for _, s := range systems {
		out = append(out, "knowledge "+s)
	}
	targets, err := config.PromoteTargets()
	if err != nil {
		return nil, err
	}
	for _, t := range targets {
		out = append(out, "target "+t)
	}
	return out, nil
}
