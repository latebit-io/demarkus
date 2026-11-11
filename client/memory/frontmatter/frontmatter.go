// Package frontmatter reads the YAML frontmatter fence other tools write at
// the top of a markdown file. No I/O: the style gate and the OKF bundle
// reader both parse through it.
package frontmatter

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// Split splits a file written by someone else's tools, so it
// tolerates CRLF and reads an unclosed fence as "no frontmatter". That is why
// it is not protocol.SplitFrontmatter, which is the strict wire grammar.
func Split(data []byte) (fm string, body []byte, ok bool) {
	lines := strings.SplitAfter(string(data), "\n")
	if len(lines) == 0 || strings.TrimRight(lines[0], "\r\n") != "---" {
		return "", data, false
	}
	for i := 1; i < len(lines); i++ {
		if strings.TrimRight(lines[i], "\r\n") == "---" {
			return strings.Join(lines[1:i], ""), []byte(strings.Join(lines[i+1:], "")), true
		}
	}
	return "", data, false
}

// Parse reads a frontmatter block into a flat string map, each value decoded
// by Value. Any other line that is not "key: value" or a list
// item is an error, failing conformance.
func Parse(fm string) (map[string]string, error) {
	meta := map[string]string{}
	lastListKey := ""
	for raw := range strings.SplitSeq(fm, "\n") {
		line := strings.TrimRight(raw, "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if item, ok := blockListItem(trimmed); ok {
			if lastListKey == "" {
				return nil, fmt.Errorf("list item with no parent key: %q", trimmed)
			}
			meta[lastListKey] = appendCSV(meta[lastListKey], item)
			continue
		}
		key, val, found := strings.Cut(line, ":")
		if !found {
			return nil, fmt.Errorf("unparseable frontmatter line: %q", trimmed)
		}
		key = strings.TrimSpace(key)
		if key == "" {
			return nil, fmt.Errorf("empty key in frontmatter line: %q", trimmed)
		}
		v, bare := Value(strings.TrimSpace(val))
		meta[key] = v
		lastListKey = ""
		if bare {
			lastListKey = key // a bare "key:" may introduce a block list
		}
	}
	return meta, nil
}

func blockListItem(trimmed string) (string, bool) {
	if trimmed == "-" {
		return "", true
	}
	if strings.HasPrefix(trimmed, "- ") {
		v, _ := Value(strings.TrimSpace(trimmed[2:])) // a bare item is empty
		return v, true
	}
	return "", false
}

// Value decodes one frontmatter value as YAML: quotes, escapes and comments
// resolved, a list of scalars flattened to comma-separated items. bare reports
// no value at all, as in "key:" or "key: # comment".
func Value(raw string) (v string, bare bool) {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(raw), &doc); err != nil {
		return trimQuotes(raw), false // not YAML: import tolerates it, as written
	}
	if len(doc.Content) == 0 {
		return "", true
	}
	n := doc.Content[0]
	if n.Kind == yaml.ScalarNode {
		return n.Value, false
	}
	if items, ok := scalarItems(n); ok {
		return strings.Join(items, ","), false
	}
	// A mapping or nested list has no flat form: kept as YAML, comments dropped.
	dropComments(n)
	out, err := yaml.Marshal(n)
	if err != nil {
		return raw, false // a decoded node re-encodes; as written if not
	}
	return strings.TrimSpace(string(out)), false
}

// scalarItems returns a sequence's non-empty items, ok only if all are scalars.
func scalarItems(n *yaml.Node) ([]string, bool) {
	if n.Kind != yaml.SequenceNode {
		return nil, false
	}
	items := make([]string, 0, len(n.Content))
	for _, c := range n.Content {
		if c.Kind != yaml.ScalarNode {
			return nil, false
		}
		if c.Value != "" {
			items = append(items, c.Value)
		}
	}
	return items, true
}

func dropComments(n *yaml.Node) {
	n.HeadComment, n.LineComment, n.FootComment = "", "", ""
	for _, c := range n.Content {
		dropComments(c)
	}
}

func trimQuotes(s string) string {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

func appendCSV(existing, item string) string {
	switch {
	case item == "":
		return existing
	case existing == "":
		return item
	default:
		return existing + "," + item
	}
}
