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
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// linkRe matches markdown list items: - [title](url) with optional — date suffix.
var linkRe = regexp.MustCompile(`^- \[((?:[^\]\\]|\\.)+)\]\((.+?)\)(?:\s*—\s*(\S+))?`)

// Bookmark represents a single bookmarked document.
type Bookmark struct {
	Title string
	URL   string
	Date  string
}

// Store manages bookmarks persisted as a markdown file.
type Store struct {
	path      string
	bookmarks []Bookmark
}

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
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, fmt.Errorf("read bookmarks file %q: %w", path, err)
	}
	s.parse(string(data))
	return s, nil
}

func (s *Store) parse(content string) {
	for line := range strings.SplitSeq(content, "\n") {
		m := linkRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		s.bookmarks = append(s.bookmarks, Bookmark{
			Title: unescapeTitle(m[1]),
			URL:   m[2],
			Date:  m[3],
		})
	}
}

// List returns all bookmarks.
func (s *Store) List() []Bookmark {
	return s.bookmarks
}

// Has returns true if the given URL is bookmarked.
func (s *Store) Has(url string) bool {
	for _, b := range s.bookmarks {
		if b.URL == url {
			return true
		}
	}
	return false
}

// Add appends a bookmark. If the URL is already bookmarked, this is a no-op.
func (s *Store) Add(url, title string) error {
	if s.Has(url) {
		return nil
	}
	date := time.Now().Format("2006-01-02")
	s.bookmarks = append(s.bookmarks, Bookmark{
		Title: title,
		URL:   url,
		Date:  date,
	})
	return s.save()
}

// Remove deletes the bookmark with the given URL and writes to disk.
func (s *Store) Remove(url string) error {
	filtered := make([]Bookmark, 0, len(s.bookmarks))
	for _, b := range s.bookmarks {
		if b.URL != url {
			filtered = append(filtered, b)
		}
	}
	if len(filtered) == len(s.bookmarks) {
		return nil
	}
	s.bookmarks = filtered
	return s.save()
}

// Render returns the bookmarks as a markdown document.
func (s *Store) Render() string {
	var sb strings.Builder
	sb.WriteString("# Bookmarks\n\n")
	for _, b := range s.bookmarks {
		sb.WriteString(fmt.Sprintf("- [%s](%s)", escapeTitle(b.Title), b.URL))
		if b.Date != "" {
			sb.WriteString(fmt.Sprintf(" — %s", b.Date))
		}
		sb.WriteString("\n")
	}
	if len(s.bookmarks) == 0 {
		sb.WriteString("No bookmarks yet. Press `b` on any page to bookmark it.\n")
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

func (s *Store) save() error {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create bookmarks directory: %w", err)
	}
	content := s.Render()
	if err := os.WriteFile(s.path, []byte(content), 0o644); err != nil {
		return fmt.Errorf("write bookmarks file: %w", err)
	}
	return nil
}
