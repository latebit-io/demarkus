// Package mcpconfig edits the MCP server config file of a harness that reads
// one on load (pi-mcp-adapter, Cursor); the others register servers themselves.
package mcpconfig

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/registry/statefile"

	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/host"
)

// loadMcp reads ~/.config/mcp/mcp.json, normalizing to a {mcpServers:{}} object.
// Array-shaped JSON (a top-level [] or array mcpServers) is rejected to an empty
// object so a named property can't be silently dropped by Marshal.
func loadMcp(path string) (map[string]any, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]any{"mcpServers": map[string]any{}}, nil
		}
		return nil, err
	}
	var obj any
	if json.Unmarshal(b, &obj) != nil {
		return nil, errors.New(path + " is not valid JSON; fix it by hand before retrying")
	}
	// Refuse to clobber a malformed-but-present config: a top-level array/scalar
	// or an array-valued mcpServers is surfaced as an error rather than silently
	// reset (which would drop the user's existing servers on the next write).
	m, ok := obj.(map[string]any)
	if !ok {
		return nil, errors.New(path + " is not a JSON object; fix it by hand before retrying")
	}
	if raw, present := m["mcpServers"]; present {
		srv, ok := raw.(map[string]any)
		if !ok {
			return nil, errors.New(path + ": mcpServers is not a JSON object; fix it by hand before retrying")
		}
		m["mcpServers"] = srv
	} else {
		m["mcpServers"] = map[string]any{}
	}
	return m, nil
}

func saveMcp(path string, m map[string]any) error {
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	// The config is the user's file: write through a symlink to its target and
	// keep the existing mode, which may be 0600 because other servers' keys live there.
	target, perm := path, os.FileMode(0o644)
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		target = resolved
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("resolve %s: %w", path, err)
	}
	if info, err := os.Stat(target); err == nil {
		perm = info.Mode().Perm()
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat %s: %w", target, err)
	}
	return statefile.WriteFile(target, append(out, '\n'), perm)
}

func servers(m map[string]any) map[string]any { return m["mcpServers"].(map[string]any) }

// Add registers a stdio MCP server (a command + optional args) in h's config.
func Add(h host.Host, name, command string, args []string) error {
	path, err := h.McpConfigPath()
	if err != nil {
		return err
	}
	return statefile.WithLock(path, func() error {
		m, err := loadMcp(path)
		if err != nil {
			return err
		}
		entry := map[string]any{"command": command}
		if len(args) > 0 {
			entry["args"] = args
		}
		servers(m)[name] = entry
		return saveMcp(path, m)
	})
}

// AddHTTP registers an HTTP/broker MCP server by URL (OAuth auto-detected).
func AddHTTP(h host.Host, name, url string) error {
	path, err := h.McpConfigPath()
	if err != nil {
		return err
	}
	return statefile.WithLock(path, func() error {
		m, err := loadMcp(path)
		if err != nil {
			return err
		}
		servers(m)[name] = h.HTTPEntry(url)
		return saveMcp(path, m)
	})
}

// Remove deletes a server entry; reports whether it existed.
func Remove(h host.Host, name string) (bool, error) {
	path, err := h.McpConfigPath()
	if err != nil {
		return false, err
	}
	existed := false
	err = statefile.WithLock(path, func() error {
		m, err := loadMcp(path)
		if err != nil {
			return err
		}
		s := servers(m)
		if _, ok := s[name]; ok {
			delete(s, name)
			existed = true
			return saveMcp(path, m)
		}
		return nil
	})
	return existed, err
}

// List returns the configured server names (sorted).
func List(h host.Host) ([]string, error) {
	path, err := h.McpConfigPath()
	if err != nil {
		return nil, err
	}
	m, err := loadMcp(path)
	if err != nil {
		return nil, err
	}
	var names []string
	for k := range servers(m) {
		names = append(names, k)
	}
	sort.Strings(names)
	return names, nil
}
