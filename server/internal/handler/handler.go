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
	"path"
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

// controlKeys are request metadata keys consumed by the handler and never stored.
var controlKeys = map[string]bool{
	"auth":              true,
	"expected-version":  true,
	"if-none-match":     true,
	"if-modified-since": true,
}

// reservedKeys are server-owned response metadata keys that publishers cannot set.
var reservedKeys = map[string]bool{
	"version":         true,
	"modified":        true,
	"etag":            true,
	"content-hash":    true,
	"current-version": true,
	"server-version":  true,
	"your-version":    true,
	"total":           true,
	"current":         true,
	"chain-valid":     true,
	"chain-error":     true,
	"archived":        true,
	"entries":         true,
	"status":          true,
}

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

	// Reject path traversal attempts before any handler logic (including auth)
	// to prevent scope bypass via paths like /allowed/../secret.md.
	if containsDotDot(req.Path) {
		h.logger().Warn("path traversal attempt blocked", "path", sanitize(req.Path))
		h.writeError(stream, protocol.StatusNotFound, req.Path+" not found")
		return
	}

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
	case protocol.VerbAppend:
		h.handleAppend(stream, req)
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

// isHashPath checks if a path is a content-addressed hash: /sha256-<64 hex chars>.
func isHashPath(reqPath string) (string, bool) {
	// /sha256-<64 hex> = 1 + 7 + 64 = 72 characters
	if len(reqPath) != 72 || !strings.HasPrefix(reqPath, "/sha256-") {
		return "", false
	}
	hash := reqPath[1:] // strip leading /
	for _, c := range hash[7:] {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return "", false
		}
	}
	return hash, true
}

func (h *Handler) handleFetchByHash(w io.Writer, req protocol.Request, hash string) {
	docPath, ok := h.Store.LookupHash(hash)
	if !ok {
		h.logger().Info("hash not found", "hash", hash)
		h.writeError(w, protocol.StatusNotFound, "content not found for hash "+hash)
		return
	}

	doc, err := h.Store.Get(docPath, 0)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			h.logger().Info("hash index stale", "hash", hash, "path", sanitize(docPath))
			h.writeError(w, protocol.StatusNotFound, "content not found for hash "+hash)
			return
		}
		h.logger().Error("fetch by hash failed", "hash", hash, "path", sanitize(docPath), "error", err)
		h.writeError(w, protocol.StatusServerError, "internal error")
		return
	}

	h.serveDocument(w, req, doc, docPath)
}

func (h *Handler) handleFetch(w io.Writer, req protocol.Request) {
	// Check for content-addressed hash: FETCH /sha256-<64hex>
	if hash, ok := isHashPath(req.Path); ok {
		h.handleFetchByHash(w, req, hash)
		return
	}

	// Check for path-based version access: FETCH /doc.md/v3
	if basePath, version := parseVersionPath(req.Path); version > 0 {
		h.handleFetchVersion(w, req, basePath, version)
		return
	}

	doc, err := h.Store.Get(req.Path, 0)
	if err != nil {
		if os.IsNotExist(err) {
			// Check if the path is a directory — serve index.md or auto-generate listing.
			isDir, dirErr := h.Store.IsDir(req.Path)
			if dirErr != nil && !os.IsNotExist(dirErr) {
				h.logger().Error("isdir check failed", "path", sanitize(req.Path), "error", dirErr)
				h.writeError(w, protocol.StatusServerError, "internal error")
				return
			}
			if isDir {
				h.handleFetchDirectory(w, req)
				return
			}
			h.logger().Info("not found", "path", sanitize(req.Path))
			h.writeError(w, protocol.StatusNotFound, req.Path+" not found")
			return
		}
		h.logger().Error("fetch failed", "path", sanitize(req.Path), "error", err)
		h.writeError(w, protocol.StatusServerError, "internal error")
		return
	}

	h.serveDocument(w, req, doc, req.Path)
}

