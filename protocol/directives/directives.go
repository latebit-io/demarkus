// Package directives reads the `key: value` lines a well-known document
// carries in its prose (policy.md, template.md): the first line of each key
// wins, wherever it sits in the body.
package directives

import "strings"

// First returns the first value of every `key: value` line. A key is the
// text before the first colon with no whitespace in it; spaces and tabs may
// lead the line, other whitespace may not.
func First(body string) map[string]string {
	out := map[string]string{}
	for line := range strings.SplitSeq(body, "\n") {
		key, value, ok := strings.Cut(strings.TrimLeft(line, " \t"), ":")
		if !ok || key == "" || strings.ContainsAny(key, " \t\v\f\r") {
			continue
		}
		if _, seen := out[key]; !seen {
			out[key] = strings.TrimSpace(value)
		}
	}
	return out
}
