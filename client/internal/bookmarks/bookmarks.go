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
	"slices"
	"strings"
	"time"

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
		// A file from before identity keys may hold one document twice; the first wins.
		url := links.CanonicalURL(m[2])
		if s.hasCanonical(url) {
			continue
		}
		s.bookmarks = append(s.bookmarks, Bookmark{
			Title: unescapeTitle(m[1]),
			URL:   url,
			Date:  m[3],
		})
	}
}

// List returns all bookmarks.
func (s *Store) List() []Bookmark {
	return s.bookmarks
}

// Has reports whether the document is bookmarked, under any spelling of its URL.
func (s *Store) Has(url string) bool {
	return s.hasCanonical(links.CanonicalURL(url))
}

func (s *Store) hasCanonical(url string) bool {
	return slices.ContainsFunc(s.bookmarks, func(b Bookmark) bool { return b.URL == url })
}

// Add appends a bookmark. If the URL is already bookmarked, this is a no-op.
func (s *Store) Add(url, title string) error {
	url = links.CanonicalURL(url)
	if s.hasCanonical(url) {
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
	url = links.CanonicalURL(url)
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
