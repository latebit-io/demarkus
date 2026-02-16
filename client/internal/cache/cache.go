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

// New creates a cache rooted at the given directory.
func New(dir string) *Cache {
	return &Cache{Dir: dir}
}

// Put writes a response to the cache.
func (c *Cache) Put(host, path, verb string, resp protocol.Response) error {
	filePath := c.filePath(host, path, verb)
	metaPath := filePath + ".meta"

	if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
		return err
	}

	if err := os.WriteFile(filePath, []byte(resp.Body), 0o644); err != nil {
		return err
	}

	m := meta{
		URL:      "mark://" + host + path,
		Verb:     verb,
		Status:   resp.Status,
		CachedAt: time.Now().UTC(),
		Metadata: resp.Metadata,
	}

	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(m); err != nil {
		return err
	}
	return os.WriteFile(metaPath, buf.Bytes(), 0o644)
}

// Get reads a cached response. Returns nil if not cached.
func (c *Cache) Get(host, path, verb string) (*Entry, error) {
	filePath := c.filePath(host, path, verb)
	metaPath := filePath + ".meta"

	body, err := os.ReadFile(filePath)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	var m meta
	if _, err := toml.DecodeFile(metaPath, &m); err != nil {
		return nil, nil
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
func (c *Cache) filePath(host, reqPath, verb string) string {
	safeHost := strings.ReplaceAll(host, "..", "_")
	safeHost = strings.ReplaceAll(safeHost, string(filepath.Separator), "_")

	cleaned := filepath.Clean(reqPath)
	cleaned = strings.TrimLeft(cleaned, "/")

	switch {
	case verb == protocol.VerbList:
		cleaned = filepath.Join(cleaned, ".list")
	case cleaned == "" || cleaned == ".":
		// Root FETCH — use a sentinel filename to avoid colliding with the directory.
		cleaned = ".index"
	}

	return filepath.Join(c.Dir, safeHost, cleaned)
}
