// Package cache provides local file-based caching for Mark Protocol responses.
package cache

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/protocol"
)

// DemarkusCacheDir is the environment variable for overriding the cache directory.
const DemarkusCacheDir = "DEMARKUS_CACHE_DIR"

// Cache stores Mark Protocol responses on the local filesystem.
type Cache struct {
	Dir string
}

// Entry is the client's cached response type; Cache implements fetch.ResponseCache.
type Entry = fetch.CachedResponse

var _ fetch.ResponseCache = (*Cache)(nil)

// meta is the TOML-serializable cache metadata.
type meta struct {
	URL      string            `toml:"url"` // dial address the entry was read from; a label, never a node identity
	Verb     string            `toml:"verb"`
	Status   string            `toml:"status"`
	CachedAt time.Time         `toml:"cached_at"`
	Metadata map[string]string `toml:"metadata"`
}

// DefaultDir returns the default cache directory.
// It checks DEMARKUS_CACHE_DIR first, then falls back to ~/.mark/cache.
func DefaultDir() string {
	if dir := os.Getenv(DemarkusCacheDir); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".", "."+protocol.ALPN, "cache")
	}
	return filepath.Join(home, "."+protocol.ALPN, "cache")
}

// New creates a cache rooted at the given directory.
func New(dir string) *Cache {
	return &Cache{Dir: dir}
}

// renameFile is os.Rename; tests replace it to interrupt a Put.
var renameFile = os.Rename

// Put replaces body then metadata, each by rename. A crash in between leaves
// the old etag over the new body, which revalidates; the reverse never would.
func (c *Cache) Put(host, path, verb string, resp protocol.Response) error {
	filePath := c.filePath(host, path, verb)

	if err := c.ensureDir(filepath.Dir(filePath)); err != nil {
		return err
	}

	m := meta{
		URL:      protocol.ALPN + "://" + host + path,
		Verb:     verb,
		Status:   resp.Status,
		CachedAt: time.Now().UTC(),
		Metadata: resp.Metadata,
	}
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(m); err != nil {
		return fmt.Errorf("encode cache metadata %s: %w", filePath, err)
	}

	if err := replaceFile(filePath, []byte(resp.Body)); err != nil {
		return err
	}
	return replaceFile(filePath+".meta", buf.Bytes())
}

// ensureDir creates dir; a flat-file entry from the old layout in the way is removed.
func (c *Cache) ensureDir(dir string) error {
	err := os.MkdirAll(dir, 0o755)
	if err == nil {
		return nil
	}
	if os.Remove(dir) != nil {
		return fmt.Errorf("create cache dir %s: %w", dir, err)
	}
	if rmErr := os.Remove(dir + ".meta"); rmErr != nil && !os.IsNotExist(rmErr) {
		return fmt.Errorf("remove stale cache metadata %s.meta: %w", dir, rmErr)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create cache dir %s: %w", dir, err)
	}
	return nil
}

// replaceFile writes data to a temp file beside path and renames it into place.
// The file stays 0600: a cached body may have been fetched with a token.
func replaceFile(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".put-*.tmp")
	if err != nil {
		return fmt.Errorf("create cache temp file for %s: %w", path, err)
	}
	if _, err := tmp.Write(data); err != nil {
		return errors.Join(fmt.Errorf("write cache file %s: %w", path, err), tmp.Close(), removeTemp(tmp.Name()))
	}
	if err := tmp.Close(); err != nil {
		return errors.Join(fmt.Errorf("close cache file %s: %w", path, err), removeTemp(tmp.Name()))
	}
	if err := renameFile(tmp.Name(), path); err != nil {
		return errors.Join(fmt.Errorf("replace cache file %s: %w", path, err), removeTemp(tmp.Name()))
	}
	return nil
}

func removeTemp(name string) error {
	if err := os.Remove(name); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove temp file %s: %w", name, err)
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
				_ = os.Remove(filePath)
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
		_ = os.Remove(metaPath)
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
		sentinel = "." + strings.ToLower(protocol.VerbList)
	default:
		sentinel = "." + strings.ToLower(protocol.VerbFetch)
	}

	return filepath.Join(c.Dir, safeHost, cleaned, sentinel)
}
