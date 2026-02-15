// Package handler serves Mark Protocol requests from a content directory.
package handler

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/latebit/demarkus/protocol"
)

// Handler serves markdown files from a content directory.
type Handler struct {
	ContentDir string
}

// Stream represents a bidirectional stream that can be read, written, and closed.
type Stream interface {
	io.ReadWriteCloser
}

// HandleStream reads a request from the stream and writes a response.
func (h *Handler) HandleStream(stream Stream) {
	defer stream.Close()

	req, err := protocol.ParseRequest(stream)
	if err != nil {
		log.Printf("bad request: %v", err)
		h.writeError(stream, protocol.StatusServerError, "bad request")
		return
	}

	log.Printf("%s %s", sanitize(req.Verb), sanitize(req.Path))

	switch req.Verb {
	case protocol.VerbFetch:
		h.handleFetch(stream, req)
	case protocol.VerbList:
		h.handleList(stream, req.Path)
	default:
		h.writeError(stream, protocol.StatusServerError, "unsupported verb: "+sanitize(req.Verb))
	}
}

// resolvePath validates and resolves a request path to an absolute filesystem path
// within the content directory. Returns empty string if the path escapes the root.
// Uses filepath.EvalSymlinks to prevent symlink escape attacks.
func (h *Handler) resolvePath(reqPath string) string {
	cleaned := filepath.Clean(reqPath)
	cleaned = strings.TrimLeft(cleaned, "/")
	joined := filepath.Join(h.ContentDir, cleaned)

	absRoot, err := filepath.Abs(h.ContentDir)
	if err != nil {
		log.Printf("failed to resolve content directory: %v", err)
		return ""
	}
	// Resolve symlinks in root for consistent comparison.
	resolved, err := filepath.EvalSymlinks(absRoot)
	if err != nil {
		log.Printf("failed to resolve content directory symlinks: %v", err)
		return ""
	}
	absRoot = resolved

	// Resolve symlinks in the target path to detect escapes.
	absPath, err := filepath.EvalSymlinks(joined)
	if err != nil {
		// Path doesn't exist yet — fall back to Abs for structural validation.
		absPath, err = filepath.Abs(joined)
		if err != nil {
			return ""
		}
	}
	// EvalSymlinks may return a relative path; ensure it's absolute.
	if !filepath.IsAbs(absPath) {
		absPath, err = filepath.Abs(absPath)
		if err != nil {
			return ""
		}
	}

	if absPath != absRoot && !strings.HasPrefix(absPath, absRoot+string(filepath.Separator)) {
		return ""
	}
	return absPath
}

func (h *Handler) handleFetch(w io.Writer, req protocol.Request) {
	filePath := h.resolvePath(req.Path)
	if filePath == "" {
		h.writeError(w, protocol.StatusNotFound, req.Path+" not found")
		return
	}

	info, err := os.Stat(filePath)
	if os.IsNotExist(err) {
		h.writeError(w, protocol.StatusNotFound, req.Path+" not found")
		return
	}
	if err != nil {
		log.Printf("stat error: %v", err)
		h.writeError(w, protocol.StatusServerError, "internal error")
		return
	}
	if info.IsDir() {
		h.writeError(w, protocol.StatusNotFound, req.Path+" is a directory")
		return
	}

	data, err := os.ReadFile(filePath)
	if err != nil {
		log.Printf("read error: %v", err)
		h.writeError(w, protocol.StatusServerError, "internal error")
		return
	}

	etag := computeEtag(data)
	modified := info.ModTime().UTC().Truncate(time.Second)

	// Check conditional: etag first, then modified-since.
	if ifNoneMatch, ok := req.Metadata["if-none-match"]; ok && ifNoneMatch == etag {
		h.writeNotModified(w)
		return
	}
	if ifModSince, ok := req.Metadata["if-modified-since"]; ok {
		if t, err := time.Parse(time.RFC3339, ifModSince); err == nil {
			if !modified.After(t) {
				h.writeNotModified(w)
				return
			}
		}
	}

	body, existingMeta := stripFrontmatter(string(data))

	meta := map[string]string{
		"modified": modified.Format(time.RFC3339),
		"etag":     etag,
	}
	if v, ok := existingMeta["version"]; ok {
		meta["version"] = v
	} else {
		meta["version"] = "1"
	}

	resp := protocol.Response{
		Status:   protocol.StatusOK,
		Metadata: meta,
		Body:     body,
	}
	resp.WriteTo(w)
}

