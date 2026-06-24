package registry

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
)

func mcpConfigPath() (string, error) {
	h, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(h, ".config", "mcp", "mcp.json"), nil
}

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
	return atomicWrite(path, append(out, '\n'))
}

func servers(m map[string]any) map[string]any { return m["mcpServers"].(map[string]any) }

// McpAdd registers a stdio MCP server (a command + optional args).
func McpAdd(name, command string, args []string) error {
	path, err := mcpConfigPath()
	if err != nil {
		return err
	}
	return withLock(path, func() error {
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

// McpAddHTTP registers an HTTP/broker MCP server by URL (OAuth auto-detected).
func McpAddHTTP(name, url string) error {
	path, err := mcpConfigPath()
	if err != nil {
		return err
	}
	return withLock(path, func() error {
		m, err := loadMcp(path)
		if err != nil {
			return err
		}
		servers(m)[name] = map[string]any{"url": url, "auth": "oauth"}
		return saveMcp(path, m)
	})
}

// McpRemove deletes a server entry; reports whether it existed.
func McpRemove(name string) (bool, error) {
	path, err := mcpConfigPath()
	if err != nil {
		return false, err
	}
	existed := false
	err = withLock(path, func() error {
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

// McpList returns the configured server names (sorted).
func McpList() ([]string, error) {
	path, err := mcpConfigPath()
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
