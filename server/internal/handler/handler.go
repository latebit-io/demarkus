// Package handler serves Mark Protocol requests from a content directory.
package handler

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/latebit/demarkus/protocol"
	"github.com/latebit/demarkus/server/internal/auth"
	"github.com/latebit/demarkus/server/internal/store"
)

// MaxDirectoryEntries is the maximum number of entries returned by LIST.
const MaxDirectoryEntries = 1000

// Handler serves markdown files from a content directory.
type Handler struct {
	ContentDir    string
	Store         *store.Store
	GetTokenStore func() *auth.TokenStore // nil callback or nil return means writes are denied
	Logger        *slog.Logger
}

func (h *Handler) logger() *slog.Logger {
	if h.Logger != nil {
		return h.Logger
	}
	return slog.Default()
}

// Stream represents a bidirectional stream that can be read, written, and closed.
type Stream interface {
	io.ReadWriteCloser
}

// HandleStream reads a request from the stream and writes a response.
func (h *Handler) HandleStream(stream Stream) {
	defer func() { _ = stream.Close() }()

	req, err := protocol.ParseRequest(stream)
	if err != nil {
		h.logger().Error("parse request failed", "error", err)
		h.writeError(stream, protocol.StatusServerError, "bad request")
		return
	}

	h.logger().Info("request", "verb", sanitize(req.Verb), "path", sanitize(req.Path))

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
	case protocol.VerbPublish:
		h.handlePublish(stream, req)
	case protocol.VerbArchive:
		h.handleArchive(stream, req)
	default:
		h.writeError(stream, protocol.StatusServerError, "unsupported verb: "+sanitize(req.Verb))
	}
}

// parseVersionPath checks if a path ends with /vN (e.g., /doc.md/v3).
// Returns the base path and version number, or the original path and 0.
func parseVersionPath(reqPath string) (basePath string, version int) {
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
	if basePath, version := parseVersionPath(req.Path); version > 0 {
		h.handleFetchVersion(w, req, basePath, version)
		return
	}

	doc, err := h.Store.Get(req.Path, 0)
	if err != nil {
		if os.IsNotExist(err) {
			h.logger().Info("not found", "path", sanitize(req.Path))
			h.writeError(w, protocol.StatusNotFound, req.Path+" not found")
			return
		}
		h.logger().Error("fetch failed", "path", sanitize(req.Path), "error", err)
		h.writeError(w, protocol.StatusServerError, "internal error")
		return
	}

	if doc.Archived {
		h.logger().Info("archived", "path", sanitize(req.Path))
		h.writeError(w, protocol.StatusArchived, req.Path+" is archived")
		return
	}

	etag := computeEtag(doc.Content)
	modified := doc.Modified

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

	body, existingMeta := stripFrontmatter(string(doc.Content))

	meta := map[string]string{
		"modified": modified.Format(time.RFC3339),
		"etag":     etag,
		"version":  strconv.Itoa(doc.Version),
	}
	if v, ok := existingMeta["version"]; ok {
		meta["version"] = v
	}

	resp := protocol.Response{
		Status:   protocol.StatusOK,
		Metadata: meta,
		Body:     body,
	}
	h.writeResponse(w, resp)
}

func (h *Handler) writeNotModified(w io.Writer) {
	resp := protocol.Response{
		Status:   protocol.StatusNotModified,
		Metadata: map[string]string{},
	}
	h.writeResponse(w, resp)
}

