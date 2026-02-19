// Package cache provides local file-based caching for Mark Protocol responses.
package cache

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/latebit/demarkus/protocol"
)

// Cache stores Mark Protocol responses on the local filesystem.
type Cache struct {
	Dir string
}

// Entry is a cached response with metadata about when it was stored.
type Entry struct {
	Response protocol.Response
	CachedAt time.Time
}

// meta is the TOML-serializable cache metadata.
type meta struct {
	URL      string            `toml:"url"`
	Verb     string            `toml:"verb"`
	Status   string            `toml:"status"`
	CachedAt time.Time         `toml:"cached_at"`
	Metadata map[string]string `toml:"metadata"`
}

// DefaultDir returns the default cache directory.
// It checks DEMARKUS_CACHE_DIR first, then falls back to ~/.mark/cache.
func DefaultDir() string {
	if dir := os.Getenv("DEMARKUS_CACHE_DIR"); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".", ".mark", "cache")
	}
	return filepath.Join(home, ".mark", "cache")
}

// New creates a cache rooted at the given directory.
func New(dir string) *Cache {
	return &Cache{Dir: dir}
}

// Put writes a response to the cache atomically.
// Writes metadata first (which is smaller), then body. This ensures
// if we crash, we don't have orphaned body files without metadata.
func (c *Cache) Put(host, path, verb string, resp protocol.Response) error {
	filePath := c.filePath(host, path, verb)
	metaPath := filePath + ".meta"

	dir := filepath.Dir(filePath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		// A stale flat-file cache entry may block directory creation.
		// Remove it and retry once.
		if os.Remove(dir) == nil {
			// Also remove the companion .meta file from the old layout.
			os.Remove(dir + ".meta")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return err
			}
		} else {
			return err
		}
	}

	m := meta{
		URL:      "mark://" + host + path,
		Verb:     verb,
		Status:   resp.Status,
		CachedAt: time.Now().UTC(),
		Metadata: resp.Metadata,
	}

	// Write metadata first (atomic order for crash safety).
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(m); err != nil {
		return err
	}
	if err := os.WriteFile(metaPath, buf.Bytes(), 0o644); err != nil {
		return err
	}

	// Then write body. If this fails, metadata still exists as a marker.
	if err := os.WriteFile(filePath, []byte(resp.Body), 0o644); err != nil {
		// Best effort cleanup if body write fails.
		os.Remove(metaPath)
		return err
	}

	return nil
}

// Get reads a cached response. Returns nil if not cached.
// If cache files are inconsistent (metadata missing but body exists),
// cleans up the orphaned body and returns nil.
func (c *Cache) Get(host, path, verb string) (*Entry, error) {
	filePath := c.filePath(host, path, verb)
	metaPath := filePath + ".meta"

	// Try to read metadata first (it's required).
	var m meta
	if _, err := toml.DecodeFile(metaPath, &m); err != nil {
		if os.IsNotExist(err) {
			// Metadata missing. Check if body exists (corrupted cache).
			if _, err := os.Stat(filePath); err == nil {
				// Body exists but metadata doesn't — clean it up.
				os.Remove(filePath)
			}
			return nil, nil
		}
		// Metadata unreadable for other reasons, treat as miss.
		return nil, nil
	}

	// Metadata exists, now read body.
	body, err := os.ReadFile(filePath)
	if os.IsNotExist(err) {
		// Body missing but metadata exists (corrupted cache). Clean up metadata.
		os.Remove(metaPath)
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	return &Entry{
		Response: protocol.Response{
			Status:   m.Status,
			Metadata: m.Metadata,
			Body:     string(body),
		},
		CachedAt: m.CachedAt,
	}, nil
}

// filePath returns the cache file path for a given host, request path, and verb.
//
// Each path gets its own directory with verb-specific sentinel files inside,
// so FETCH and LIST for the same path never collide on the filesystem:
//
//	cache/host/index.md/.fetch      ← FETCH /index.md
//	cache/host/index.md/.list       ← LIST  /index.md
//	cache/host/.fetch               ← FETCH /
//	cache/host/.list                ← LIST  /
func (c *Cache) filePath(host, reqPath, verb string) string {
	safeHost := strings.ReplaceAll(host, "..", "_")
	safeHost = strings.ReplaceAll(safeHost, string(filepath.Separator), "_")

	cleaned := filepath.Clean(reqPath)
	cleaned = strings.TrimLeft(cleaned, "/")
	if cleaned == "." {
		cleaned = ""
	}

	var sentinel string
	switch verb {
	case protocol.VerbList:
		sentinel = ".list"
	default:
		sentinel = ".fetch"
	}

	return filepath.Join(c.Dir, safeHost, cleaned, sentinel)
}
