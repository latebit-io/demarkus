package project

import (
	"errors"
	"io/fs"
	"os"
	"strings"
)

// gitValues are the local config values identity reads.
type gitValues struct {
	override    string // demarkus.project, last value
	hasOverride bool
	origin      string // remote.origin.url, first value
	bare        bool   // core.bare
	worktree    string // core.worktree: the checkout of a git dir kept elsewhere
}

// readGitConfig reads a repository's own config file; an absent file has no
// values. Includes are not followed, and no other scope is ever read: a
// global demarkus.project would fold every project into one root.
func readGitConfig(path string) (gitValues, error) {
	b, err := os.ReadFile(path) //nolint:gosec // a repository's config, parsed for fixed keys and never echoed
	if errors.Is(err, fs.ErrNotExist) {
		return gitValues{}, nil
	}
	if err != nil {
		return gitValues{}, err
	}
	return parseGitConfig(string(b)), nil
}

// parseGitConfig follows git's config grammar far enough for a few keys:
// case-insensitive sections and keys, case-sensitive subsections, quoted
// values, backslash escapes and continuations, # and ; comments.
func parseGitConfig(text string) gitValues {
	var v gitValues
	var section, sub string
	for text != "" {
		var line string
		line, text, _ = strings.Cut(text, "\n")
		ln := trimLead(line)
		if isBlank(ln) {
			continue
		}
		if ln[0] == '[' {
			var rest string
			section, sub, rest = parseSection(ln)
			if ln = trimLead(rest); isBlank(ln) {
				continue
			}
		}
		key, rest := cutKey(ln)
		value, valued := "", false
		if rest = trimLead(rest); strings.HasPrefix(rest, "=") {
			valued = true
			value, text = parseValue(rest[1:], text)
		}
		switch {
		case section == "demarkus" && sub == "" && key == "project":
			v.override, v.hasOverride = value, true
		case section == "remote" && sub == "origin" && key == "url" && v.origin == "":
			v.origin = value
		case section == "core" && sub == "" && key == "bare":
			v.bare = !valued || isTrue(value) // a bare key is true
		case section == "core" && sub == "" && key == "worktree":
			v.worktree = value
		}
	}
	return v
}

// isTrue reads a boolean the way git does.
func isTrue(value string) bool {
	switch strings.ToLower(value) {
	case "true", "yes", "on", "1":
		return true
	}
	return false
}

func trimLead(s string) string {
	return strings.TrimLeft(strings.TrimSuffix(s, "\r"), " \t")
}

func isBlank(ln string) bool { return ln == "" || ln[0] == '#' || ln[0] == ';' }

// parseSection reads `[name]`, `[name "sub"]` or the legacy `[name.sub]`,
// whose subsection git lowercases; rest is the text after the bracket. A
// header that never closes names no section, as git rejects it.
func parseSection(ln string) (section, sub, rest string) {
	i := 1
	for i < len(ln) && !strings.ContainsRune("] \t\"", rune(ln[i])) {
		i++
	}
	section = strings.ToLower(ln[1:i])
	if name, legacy, ok := strings.Cut(section, "."); ok {
		section, sub = name, legacy
	}
	for i < len(ln) && (ln[i] == ' ' || ln[i] == '\t') {
		i++
	}
	if i < len(ln) && ln[i] == '"' {
		var b strings.Builder
		for i++; i < len(ln) && ln[i] != '"'; i++ {
			if ln[i] == '\\' && i+1 < len(ln) {
				i++
			}
			b.WriteByte(ln[i])
		}
		sub = b.String()
		i++ // the closing quote
	}
	if i >= len(ln) || ln[i] != ']' {
		return "", "", ""
	}
	return section, sub, ln[i+1:]
}

// cutKey splits a variable line at the end of its name, lowercased.
func cutKey(ln string) (key, rest string) {
	i := 0
	for i < len(ln) && (ln[i] == '-' || ln[i] >= '0' && ln[i] <= '9' || ln[i]|0x20 >= 'a' && ln[i]|0x20 <= 'z') {
		i++
	}
	return strings.ToLower(ln[:i]), ln[i:]
}

// parseValue reads a value from s, the text after "=" on its line. A
// trailing backslash continues it onto the next line of rest, returned less
// what was consumed. Unquoted trailing space is dropped.
func parseValue(s, rest string) (value, remaining string) {
	var b strings.Builder
	quoted, keep := false, 0 // keep: length that survives the trailing trim
	s = strings.TrimLeft(s, " \t")
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '\\' && i+1 == len(s):
			if rest == "" {
				return b.String()[:keep], ""
			}
			s, rest, _ = strings.Cut(rest, "\n")
			s, i = strings.TrimSuffix(s, "\r"), -1
		case c == '\\':
			i++
			switch s[i] {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'b':
				b.WriteByte('\b')
			default:
				b.WriteByte(s[i])
			}
			keep = b.Len()
		case c == '"':
			quoted = !quoted
			keep = b.Len()
		case !quoted && (c == '#' || c == ';'):
			return b.String()[:keep], rest
		default:
			b.WriteByte(c)
			if quoted || c != ' ' && c != '\t' {
				keep = b.Len()
			}
		}
	}
	return b.String()[:keep], rest
}