// serveDocument handles the common document-serving logic: archived check,
// conditional request handling (etag / if-modified-since), frontmatter
// stripping, and response assembly.
func (h *Handler) serveDocument(w io.Writer, req protocol.Request, doc *store.Document, logPath string) {
	if doc.Archived {
		h.logger().Info("archived", "path", sanitize(logPath))
		h.writeError(w, protocol.StatusArchived, logPath+" is archived")
		return
	}

	etag := computeEtag(doc.Content)

	if ifNoneMatch, ok := req.Metadata["if-none-match"]; ok && ifNoneMatch == etag {
		h.writeNotModified(w)
		return
	}
	if ifModSince, ok := req.Metadata["if-modified-since"]; ok {
		if t, err := time.Parse(time.RFC3339, ifModSince); err == nil {
			if !doc.Modified.After(t) {
				h.writeNotModified(w)
				return
			}
		}
	}

	body := stripFrontmatter(string(doc.Content))
	// Copy publisher metadata first, then set server-owned keys so they can't be overwritten.
	meta := make(map[string]string)
	copyPublisherMeta(meta, doc.Metadata)
	meta["modified"] = doc.Modified.Format(time.RFC3339)
	meta["etag"] = etag
	meta["version"] = strconv.Itoa(doc.Version)
	meta["content-hash"] = computeContentHash(body)
	h.writeResponse(w, protocol.Response{Status: protocol.StatusOK, Metadata: meta, Body: body})
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

func computeContentHash(body string) string {
	hash := sha256.Sum256([]byte(body))
	return "sha256-" + hex.EncodeToString(hash[:])
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

	body, entryCount := buildDirectoryIndex(reqPath, entries)

	resp := protocol.Response{
		Status: protocol.StatusOK,
		Metadata: map[string]string{
			"entries": fmt.Sprintf("%d", entryCount),
		},
		Body: body,
	}
	h.writeResponse(w, resp)
}

// buildDirectoryIndex renders a markdown listing from directory entries.
// Returns the markdown body and the number of entries included.
func buildDirectoryIndex(reqPath string, entries []os.DirEntry) (body string, entryCount int) {
	var sb strings.Builder
	sb.WriteString("\n# Index of " + escapeMD(reqPath) + "\n\n")

	for _, entry := range entries {
		if entryCount >= MaxDirectoryEntries {
			sb.WriteString("\n*...truncated, too many entries*\n")
			break
		}
		entryCount++
		display := escapeMD(entry.Name())
		link := escapeURL(entry.Name())
		if entry.IsDir() {
			sb.WriteString("- [" + display + "/](" + link + "/)\n")
		} else {
			sb.WriteString("- [" + display + "](" + link + ")\n")
		}
	}

	return sb.String(), entryCount
}

func (h *Handler) handleFetchDirectory(w io.Writer, req protocol.Request) {
	// Try index.md first — if the directory has an explicit index, serve it as a normal document.
	indexPath := path.Join(req.Path, "index.md")
	doc, err := h.Store.Get(indexPath, 0)
	if err != nil && !os.IsNotExist(err) {
		h.logger().Error("fetch index failed", "path", sanitize(indexPath), "error", err)
		h.writeError(w, protocol.StatusServerError, "internal error")
		return
	}
	if err == nil {
		h.serveDocument(w, req, doc, req.Path)
		return
	}

	// No index.md — generate a directory listing.
	entries, err := h.Store.ListDir(req.Path)
	if err != nil {
		if os.IsNotExist(err) {
			h.logger().Info("not found", "path", sanitize(req.Path))
			h.writeError(w, protocol.StatusNotFound, req.Path+" not found")
			return
		}
		h.logger().Error("fetch directory failed", "path", sanitize(req.Path), "error", err)
		h.writeError(w, protocol.StatusServerError, "internal error")
		return
	}

	body, entryCount := buildDirectoryIndex(req.Path, entries)
	resp := protocol.Response{
		Status: protocol.StatusOK,
		Metadata: map[string]string{
			"entries": fmt.Sprintf("%d", entryCount),
		},
		Body: body,
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

	body := stripFrontmatter(string(doc.Content))

	// Copy publisher metadata first, then set server-owned keys so they can't be overwritten.
	meta := make(map[string]string)
	copyPublisherMeta(meta, doc.Metadata)
	meta["modified"] = doc.Modified.Format(time.RFC3339)
	meta["version"] = strconv.Itoa(doc.Version)
	meta["content-hash"] = computeContentHash(body)
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
	if _, ok := isHashPath(req.Path); ok {
		h.writeError(w, protocol.StatusBadRequest, "paths matching /sha256-<hash> are reserved")
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
	tokenLabel, err := ts.Authorize(token, req.Path, "publish")
	if err != nil {
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

	h.logger().Info("archive", "audit", true, "operation", "ARCHIVE", "path", sanitize(req.Path), "version", doc.Version, "token_label", sanitize(tokenLabel), "success", true)
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
	if _, ok := isHashPath(req.Path); ok {
		h.writeError(w, protocol.StatusBadRequest, "paths matching /sha256-<hash> are reserved")
		return
	}
	if int64(len(req.Body)) > protocol.MaxBodyLength {
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
	tokenLabel, err := ts.Authorize(token, req.Path, "publish")
	if err != nil {
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
			h.logger().Info("unarchive", "audit", true, "operation", "UNARCHIVE", "path", sanitize(req.Path), "version", doc.Version, "token_label", sanitize(tokenLabel), "success", true)
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

	pubMeta, err := extractPublisherMeta(req.Metadata)
	if err != nil {
		h.writeError(w, protocol.StatusBadRequest, err.Error())
		return
	}

	expectedVersion := -1 // default: no check when expected-version is absent
	if ev := req.Metadata["expected-version"]; ev != "" {
		v, err := strconv.Atoi(ev)
		if err != nil || v < 0 {
			h.writeError(w, protocol.StatusBadRequest, "invalid expected-version")
			return
		}
		expectedVersion = v
	}

	doc, err := h.Store.WriteVersion(req.Path, expectedVersion, []byte(req.Body), pubMeta)
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			h.logger().Info("publish conflict", "audit", true, "operation", "PUBLISH", "path", sanitize(req.Path), "expected_version", expectedVersion, "server_version", doc.Version, "token_label", sanitize(tokenLabel), "success", false)
			var body string
			if expectedVersion == 0 {
				body = fmt.Sprintf("# Version Conflict\n\nA document already exists at this path (version %d).\n\nFetch the current version and publish with the correct expected-version to update it.\n", doc.Version)
			} else {
				body = fmt.Sprintf("# Version Conflict\n\nThe document has been modified since you last fetched it.\n\nYour version: %d\nServer version: %d\n\nPlease fetch the latest version and reapply your edits.\n", expectedVersion, doc.Version)
			}
			resp := protocol.Response{
				Status: protocol.StatusConflict,
				Metadata: map[string]string{
					"your-version":   strconv.Itoa(expectedVersion),
					"server-version": strconv.Itoa(doc.Version),
				},
				Body: body,
			}
			h.writeResponse(w, resp)
			return
		}
		if errors.Is(err, store.ErrNotModified) {
			h.logger().Info("publish unchanged", "audit", true, "operation", "PUBLISH", "path", sanitize(req.Path), "version", doc.Version, "token_label", sanitize(tokenLabel), "success", true)
			resp := protocol.Response{
				Status: protocol.StatusOK,
				Metadata: map[string]string{
					"version":  strconv.Itoa(doc.Version),
					"modified": doc.Modified.Format(time.RFC3339),
				},
			}
			h.writeResponse(w, resp)
			return
		}
		if errors.Is(err, store.ErrArchived) {
			h.logger().Info("publish rejected", "audit", true, "operation", "PUBLISH", "path", sanitize(req.Path), "token_label", sanitize(tokenLabel), "success", false, "reason", "archived")
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

	h.logger().Info("publish", "audit", true, "operation", "PUBLISH", "path", sanitize(req.Path), "version", doc.Version, "token_label", sanitize(tokenLabel), "success", true, "size_bytes", len(req.Body))
	resp := protocol.Response{
		Status: protocol.StatusCreated,
		Metadata: map[string]string{
			"version":  strconv.Itoa(doc.Version),
			"modified": doc.Modified.Format(time.RFC3339),
		},
	}
	h.writeResponse(w, resp)
}

func (h *Handler) handleAppend(w io.Writer, req protocol.Request) {
	if h.Store == nil {
		h.writeError(w, protocol.StatusServerError, "appending not configured")
		return
	}
	if _, ok := isHashPath(req.Path); ok {
		h.writeError(w, protocol.StatusBadRequest, "paths matching /sha256-<hash> are reserved")
		return
	}
	if int64(len(req.Body)) > protocol.MaxBodyLength {
		h.logger().Error("body too large", "path", sanitize(req.Path), "size_bytes", len(req.Body))
		h.writeError(w, protocol.StatusServerError, "content exceeds size limit")
		return
	}
	if req.Body == "" {
		h.writeError(w, protocol.StatusServerError, "append requires a body")
		return
	}

	var ts *auth.TokenStore
	if h.GetTokenStore != nil {
		ts = h.GetTokenStore()
	}
	if ts == nil {
		h.writeError(w, protocol.StatusNotPermitted, "appending requires auth configuration")
		return
	}

	token := req.Metadata["auth"]
	tokenLabel, err := ts.Authorize(token, req.Path, "publish")
	if err != nil {
		switch {
		case errors.Is(err, auth.ErrNoToken), errors.Is(err, auth.ErrInvalidToken), errors.Is(err, auth.ErrTokenExpired):
			h.logger().Warn("unauthorized", "operation", "APPEND", "path", sanitize(req.Path))
			h.writeError(w, protocol.StatusUnauthorized, "authentication required")
		default:
			h.logger().Warn("not permitted", "operation", "APPEND", "path", sanitize(req.Path))
			h.writeError(w, protocol.StatusNotPermitted, "insufficient permissions")
		}
		return
	}

	pubMeta, err := extractPublisherMeta(req.Metadata)
	if err != nil {
		h.writeError(w, protocol.StatusBadRequest, err.Error())
		return
	}

	ev := req.Metadata["expected-version"]
	if ev == "" {
		h.writeError(w, protocol.StatusBadRequest, "APPEND requires expected-version metadata")
		return
	}
	expectedVersion, err := strconv.Atoi(ev)
	if err != nil || expectedVersion < 1 {
		h.writeError(w, protocol.StatusBadRequest, "invalid expected-version for APPEND (must be >= 1)")
		return
	}

	doc, err := h.Store.Append(req.Path, expectedVersion, []byte(req.Body), pubMeta)
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			h.logger().Info("append conflict", "audit", true, "operation", "APPEND", "path", sanitize(req.Path), "expected_version", expectedVersion, "server_version", doc.Version, "token_label", sanitize(tokenLabel), "success", false)
			body := fmt.Sprintf("# Version Conflict\n\nThe document has been modified since you last fetched it.\n\nYour version: %d\nServer version: %d\n\nFetch the latest version and verify whether your append was applied before retrying.\n", expectedVersion, doc.Version)
			resp := protocol.Response{
				Status: protocol.StatusConflict,
				Metadata: map[string]string{
					"your-version":   strconv.Itoa(expectedVersion),
					"server-version": strconv.Itoa(doc.Version),
				},
				Body: body,
			}
			h.writeResponse(w, resp)
			return
		}
		if errors.Is(err, store.ErrArchived) {
			h.logger().Info("append rejected", "audit", true, "operation", "APPEND", "path", sanitize(req.Path), "token_label", sanitize(tokenLabel), "success", false, "reason", "archived")
			h.writeError(w, protocol.StatusArchived, "document is archived; unarchive first")
			return
		}
		if os.IsNotExist(err) {
			h.logger().Info("not found", "path", sanitize(req.Path))
			h.writeError(w, protocol.StatusNotFound, req.Path+" not found")
			return
		}
		if errors.Is(err, store.ErrSizeLimit) {
			h.logger().Info("append rejected", "audit", true, "operation", "APPEND", "path", sanitize(req.Path), "token_label", sanitize(tokenLabel), "success", false, "reason", "size limit exceeded")
			h.writeError(w, protocol.StatusServerError, "content exceeds size limit")
			return
		}
		h.logger().Error("append failed", "path", sanitize(req.Path), "error", err)
		h.writeError(w, protocol.StatusServerError, "internal error")
		return
	}

	h.logger().Info("append", "audit", true, "operation", "APPEND", "path", sanitize(req.Path), "version", doc.Version, "token_label", sanitize(tokenLabel), "success", true, "size_bytes", len(req.Body))
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

// containsDotDot reports whether the path contains a ".." segment.
func containsDotDot(p string) bool {
	for seg := range strings.SplitSeq(p, "/") {
		if seg == ".." {
			return true
		}
	}
	return false
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

func stripFrontmatter(content string) string {
	if !strings.HasPrefix(content, "---\n") {
		return content
	}

	end := strings.Index(content[4:], "\n---\n")
	if end == -1 {
		return content
	}

	return content[4+end+5:]
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

// extractPublisherMeta returns non-control metadata keys from a request.
// Returns nil if no publisher keys are present.
func extractPublisherMeta(reqMeta map[string]string) (map[string]string, error) {
	var meta map[string]string
	size := 0
	for k, v := range reqMeta {
		if controlKeys[k] {
			continue
		}
		if reservedKeys[k] {
			return nil, fmt.Errorf("metadata key %q is reserved", k)
		}
		if !protocol.IsValidMetaKey(k) {
			return nil, fmt.Errorf("metadata key %q contains invalid characters", k)
		}
		if !protocol.IsValidMetaValue(v) {
			return nil, fmt.Errorf("metadata value for key %q contains newlines", k)
		}
		if meta == nil {
			meta = make(map[string]string)
		}
		meta[k] = v
		size += len(k) + len(v)
	}
	if len(meta) > protocol.MaxMetaKeys {
		return nil, fmt.Errorf("too many metadata keys (max %d)", protocol.MaxMetaKeys)
	}
	if size > protocol.MaxMetaBytes {
		return nil, fmt.Errorf("metadata too large (max %d bytes)", protocol.MaxMetaBytes)
	}
	return meta, nil
}

// copyPublisherMeta copies stored metadata into dst, filtering out any
// reserved or control keys. This prevents tampered version files from
// leaking server-owned keys into responses.
func copyPublisherMeta(dst, src map[string]string) {
	for k, v := range src {
		if reservedKeys[k] || controlKeys[k] {
			continue
		}
		if !protocol.IsValidMetaKey(k) || !protocol.IsValidMetaValue(v) {
			continue
		}
		dst[k] = v
	}
}