func (h *Handler) writeNotModified(w io.Writer) {
	resp := protocol.Response{
		Status:   protocol.StatusNotModified,
		Metadata: map[string]string{},
	}
	resp.WriteTo(w)
}

func computeEtag(data []byte) string {
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

func (h *Handler) handleList(w io.Writer, reqPath string) {
	dirPath := h.resolvePath(reqPath)
	if dirPath == "" {
		h.writeError(w, protocol.StatusNotFound, reqPath+" not found")
		return
	}

	info, err := os.Stat(dirPath)
	if os.IsNotExist(err) {
		h.writeError(w, protocol.StatusNotFound, reqPath+" not found")
		return
	}
	if err != nil {
		log.Printf("stat error: %v", err)
		h.writeError(w, protocol.StatusServerError, "internal error")
		return
	}
	if !info.IsDir() {
		h.writeError(w, protocol.StatusNotFound, reqPath+" is not a directory")
		return
	}

	entries, err := os.ReadDir(dirPath)
	if err != nil {
		log.Printf("readdir error: %v", err)
		h.writeError(w, protocol.StatusServerError, "internal error")
		return
	}

	var body strings.Builder
	body.WriteString("\n# Index of " + escapeMD(reqPath) + "\n\n")

	entryCount := 0
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		entryCount++
		display := escapeMD(name)
		link := escapeURL(name)
		if entry.IsDir() {
			body.WriteString("- [" + display + "/](" + link + "/)\n")
		} else {
			body.WriteString("- [" + display + "](" + link + ")\n")
		}
	}

	resp := protocol.Response{
		Status: protocol.StatusOK,
		Metadata: map[string]string{
			"entries": fmt.Sprintf("%d", entryCount),
		},
		Body: body.String(),
	}
	resp.WriteTo(w)
}

func (h *Handler) writeError(w io.Writer, status, message string) {
	resp := protocol.Response{
		Status:   status,
		Metadata: map[string]string{},
		Body:     fmt.Sprintf("\n# %s\n\n%s\n", statusTitle(status), message),
	}
	resp.WriteTo(w)
}

// sanitize strips control characters from a string for safe logging.
func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 32 || r == 127 {
			return -1
		}
		return r
	}, s)
}

func statusTitle(s string) string {
	return strings.ToUpper(s[:1]) + strings.ReplaceAll(s[1:], "-", " ")
}

func stripFrontmatter(content string) (string, map[string]string) {
	meta := make(map[string]string)

	if !strings.HasPrefix(content, "---\n") {
		return content, meta
	}

	end := strings.Index(content[4:], "\n---\n")
	if end == -1 {
		return content, meta
	}

	fmBlock := content[4 : 4+end]
	for _, line := range strings.Split(fmBlock, "\n") {
		key, val, ok := strings.Cut(line, ": ")
		if ok {
			meta[strings.TrimSpace(key)] = strings.TrimSpace(val)
		}
	}

	body := content[4+end+5:]
	return body, meta
}

var mdReplacer = strings.NewReplacer(
	`\`, `\\`,
	`[`, `\[`, `]`, `\]`,
	`(`, `\(`, `)`, `\)`,
	`*`, `\*`, `_`, `\_`,
	"`", "\\`", `~`, `\~`,
	`#`, `\#`, `|`, `\|`,
)

func escapeMD(s string) string {
	return mdReplacer.Replace(s)
}

func escapeURL(s string) string {
	return url.PathEscape(s)
}
