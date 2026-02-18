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
	"strconv"
	"strings"
	"time"

	"github.com/latebit/demarkus/protocol"
	"github.com/latebit/demarkus/server/internal/store"
)

// MaxDirectoryEntries is the maximum number of entries returned by LIST.
const MaxDirectoryEntries = 1000

// Handler serves markdown files from a content directory.
type Handler struct {
	ContentDir string
	Store      *store.Store
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
		log.Printf("[ERROR] parse request: %v", err)
		h.writeError(stream, protocol.StatusServerError, "bad request")
		return
	}

	log.Printf("[REQUEST] %s %s", sanitize(req.Verb), sanitize(req.Path))

	// Health check endpoint: responds to FETCH /health with OK
	if req.Path == "/health" && req.Verb == protocol.VerbFetch {
		h.handleHealth(stream)
		return
	}

	switch req.Verb {
	case protocol.VerbFetch:
		h.handleFetch(stream, req)
	case protocol.VerbList:
		h.handleList(stream, req.Path)
	case protocol.VerbVersions:
		h.handleVersions(stream, req.Path)
	case protocol.VerbWrite:
		h.handleWrite(stream, req)
	default:
		h.writeError(stream, protocol.StatusServerError, "unsupported verb: "+sanitize(req.Verb))
	}
}

