package config

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// The ~/.demarkus layout the managed server lifecycle shares.

// BinPath is the installed binary name under ~/.demarkus/bin.
func BinPath(name string) (string, error) { return path(filepath.Join("bin", name)) }

// TokenPath is the plugin's raw write token file.
func TokenPath() (string, error) { return path("plugin-memory.token") }

// SharedMemoryDir is the default mode's memory root.
func SharedMemoryDir() (string, error) { return path("soul") }

// IsolatedMemoryDir is the isolated mode's memory root.
func IsolatedMemoryDir() (string, error) { return path("plugin-soul") }

// memoryID is a stable per-memory identifier: basename for readability plus a
// path hash for uniqueness across memories.
func memoryID(memoryDir string) string {
	sum := sha256.Sum256([]byte(memoryDir))
	return filepath.Base(memoryDir) + "-" + hex.EncodeToString(sum[:4])
}

// ManagedServerLogPath places the managed server's log under ~/.demarkus/logs,
// never inside memoryDir, which the server watches for tokens.toml changes (#289).
func ManagedServerLogPath(memoryDir string) (string, error) {
	return path(filepath.Join("logs", "server-"+memoryID(memoryDir)+".log"))
}

// ManagedTokensPath is the out-of-root tokens.toml for memoryDir's managed
// server: token data does not belong inside the served content root (#289
// follow-up). Managed modes only; reuse mode keeps <root>/tokens.toml.
func ManagedTokensPath(memoryDir string) (string, error) {
	return path(filepath.Join("tokens", memoryID(memoryDir), "tokens.toml"))
}

// FileExists reports whether p is an existing file (not a directory).
func FileExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir()
}

// SaveConfig atomically writes plugin-memory.conf. Values are backslash-escaped
// (shellQuote), equivalent to bash printf '%q' for the simple path/word values we
// store, and reversed by unquoteShell on load.
func SaveConfig(memory string, port int, mode, tokensTOML string) error {
	h, err := Home()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(h, 0o755); err != nil {
		return err
	}
	cfg, err := path("plugin-memory.conf")
	if err != nil {
		return err
	}
	content := fmt.Sprintf(
		"# demarkus-memory plugin config: managed by demarkus-plugin provision\nSOUL_DIR=%s\nPORT=%d\nMODE=%s\n",
		shellQuote(memory), port, shellQuote(mode),
	)
	// Persisted so a later run (or VerifyAuth) with the server down still
	// targets the registry resolved at setup, not a conventional guess.
	if tokensTOML != "" {
		content += fmt.Sprintf("TOKENS=%s\n", shellQuote(tokensTOML))
	}
	tmp := cfg + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, cfg)
}

// shellQuote backslash-escapes the shell-special characters in s, matching what
// bash printf '%q' emits for the values we store; unquoteShell reverses it with
// a plain backslash-unescape, which is why the '…' form is not used.
func shellQuote(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\n'\"\\$`*?[]{}()&;|<>#~!") {
		return s
	}
	var b strings.Builder
	for _, r := range s {
		if strings.ContainsRune(" \t\n'\"\\$`*?[]{}()&;|<>#~!", r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}
