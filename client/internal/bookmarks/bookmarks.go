// Package bookmarks provides client-side bookmark storage as a markdown file.
//
// Bookmarks are stored as a markdown list in ~/.mark/bookmarks.md:
//
//	# Bookmarks
//
//	- [Document Title](mark://host:6309/path.md) — 2026-03-05
//	- [Another Doc](mark://other:6309/doc.md) — 2026-03-05
//
// This format is both human-readable and publishable to a demarkus server,
// making your bookmark collection a personal hub.
package bookmarks

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/latebit-io/demarkus/client/internal/filelock"
	"github.com/latebit-io/demarkus/client/links"
)

// linkRe matches markdown list items: - [title](url) with optional — date suffix.
var linkRe = regexp.MustCompile(`^- \[((?:[^\]\\]|\\.)+)\]\((.+?)\)(?:\s*—\s*(\S+))?`)

// Bookmark represents a single bookmarked document.
type Bookmark struct {
	Title string
	URL   string
	Date  string
}

// Store manages bookmarks persisted as a markdown file. It keeps every line
// of the file, so what a person wrote around the list survives a rewrite.
type Store struct {
	path  string
	lines []line
}

// line is one line of the file: a bookmark, or text kept as written.
type line struct {
	raw string
	bm  *Bookmark // nil for text; URL is canonical
}

const (
	header = "# Bookmarks"
	// emptyHint is shown for an empty list; never stored, dropped when read.
	emptyHint = "No bookmarks yet. Press `b` on any page to bookmark it."
)

// DefaultPath returns the default bookmarks file path (~/.mark/bookmarks.md).
func DefaultPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".mark", "bookmarks.md")
}

// Load reads bookmarks from a markdown file. Returns an empty store if the
// file does not exist yet. Returns an error if path is empty.
func Load(path string) (*Store, error) {
	if path == "" {
		return nil, fmt.Errorf("bookmarks file path is empty (could not determine home directory)")
	}
	s := &Store{path: path}
	if err := s.reload(); err != nil {
		return nil, err
	}
	return s, nil
}

// reload replaces the in-memory lines with the file's; an absent file is empty.
func (s *Store) reload() error {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			s.lines = nil
			return nil
		}
		return fmt.Errorf("read bookmarks file %q: %w", s.path, err)
	}
	s.lines = parse(string(data))
	return nil
}

func parse(content string) []line {
	if content == "" {
		return nil
	}
	var lines []line
	// Only the last terminator goes: blank lines a person left at the end stay.
	for raw := range strings.SplitSeq(strings.TrimSuffix(content, "\n"), "\n") {
		if raw == emptyHint {
			continue
		}
		l := line{raw: raw}
		if m := linkRe.FindStringSubmatch(raw); m != nil {
			l.bm = &Bookmark{Title: unescapeTitle(m[1]), URL: links.CanonicalURL(m[2]), Date: m[3]}
		}
		lines = append(lines, l)
	}
	return lines
}

// List returns all bookmarks. A file from before identity keys may hold one
// document twice; the first wins.
func (s *Store) List() []Bookmark {
	var out []Bookmark
	for _, l := range s.lines {
		if l.bm != nil && !slices.ContainsFunc(out, func(b Bookmark) bool { return b.URL == l.bm.URL }) {
			out = append(out, *l.bm)
		}
	}
	return out
}

// Has reports whether the document is bookmarked, under any spelling of its URL.
func (s *Store) Has(url string) bool {
	return s.hasCanonical(links.CanonicalURL(url))
}

func (s *Store) hasCanonical(url string) bool {
	return slices.ContainsFunc(s.lines, func(l line) bool { return l.marks(url) })
}

// marks reports the line that bookmarks the canonical url.
func (l line) marks(url string) bool { return l.bm != nil && l.bm.URL == url }

// with is lines plus a bookmark of the canonical url, dated today. It goes
// after the last bookmark, so text a person wrote below the list stays below.
func with(lines []line, url, title string) []line {
	if len(lines) == 0 {
		lines = []line{{raw: header}, {raw: ""}}
	}
	bm := Bookmark{Title: title, URL: url, Date: time.Now().Format("2006-01-02")}
	at := len(lines)
	for i, l := range slices.Backward(lines) {
		if l.bm != nil {
			at = i + 1
			break
		}
	}
	return slices.Insert(slices.Clone(lines), at, line{raw: bm.markdown(), bm: &bm})
}

// without is lines less the bookmark of the canonical url, nil when there is none.
func without(lines []line, url string) []line {
	kept := slices.DeleteFunc(slices.Clone(lines), func(l line) bool { return l.marks(url) })
	if len(kept) == len(lines) {
		return nil
	}
	return kept
}