func computeEtag(data []byte) string {
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

func (h *Handler) handleList(w io.Writer, reqPath string) {
	entries, err := h.Store.ListDir(reqPath)
	if err != nil {
		if os.IsNotExist(err) {
			h.logger().Info("not found", "path", sanitize(reqPath))
			h.writeError(w, protocol.StatusNotFound, reqPath+" not found")
			return
		}
		h.logger().Error("list failed", "path", sanitize(reqPath), "error", err)
		h.writeError(w, protocol.StatusServerError, "internal error")
		return
	}

	var body strings.Builder
	body.WriteString("\n# Index of " + escapeMD(reqPath) + "\n\n")

	entryCount := 0
	for _, entry := range entries {
		entryCount++
		if entryCount > MaxDirectoryEntries {
			body.WriteString("\n*...truncated, too many entries*\n")
			break
		}
		display := escapeMD(entry.Name())
		link := escapeURL(entry.Name())
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
	h.writeResponse(w, resp)
}

func (h *Handler) handleFetchVersion(w io.Writer, req protocol.Request, basePath string, version int) {
	doc, err := h.Store.Get(basePath, version)
	if err != nil {
		if os.IsNotExist(err) {
			h.logger().Info("not found", "path", sanitize(basePath), "version", version)
			h.writeError(w, protocol.StatusNotFound, req.Path+" not found")
			return
		}
		h.logger().Error("fetch version failed", "path", sanitize(basePath), "version", version, "error", err)
		h.writeError(w, protocol.StatusServerError, "internal error")
		return
	}

	if int64(len(doc.Content)) > store.MaxFileSize {
		h.logger().Error("file too large", "path", sanitize(basePath), "version", version, "size_bytes", len(doc.Content))
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
	h.writeResponse(w, resp)
}

func (h *Handler) handleVersions(w io.Writer, reqPath string) {
	if h.Store == nil {
		h.writeError(w, protocol.StatusServerError, "versioning not configured")
		return
	}

	versions, err := h.Store.Versions(reqPath)
	if err != nil {
		if os.IsNotExist(err) {
			h.logger().Info("not found", "path", sanitize(reqPath))
			h.writeError(w, protocol.StatusNotFound, reqPath+" not found")
			return
		}
		h.logger().Error("versions failed", "path", sanitize(reqPath), "error", err)
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
		h.logger().Warn("chain verification failed", "path", sanitize(reqPath), "error", err)
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
	h.writeResponse(w, resp)
}

func (h *Handler) handleArchive(w io.Writer, req protocol.Request) {
	if h.Store == nil {
		h.writeError(w, protocol.StatusServerError, "archiving not configured")
		return
	}

	var ts *auth.TokenStore
	if h.GetTokenStore != nil {
		ts = h.GetTokenStore()
	}
	if ts == nil {
		h.writeError(w, protocol.StatusNotPermitted, "archiving requires auth configuration")
		return
	}

	token := req.Metadata["auth"]
	if err := ts.Authorize(token, req.Path, "publish"); err != nil {
		switch {
		case errors.Is(err, auth.ErrNoToken), errors.Is(err, auth.ErrInvalidToken), errors.Is(err, auth.ErrTokenExpired):
			h.logger().Warn("unauthorized", "operation", "ARCHIVE", "path", sanitize(req.Path))
			h.writeError(w, protocol.StatusUnauthorized, "authentication required")
		default:
			h.logger().Warn("not permitted", "operation", "ARCHIVE", "path", sanitize(req.Path))
			h.writeError(w, protocol.StatusNotPermitted, "insufficient permissions")
		}
		return
	}

	doc, err := h.Store.Get(req.Path, 0)
	if err != nil {
		if os.IsNotExist(err) {
			h.logger().Info("not found", "path", sanitize(req.Path))
			h.writeError(w, protocol.StatusNotFound, req.Path+" not found")
			return
		}
		h.logger().Error("archive failed", "path", sanitize(req.Path), "error", err)
		h.writeError(w, protocol.StatusServerError, "internal error")
		return
	}

	if err := h.Store.Archive(req.Path, true); err != nil {
		h.logger().Error("archive failed", "path", sanitize(req.Path), "error", err)
		h.writeError(w, protocol.StatusServerError, "internal error")
		return
	}

	h.logger().Info("archive", "audit", true, "operation", "ARCHIVE", "path", sanitize(req.Path), "version", doc.Version, "success", true)
	resp := protocol.Response{
		Status: protocol.StatusOK,
		Metadata: map[string]string{
			"version":  strconv.Itoa(doc.Version),
			"archived": "true",
		},
	}
	h.writeResponse(w, resp)
}

func (h *Handler) handlePublish(w io.Writer, req protocol.Request) {
	if h.Store == nil {
		h.writeError(w, protocol.StatusServerError, "publishing not configured")
		return
	}
	if int64(len(req.Body)) > store.MaxFileSize {
		h.logger().Error("body too large", "path", sanitize(req.Path), "size_bytes", len(req.Body))
		h.writeError(w, protocol.StatusServerError, "content exceeds size limit")
		return
	}

	var ts *auth.TokenStore
	if h.GetTokenStore != nil {
		ts = h.GetTokenStore()
	}
	if ts == nil {
		h.writeError(w, protocol.StatusNotPermitted, "publishing requires auth configuration")
		return
	}

	token := req.Metadata["auth"]
	if err := ts.Authorize(token, req.Path, "publish"); err != nil {
		switch {
		case errors.Is(err, auth.ErrNoToken), errors.Is(err, auth.ErrInvalidToken), errors.Is(err, auth.ErrTokenExpired):
			h.logger().Warn("unauthorized", "operation", "PUBLISH", "path", sanitize(req.Path))
			h.writeError(w, protocol.StatusUnauthorized, "authentication required")
		default:
			h.logger().Warn("not permitted", "operation", "PUBLISH", "path", sanitize(req.Path))
			h.writeError(w, protocol.StatusNotPermitted, "insufficient permissions")
		}
		return
	}

	// Handle empty body case: unarchive if archived, no-op if active
	if req.Body == "" {
		doc, err := h.Store.Get(req.Path, 0)
		if err != nil {
			if os.IsNotExist(err) {
				h.logger().Info("not found", "path", sanitize(req.Path))
				h.writeError(w, protocol.StatusNotFound, req.Path+" not found")
				return
			}
			h.logger().Error("publish failed", "path", sanitize(req.Path), "error", err)
			h.writeError(w, protocol.StatusServerError, "internal error")
			return
		}

		if doc.Archived {
			if err := h.Store.Archive(req.Path, false); err != nil {
				h.logger().Error("unarchive failed", "path", sanitize(req.Path), "error", err)
				h.writeError(w, protocol.StatusServerError, "internal error")
				return
			}
			h.logger().Info("unarchive", "audit", true, "operation", "UNARCHIVE", "path", sanitize(req.Path), "version", doc.Version, "success", true)
		}

		// Return OK (no-op for active documents, or unarchive response)
		resp := protocol.Response{
			Status: protocol.StatusOK,
			Metadata: map[string]string{
				"version": strconv.Itoa(doc.Version),
			},
		}
		h.writeResponse(w, resp)
		return
	}

	doc, err := h.Store.Write(req.Path, []byte(req.Body))
	if err != nil {
		if errors.Is(err, store.ErrArchived) {
			h.logger().Info("publish rejected", "audit", true, "operation", "PUBLISH", "path", sanitize(req.Path), "success", false, "reason", "archived")
			h.writeError(w, protocol.StatusArchived, "document is archived; unarchive first")
			return
		}
		if os.IsNotExist(err) {
			h.logger().Warn("path traversal attempt", "path", sanitize(req.Path))
			h.writeError(w, protocol.StatusNotFound, req.Path+" not found")
			return
		}
		h.logger().Error("publish failed", "path", sanitize(req.Path), "error", err)
		h.writeError(w, protocol.StatusServerError, "internal error")
		return
	}

	h.logger().Info("publish", "audit", true, "operation", "PUBLISH", "path", sanitize(req.Path), "version", doc.Version, "success", true, "size_bytes", len(req.Body))
	resp := protocol.Response{
		Status: protocol.StatusCreated,
		Metadata: map[string]string{
			"version":  strconv.Itoa(doc.Version),
			"modified": doc.Modified.Format(time.RFC3339),
		},
	}
	h.writeResponse(w, resp)
}

func (h *Handler) handleHealth(w io.Writer) {
	resp := protocol.Response{
		Status:   protocol.StatusOK,
		Metadata: map[string]string{},
		Body:     "# Health Check\n\nServer is healthy.\n",
	}
	h.writeResponse(w, resp)
}

func (h *Handler) writeError(w io.Writer, status, message string) {
	resp := protocol.Response{
		Status:   status,
		Metadata: map[string]string{},
		Body:     fmt.Sprintf("\n# %s\n\n%s\n", statusTitle(status), message),
	}
	h.writeResponse(w, resp)
}

func (h *Handler) writeResponse(w io.Writer, resp protocol.Response) {
	if _, err := resp.WriteTo(w); err != nil {
		h.logger().Error("write response failed", "error", err)
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

func stripFrontmatter(content string) (body string, meta map[string]string) {
	meta = make(map[string]string)

	if !strings.HasPrefix(content, "---\n") {
		return content, meta
	}

	end := strings.Index(content[4:], "\n---\n")
	if end == -1 {
		return content, meta
	}

	fmBlock := content[4 : 4+end]
	for line := range strings.SplitSeq(fmBlock, "\n") {
		key, val, ok := strings.Cut(line, ": ")
		if ok {
			meta[strings.TrimSpace(key)] = strings.TrimSpace(val)
		}
	}

	body = content[4+end+5:]
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