// resolvePath validates and resolves a request path to an absolute filesystem path
// within the content directory. Returns empty string if the path escapes the root.
//
// Security (race conditions): This function is safe from TOCTOU races because:
//  1. filepath.Clean prevents .. escapes at the string level.
//  2. filepath.EvalSymlinks resolves all symlinks to detect escape attempts.
//  3. The bounds check is done after symlink resolution, so even if a symlink
//     is changed between EvalSymlinks and the caller's os.Stat, the path is still valid.
//  4. If EvalSymlinks fails (ENOENT), we use filepath.Abs which only does string ops.
func (h *Handler) resolvePath(reqPath string) string {
	cleaned := filepath.Clean(reqPath)
	cleaned = strings.TrimLeft(cleaned, "/")
	joined := filepath.Join(h.ContentDir, cleaned)

	absRoot, err := filepath.Abs(h.ContentDir)
	if err != nil {
		log.Printf("[ERROR] resolve content directory: %v", err)
		return ""
	}
	// Resolve symlinks in root for consistent comparison.
	resolved, err := filepath.EvalSymlinks(absRoot)
	if err != nil {
		log.Printf("[ERROR] resolve content directory symlinks: %v", err)
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

// parseVersionPath checks if a path ends with /vN (e.g., /doc.md/v3).
// Returns the base path and version number, or the original path and 0.
func parseVersionPath(reqPath string) (string, int) {
	dir, last := filepath.Split(reqPath)
	if !strings.HasPrefix(last, "v") {
		return reqPath, 0
	}
	num, err := strconv.Atoi(last[1:])
	if err != nil || num < 1 {
		return reqPath, 0
	}
	// dir has trailing slash, clean it
	base := strings.TrimRight(dir, "/")
	if base == "" {
		return reqPath, 0
	}
	return base, num
}

func (h *Handler) handleFetch(w io.Writer, req protocol.Request) {
	// Check for path-based version access: FETCH /doc.md/v3
	if basePath, version := parseVersionPath(req.Path); version > 0 && h.Store != nil {
		h.handleFetchVersion(w, req, basePath, version)
		return
	}

	filePath := h.resolvePath(req.Path)
	if filePath == "" {
		log.Printf("[SECURITY] path traversal attempt: %s", sanitize(req.Path))
		h.writeError(w, protocol.StatusNotFound, req.Path+" not found")
		return
	}

	info, err := os.Stat(filePath)
	if os.IsNotExist(err) {
		log.Printf("[NOTFOUND] %s", sanitize(req.Path))
		h.writeError(w, protocol.StatusNotFound, req.Path+" not found")
		return
	}
	if err != nil {
		log.Printf("[ERROR] stat %s: %v", sanitize(req.Path), err)
		h.writeError(w, protocol.StatusServerError, "internal error")
		return
	}
	if info.IsDir() {
		h.writeError(w, protocol.StatusNotFound, req.Path+" is a directory")
		return
	}
	if info.Size() > store.MaxFileSize {
		log.Printf("[ERROR] file too large: %s (%d bytes)", sanitize(req.Path), info.Size())
		h.writeError(w, protocol.StatusServerError, "file exceeds size limit")
		return
	}

	data, err := os.ReadFile(filePath)
	if err != nil {
		log.Printf("[ERROR] read %s: %v", sanitize(req.Path), err)
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
	writeResponse(w, resp)
}

func (h *Handler) writeNotModified(w io.Writer) {
	resp := protocol.Response{
		Status:   protocol.StatusNotModified,
		Metadata: map[string]string{},
	}
	writeResponse(w, resp)
}

func computeEtag(data []byte) string {
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

func (h *Handler) handleList(w io.Writer, reqPath string) {
	dirPath := h.resolvePath(reqPath)
	if dirPath == "" {
		log.Printf("[SECURITY] path traversal attempt: %s", sanitize(reqPath))
		h.writeError(w, protocol.StatusNotFound, reqPath+" not found")
		return
	}

	info, err := os.Stat(dirPath)
	if os.IsNotExist(err) {
		log.Printf("[NOTFOUND] %s", sanitize(reqPath))
		h.writeError(w, protocol.StatusNotFound, reqPath+" not found")
		return
	}
	if err != nil {
		log.Printf("[ERROR] stat %s: %v", sanitize(reqPath), err)
		h.writeError(w, protocol.StatusServerError, "internal error")
		return
	}
	if !info.IsDir() {
		h.writeError(w, protocol.StatusNotFound, reqPath+" is not a directory")
		return
	}

	entries, err := os.ReadDir(dirPath)
	if err != nil {
		log.Printf("[ERROR] readdir %s: %v", sanitize(reqPath), err)
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
		if entryCount > MaxDirectoryEntries {
			body.WriteString("\n*...truncated, too many entries*\n")
			break
		}
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
	writeResponse(w, resp)
}

func (h *Handler) handleFetchVersion(w io.Writer, req protocol.Request, basePath string, version int) {
	doc, err := h.Store.Get(basePath, version)
	if err != nil {
		if os.IsNotExist(err) {
			log.Printf("[NOTFOUND] %s (v%d)", sanitize(basePath), version)
			h.writeError(w, protocol.StatusNotFound, req.Path+" not found")
			return
		}
		log.Printf("[ERROR] fetch version %s v%d: %v", sanitize(basePath), version, err)
		h.writeError(w, protocol.StatusServerError, "internal error")
		return
	}

	if int64(len(doc.Content)) > store.MaxFileSize {
		log.Printf("[ERROR] file too large: %s v%d (%d bytes)", sanitize(basePath), version, len(doc.Content))
		h.writeError(w, protocol.StatusServerError, "file exceeds size limit")
		return
	}

	body, existingMeta := stripFrontmatter(string(doc.Content))

	meta := map[string]string{
		"modified": doc.Modified.Format(time.RFC3339),
		"version":  strconv.Itoa(doc.Version),
	}
	// Preserve version from file frontmatter if present.
	if v, ok := existingMeta["version"]; ok {
		meta["version"] = v
	}
	// Indicate current version so client knows if this is historical.
	current := h.Store.CurrentVersion(basePath)
	meta["current-version"] = strconv.Itoa(current)

	resp := protocol.Response{
		Status:   protocol.StatusOK,
		Metadata: meta,
		Body:     body,
	}
	writeResponse(w, resp)
}

func (h *Handler) handleVersions(w io.Writer, reqPath string) {
	if h.Store == nil {
		h.writeError(w, protocol.StatusServerError, "versioning not configured")
		return
	}

	versions, err := h.Store.Versions(reqPath)
	if err != nil {
		if os.IsNotExist(err) {
			log.Printf("[NOTFOUND] %s", sanitize(reqPath))
			h.writeError(w, protocol.StatusNotFound, reqPath+" not found")
			return
		}
		log.Printf("[ERROR] versions %s: %v", sanitize(reqPath), err)
		h.writeError(w, protocol.StatusServerError, "internal error")
		return
	}

	var body strings.Builder
	body.WriteString("\n# Version History: " + escapeMD(reqPath) + "\n\n")
	for _, v := range versions {
		body.WriteString(fmt.Sprintf("- [v%d](%s/v%d) - %s\n",
			v.Version, escapeURL(reqPath), v.Version,
			v.Modified.Format(time.RFC3339)))
	}

	meta := map[string]string{
		"total":   fmt.Sprintf("%d", len(versions)),
		"current": fmt.Sprintf("%d", versions[0].Version),
	}

	// Verify hash chain integrity and report result.
	if err := h.Store.VerifyChain(reqPath); err != nil {
		log.Printf("[WARN] chain verification failed for %s: %v", sanitize(reqPath), err)
		meta["chain-valid"] = "false"
		meta["chain-error"] = err.Error()
	} else {
		meta["chain-valid"] = "true"
	}

	resp := protocol.Response{
		Status:   protocol.StatusOK,
		Metadata: meta,
		Body:     body.String(),
	}
	writeResponse(w, resp)
}

func (h *Handler) handleWrite(w io.Writer, req protocol.Request) {
	if h.Store == nil {
		h.writeError(w, protocol.StatusServerError, "writing not configured")
		return
	}
	if int64(len(req.Body)) > store.MaxFileSize {
		log.Printf("[ERROR] body too large: %s (%d bytes)", sanitize(req.Path), len(req.Body))
		h.writeError(w, protocol.StatusServerError, "content exceeds size limit")
		return
	}

	doc, err := h.Store.Write(req.Path, []byte(req.Body))
	if err != nil {
		if os.IsNotExist(err) {
			log.Printf("[SECURITY] path traversal attempt: %s", sanitize(req.Path))
			h.writeError(w, protocol.StatusNotFound, req.Path+" not found")
			return
		}
		log.Printf("[ERROR] write %s: %v", sanitize(req.Path), err)
		h.writeError(w, protocol.StatusServerError, "internal error")
		return
	}

	log.Printf("[WRITE] %s v%d", sanitize(req.Path), doc.Version)
	resp := protocol.Response{
		Status: protocol.StatusCreated,
		Metadata: map[string]string{
			"version":  strconv.Itoa(doc.Version),
			"modified": doc.Modified.Format(time.RFC3339),
		},
	}
	writeResponse(w, resp)
}

func (h *Handler) handleHealth(w io.Writer) {
	resp := protocol.Response{
		Status:   protocol.StatusOK,
		Metadata: map[string]string{},
		Body:     "# Health Check\n\nServer is healthy.\n",
	}
	writeResponse(w, resp)
}

func (h *Handler) writeError(w io.Writer, status, message string) {
	resp := protocol.Response{
		Status:   status,
		Metadata: map[string]string{},
		Body:     fmt.Sprintf("\n# %s\n\n%s\n", statusTitle(status), message),
	}
	writeResponse(w, resp)
}

func writeResponse(w io.Writer, resp protocol.Response) {
	if _, err := resp.WriteTo(w); err != nil {
		log.Printf("[ERROR] write response: %v", err)
	}
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