// Add appends a bookmark. If the URL is already bookmarked, this is a no-op.
func (s *Store) Add(url, title string) error {
	url = links.CanonicalURL(url)
	return s.mutate(func(lines []line) []line {
		if without(lines, url) != nil {
			return nil
		}
		return with(lines, url, title)
	})
}

// Remove deletes the bookmark with the given URL and writes to disk.
func (s *Store) Remove(url string) error {
	url = links.CanonicalURL(url)
	return s.mutate(func(lines []line) []line { return without(lines, url) })
}

// Toggle removes the bookmark of url, or adds it under title when there is
// none, and reports which. It decides on the file as it is now, under the
// lock: another process may have changed it since this store last read it.
func (s *Store) Toggle(url, title string) (added bool, err error) {
	url = links.CanonicalURL(url)
	err = s.mutate(func(lines []line) []line {
		if kept := without(lines, url); kept != nil {
			return kept
		}
		added = true
		return with(lines, url, title)
	})
	return added, err
}

// mutate applies change to the file as it is now, under the file's lock, so
// another process's bookmarks are kept, and writes the result. change returns
// nil for no change. Memory follows the disk: a failed write leaves it as is.
func (s *Store) mutate(change func([]line) []line) error {
	path, err := target(s.path)
	if err != nil {
		return fmt.Errorf("resolve bookmarks file %q: %w", s.path, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create bookmarks directory: %w", err)
	}
	return filelock.With(path, func() error {
		if err := s.reload(); err != nil {
			return err
		}
		next := change(s.lines)
		if next == nil {
			return nil
		}
		if err := write(path, next); err != nil {
			return err
		}
		s.lines = next
		return nil
	})
}

// target is the file path names, a symlink followed: the link stays, and
// every name of the file locks and replaces the same one.
func target(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil || !errors.Is(err, fs.ErrNotExist) {
		return resolved, err
	}
	// Absent, or a link to a file not written yet.
	link, err := os.Readlink(path)
	if err != nil {
		return path, nil //nolint:nilerr // not a link: the path itself is the file to create
	}
	if !filepath.IsAbs(link) {
		link = filepath.Join(filepath.Dir(path), link)
	}
	return link, nil
}

func (b *Bookmark) markdown() string {
	out := fmt.Sprintf("- [%s](%s)", escapeTitle(b.Title), b.URL)
	if b.Date != "" {
		out += " — " + b.Date
	}
	return out
}

// Render returns the bookmarks as a markdown document: the list alone, for
// a reader; the file keeps whatever else it holds.
func (s *Store) Render() string {
	var sb strings.Builder
	sb.WriteString(header + "\n\n")
	list := s.List()
	for i := range list {
		sb.WriteString(list[i].markdown() + "\n")
	}
	if len(list) == 0 {
		sb.WriteString(emptyHint + "\n")
	}
	return sb.String()
}

// escapeTitle escapes backslashes and ] so titles don't break markdown link syntax.
func escapeTitle(t string) string {
	t = strings.ReplaceAll(t, `\`, `\\`)
	t = strings.ReplaceAll(t, "]", `\]`)
	return t
}

// unescapeTitle reverses escapeTitle.
func unescapeTitle(t string) string {
	t = strings.ReplaceAll(t, `\]`, "]")
	t = strings.ReplaceAll(t, `\\`, `\`)
	return t
}

// write replaces the file at path with lines through a temp file and a
// rename, so a crash leaves the old file or the new one, never a truncated
// one. An existing file keeps its mode: another reader may depend on it.
func write(path string, lines []line) (err error) {
	var content strings.Builder
	for _, l := range lines {
		content.WriteString(l.raw + "\n")
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("write bookmarks file: %w", err)
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, removeTemp(tmp.Name()))
		}
	}()
	if info, statErr := os.Stat(path); statErr == nil {
		if err := tmp.Chmod(info.Mode().Perm()); err != nil {
			return fmt.Errorf("write bookmarks file: %w", errors.Join(err, tmp.Close()))
		}
	} else if !errors.Is(statErr, fs.ErrNotExist) {
		return fmt.Errorf("write bookmarks file: %w", errors.Join(statErr, tmp.Close()))
	}
	if _, err := tmp.WriteString(content.String()); err != nil {
		return fmt.Errorf("write bookmarks file: %w", errors.Join(err, tmp.Close()))
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write bookmarks file: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("write bookmarks file: %w", err)
	}
	return nil
}

func removeTemp(name string) error {
	if err := os.Remove(name); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
