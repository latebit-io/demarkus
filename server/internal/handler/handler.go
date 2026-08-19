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

	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/store"
	"github.com/latebit-io/demarkus/server/internal/auth"
	"github.com/latebit-io/demarkus/server/internal/catalog"
)

// MaxDirectoryEntries is the maximum number of entries returned by LIST.
const MaxDirectoryEntries = 1000

// LOOKUP result bounds.
const (
	// defaultLookupLimit is the result count used when a request omits limit.
	defaultLookupLimit = 10
	// maxLookupResults is the hard cap on LOOKUP results regardless of limit.
	maxLookupResults = 1000
)

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
	"matches":         true,
	"status":          true,
}

// DocumentStore is the handler's view of a content store. The file-backed
// implementation is protocol/store.Store; alternative backends implement the
// same contract, including the per-method error contracts below.
type DocumentStore interface {
	// A missing document or directory returns an error satisfying
	// errors.Is(err, os.ErrNotExist). Applies to every addressing method.
	Get(reqPath string, version int) (*store.Document, error)
	ListEntries(reqPath string, includeArchived bool) ([]store.DirEntry, error)
	IsDir(reqPath string) (bool, error)
	Versions(reqPath string) ([]store.VersionInfo, error)
	CurrentVersion(reqPath string) int
	// LookupHash resolves a live document by body hash; when several share
	// the body, the smallest path wins.
	LookupHash(hash string) (string, bool)
	VerifyChain(reqPath string) error
	// Writes surface the sentinels the handlers map to protocol statuses:
	// ErrConflict, ErrArchived, ErrNotModified, ErrSizeLimit, ErrInvalidMeta.
	// The file store's ErrVersionExists is folded into ErrConflict here.
	WriteVersion(reqPath string, expectedVersion int, content []byte, meta map[string]string) (*store.Document, error)
	Append(reqPath string, expectedVersion int, content []byte, meta map[string]string) (*store.Document, error)
	Archive(reqPath string, archived bool) error
}

// LookupCatalog is the LOOKUP-index seam beside DocumentStore. Backends that
// maintain the catalog in the write transaction (pgstore) no-op Put/Remove;
// the storetest LOOKUP conformance suite is the contract for Lookup parity.
type LookupCatalog interface {
	Lookup(query string, opts catalog.Options) ([]catalog.Result, error)
	Put(docPath string, meta map[string]string, body []byte, modified time.Time)
	Remove(docPath string)
}

// Handler serves the Mark protocol over a DocumentStore.
type Handler struct {
	Store         DocumentStore
	Catalog       LookupCatalog           // LOOKUP index; nil disables LOOKUP and catalog updates
	GetTokenStore func() *auth.TokenStore // nil callback or nil return means writes are denied
	Logger        *slog.Logger
	ReadOnly      bool // reject all write operations
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
	if store.ContainsDotDot(req.Path) {
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
		h.handleList(stream, req)
	case protocol.VerbVersions:
		h.handleVersions(stream, req)
	case protocol.VerbLookup:
		h.handleLookup(stream, req)
	case protocol.VerbPublish, protocol.VerbArchive, protocol.VerbAppend:
		if h.ReadOnly {
			h.writeError(stream, protocol.StatusNotPermitted, "server is read-only")
			return
		}
		switch req.Verb {
		case protocol.VerbPublish:
			h.handlePublish(stream, req)
		case protocol.VerbArchive:
			h.handleArchive(stream, req)
		case protocol.VerbAppend:
			h.handleAppend(stream, req)
		}
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

func (h *Handler) handleFetchByHash(w io.Writer, req protocol.Request, hash string) {
	docPath, ok := h.Store.LookupHash(hash)
	if !ok {
		h.logger().Info("hash not found", "hash", hash)
		h.writeError(w, protocol.StatusNotFound, "content not found for hash "+hash)
		return
	}

	// Check read auth on the resolved path — knowing a hash must not bypass access control.
	pathReq := req
	pathReq.Path = docPath
	if !h.authorizeRead(w, pathReq) {
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

// authorizeRead checks whether a read request is allowed. Returns true if the
// request may proceed. If the path is not covered by any read token, access is
// public and the request proceeds without auth. Returns false and writes an
// error response if auth is required but missing or invalid.
func (h *Handler) authorizeRead(w io.Writer, req protocol.Request) bool {
	ok, err := h.checkReadAuth(req.Path, req.Metadata["auth"])
	if ok {
		return true
	}
	switch {
	case errors.Is(err, auth.ErrNoToken), errors.Is(err, auth.ErrInvalidToken), errors.Is(err, auth.ErrTokenExpired):
		h.logger().Warn("unauthorized", "operation", req.Verb, "path", sanitize(req.Path))
		h.writeError(w, protocol.StatusUnauthorized, "authentication required")
	default:
		h.logger().Warn("not permitted", "operation", req.Verb, "path", sanitize(req.Path))
		h.writeError(w, protocol.StatusNotPermitted, "insufficient permissions")
	}
	return false
}

// checkReadAuth reports whether token may read reqPath, without writing a
// response. It returns (true, nil) when the path needs no read auth or the
// token is authorized. When unauthorized it returns (false, err) so callers
// that surface errors can distinguish unauthorized from not-permitted; callers
// that only filter (e.g. LOOKUP) can ignore err.
func (h *Handler) checkReadAuth(reqPath, token string) (bool, error) {
	var ts *auth.TokenStore
	if h.GetTokenStore != nil {
		ts = h.GetTokenStore()
	}
	if ts == nil {
		return true, nil
	}
	if reqPath == protocol.WellKnownManifestPath {
		return true, nil
	}
	if !ts.RequiresReadAuth(reqPath) {
		return true, nil
	}
	if _, err := ts.Authorize(token, reqPath, "read"); err != nil {
		return false, err
	}
	return true, nil
}

func (h *Handler) handleFetch(w io.Writer, req protocol.Request) {
	// Check for content-addressed hash: FETCH /sha256-<64hex>
	// Read auth for hash paths is checked after resolving to a real path.
	if hash, ok := protocol.IsHashPath(req.Path); ok {
		h.handleFetchByHash(w, req, hash)
		return
	}

	// For versioned paths, check read auth on the base path so that
	// /doc.md/v2 is gated by the same token as /doc.md.
	if basePath, version := parseVersionPath(req.Path); version > 0 {
		authReq := req
		authReq.Path = basePath
		if !h.authorizeRead(w, authReq) {
			return
		}
		h.handleFetchVersion(w, req, basePath, version)
		return
	}

	if !h.authorizeRead(w, req) {
		return
	}

	doc, err := h.Store.Get(req.Path, 0)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// Check if the path is a directory — serve index.md or auto-generate listing.
			isDir, dirErr := h.Store.IsDir(req.Path)
			if dirErr != nil && !errors.Is(dirErr, os.ErrNotExist) {
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
// conditional request handling (etag / if-modified-since), and response
// assembly. doc.Content is body-only per the store contract.
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

	body := string(doc.Content)
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

func (h *Handler) handleList(w io.Writer, req protocol.Request) {
	if !h.authorizeRead(w, req) {
		return
	}
	reqPath := req.Path
	if _, ok := protocol.IsHashPath(reqPath); ok {
		h.writeError(w, protocol.StatusNotFound, reqPath+" not found")
		return
	}
	includeArchived := req.Metadata["include-archived"] == "true"
	entries, err := h.Store.ListEntries(reqPath, includeArchived)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
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
func buildDirectoryIndex(reqPath string, entries []store.DirEntry) (body string, entryCount int) {
	var sb strings.Builder
	sb.WriteString("\n# Index of " + escapeMD(reqPath) + "\n\n")

	for _, entry := range entries {
		if entryCount >= MaxDirectoryEntries {
			sb.WriteString("\n*...truncated, too many entries*\n")
			break
		}
		entryCount++
		display := escapeMD(entry.Name)
		link := escapeURL(entry.Name)
		if entry.IsDir {
			sb.WriteString("- [" + display + "/](" + link + "/)\n")
		} else {
			sb.WriteString("- [" + display + "](" + link + ")\n")
		}
	}

	return sb.String(), entryCount
}

// buildLookupResults renders LOOKUP matches as a markdown table. Cells are
// markdown-escaped so paths, titles, and tags cannot break the table or inject
// markup. The Path column is server-relative; clients compose the full URL.
func buildLookupResults(query, scope string, rows []catalog.Result) string {
	var sb strings.Builder
	sb.WriteString("\n# Lookup matches for \"" + escapeMD(query) + "\" in " + escapeMD(scope) + "\n\n")
	sb.WriteString("| Path | Importance | Title | Tags |\n")
	sb.WriteString("|------|------------|-------|------|\n")
	for _, r := range rows {
		sb.WriteString("| " + escapeMD(r.Path) +
			" | " + strconv.FormatFloat(r.Importance, 'f', 2, 64) +
			" | " + escapeMD(r.Title) +
			" | " + escapeMD(strings.Join(r.Tags, ", ")) + " |\n")
	}
	return sb.String()
}

func (h *Handler) handleFetchDirectory(w io.Writer, req protocol.Request) {
	includeArchived := req.Metadata["include-archived"] == "true"

	// Try index.md first — if the directory has an explicit index, serve it as
	// a normal document. An ARCHIVED index.md is treated like a missing one:
	// serveDocument would tombstone it, blocking the whole directory view while
	// LIST still shows the directory's live entries. Fetching the archived
	// index.md itself (by its own path) still returns the tombstone.
	indexPath := path.Join(req.Path, "index.md")
	doc, err := h.Store.Get(indexPath, 0)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		h.logger().Error("fetch index failed", "path", sanitize(indexPath), "error", err)
		h.writeError(w, protocol.StatusServerError, "internal error")
		return
	}
	if err == nil && !doc.Archived {
		h.serveDocument(w, req, doc, req.Path)
		return
	}

	// No (visible) index.md — generate a directory listing.
	entries, err := h.Store.ListEntries(req.Path, includeArchived)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
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
		if errors.Is(err, os.ErrNotExist) {
			h.logger().Info("not found", "path", sanitize(basePath), "version", version)
			h.writeError(w, protocol.StatusNotFound, req.Path+" not found")
			return
		}
		h.logger().Error("fetch version failed", "path", sanitize(basePath), "version", version, "error", err)
		h.writeError(w, protocol.StatusServerError, "internal error")
		return
	}

	body := string(doc.Content)

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

func (h *Handler) handleVersions(w io.Writer, req protocol.Request) {
	if !h.authorizeRead(w, req) {
		return
	}
	reqPath := req.Path
	if h.Store == nil {
		h.writeError(w, protocol.StatusServerError, "versioning not configured")
		return
	}
	if _, ok := protocol.IsHashPath(reqPath); ok {
		h.writeError(w, protocol.StatusNotFound, reqPath+" not found")
		return
	}

	versions, err := h.Store.Versions(reqPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
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
		meta["chain-error"] = "chain integrity check failed"
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

func (h *Handler) handleLookup(w io.Writer, req protocol.Request) {
	if h.Catalog == nil {
		h.writeError(w, protocol.StatusServerError, "lookup not configured")
		return
	}

	query := strings.TrimSpace(req.Metadata["query"])
	// "*" is the match-all query (whole catalog under scope, importance
	// order); anything else needs at least 2 characters of subject.
	if query != "*" && len([]rune(query)) < 2 {
		h.writeError(w, protocol.StatusBadRequest, "query must be at least 2 characters")
		return
	}

	limit := defaultLookupLimit
	if lv := req.Metadata["limit"]; lv != "" {
		n, err := strconv.Atoi(lv)
		if err != nil || n < 1 {
			h.writeError(w, protocol.StatusBadRequest, "invalid limit")
			return
		}
		limit = n
	}
	if limit > maxLookupResults {
		limit = maxLookupResults
	}

	preds, err := catalog.ParseFilter(req.Metadata["filter"])
	if err != nil {
		h.writeError(w, protocol.StatusBadRequest, err.Error())
		return
	}

	// Scope must be the whole-server root or an existing directory.
	if req.Path != "/" {
		isDir, derr := h.Store.IsDir(req.Path)
		if derr != nil && !errors.Is(derr, os.ErrNotExist) {
			h.logger().Error("lookup isdir check failed", "path", sanitize(req.Path), "error", derr)
			h.writeError(w, protocol.StatusServerError, "internal error")
			return
		}
		if !isDir {
			h.logger().Info("lookup scope not found", "path", sanitize(req.Path))
			h.writeError(w, protocol.StatusNotFound, req.Path+" not found")
			return
		}
	}

	results, err := h.Catalog.Lookup(query, catalog.Options{Scope: req.Path, Filter: preds, Max: maxLookupResults})
	if err != nil {
		h.logger().Error("lookup failed", "path", sanitize(req.Path), "error", err)
		h.writeError(w, protocol.StatusServerError, "internal error")
		return
	}

	// Filter by read authorization before truncating to limit, so protected
	// documents the requester cannot see never displace authorized matches and
	// never reveal their existence (no path, no title, not counted).
	token := req.Metadata["auth"]
	rows := make([]catalog.Result, 0, len(results))
	for _, r := range results {
		if ok, _ := h.checkReadAuth(r.Path, token); !ok {
			continue
		}
		rows = append(rows, r)
		if len(rows) >= limit {
			break
		}
	}

	resp := protocol.Response{
		Status:   protocol.StatusOK,
		Metadata: map[string]string{"matches": strconv.Itoa(len(rows))},
		Body:     buildLookupResults(query, req.Path, rows),
	}
	h.writeResponse(w, resp)
}

// catalogPut adds or replaces the LOOKUP catalog entry for a document. body is
// the markdown content with store frontmatter already stripped. No-op when no
// catalog is configured.
func (h *Handler) catalogPut(reqPath string, body []byte, meta map[string]string, modified time.Time) {
	if h.Catalog == nil {
		return
	}
	h.Catalog.Put(reqPath, meta, body, modified)
}

// catalogRemove drops a document from the LOOKUP catalog. No-op when no catalog
// is configured.
func (h *Handler) catalogRemove(reqPath string) {
	if h.Catalog == nil {
		return
	}
	h.Catalog.Remove(reqPath)
}

func (h *Handler) handleArchive(w io.Writer, req protocol.Request) {
	if h.Store == nil {
		h.writeError(w, protocol.StatusServerError, "archiving not configured")
		return
	}
	if _, ok := protocol.IsHashPath(req.Path); ok {
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
		if errors.Is(err, os.ErrNotExist) {
			h.logger().Info("not found", "path", sanitize(req.Path))
			h.writeError(w, protocol.StatusNotFound, req.Path+" not found")
			return
		}
		h.logger().Error("archive failed", "path", sanitize(req.Path), "error", err)
		h.writeError(w, protocol.StatusServerError, "internal error")
		return
	}

	if err := h.Store.Archive(req.Path, true); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// Deleted between the Get pre-check and the archive.
			h.logger().Info("archive failed", "audit", true, "operation", "ARCHIVE", "path", sanitize(req.Path), "token_label", sanitize(tokenLabel), "success", false, "reason", "not found")
			h.writeError(w, protocol.StatusNotFound, req.Path+" not found")
			return
		}
		h.logger().Error("archive failed", "audit", true, "operation", "ARCHIVE", "path", sanitize(req.Path), "token_label", sanitize(tokenLabel), "success", false, "error", err)
		h.writeError(w, protocol.StatusServerError, "internal error")
		return
	}

	h.logger().Info("archive", "audit", true, "operation", "ARCHIVE", "path", sanitize(req.Path), "version", doc.Version, "token_label", sanitize(tokenLabel), "success", true)
	h.catalogRemove(req.Path)
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
	if _, ok := protocol.IsHashPath(req.Path); ok {
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

	// Contract before the empty-body shortcut: a non-.md path is rejected
	// even with an empty body.
	if err := store.ValidateDocumentContent(req.Path, []byte(req.Body)); err != nil {
		h.logger().Info("publish rejected", "audit", true, "operation", "PUBLISH", "path", sanitize(req.Path), "token_label", sanitize(tokenLabel), "success", false, "reason", "invalid document")
		h.writeError(w, protocol.StatusBadRequest, err.Error())
		return
	}

	// Handle empty body case: unarchive if archived, no-op if active
	if req.Body == "" {
		doc, err := h.Store.Get(req.Path, 0)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
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
				if errors.Is(err, os.ErrNotExist) {
					// Deleted between the Get pre-check and the unarchive.
					h.logger().Info("unarchive failed", "audit", true, "operation", "UNARCHIVE", "path", sanitize(req.Path), "token_label", sanitize(tokenLabel), "success", false, "reason", "not found")
					h.writeError(w, protocol.StatusNotFound, req.Path+" not found")
					return
				}
				h.logger().Error("unarchive failed", "audit", true, "operation", "UNARCHIVE", "path", sanitize(req.Path), "token_label", sanitize(tokenLabel), "success", false, "error", err)
				h.writeError(w, protocol.StatusServerError, "internal error")
				return
			}
			h.logger().Info("unarchive", "audit", true, "operation", "UNARCHIVE", "path", sanitize(req.Path), "version", doc.Version, "token_label", sanitize(tokenLabel), "success", true)
			h.catalogPut(req.Path, doc.Content, doc.Metadata, doc.Modified)
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
	pubMeta = store.ApplyOKFTypeDefault(req.Path, pubMeta)
	if err := store.ValidateMeta(pubMeta); err != nil {
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
	h.logPrune("PUBLISH", req.Path, tokenLabel, doc)
	if err != nil {
		h.writePublishError(w, req, expectedVersion, doc, err, tokenLabel)
		return
	}

	h.logger().Info("publish", "audit", true, "operation", "PUBLISH", "path", sanitize(req.Path), "version", doc.Version, "token_label", sanitize(tokenLabel), "success", true, "size_bytes", len(req.Body))
	h.catalogPut(req.Path, doc.Content, doc.Metadata, doc.Modified)
	resp := protocol.Response{
		Status: protocol.StatusCreated,
		Metadata: map[string]string{
			"version":  strconv.Itoa(doc.Version),
			"modified": doc.Modified.Format(time.RFC3339),
		},
	}
	h.writeResponse(w, resp)
}

// writePublishError maps a WriteVersion failure onto the protocol response
// for PUBLISH, covering every sentinel in the DocumentStore error contract.
func (h *Handler) writePublishError(w io.Writer, req protocol.Request, expectedVersion int, doc *store.Document, err error, tokenLabel string) {
	switch {
	case errors.Is(err, store.ErrConflict):
		h.logger().Info("publish conflict", "audit", true, "operation", "PUBLISH", "path", sanitize(req.Path), "expected_version", expectedVersion, "server_version", doc.Version, "token_label", sanitize(tokenLabel), "success", false)
		var body string
		if expectedVersion == 0 {
			body = fmt.Sprintf("# Version Conflict\n\nA document already exists at this path (version %d).\n\nFetch the current version and publish with the correct expected-version to update it.\n", doc.Version)
		} else {
			body = fmt.Sprintf("# Version Conflict\n\nThe document has been modified since you last fetched it.\n\nYour version: %d\nServer version: %d\n\nPlease fetch the latest version and reapply your edits.\n", expectedVersion, doc.Version)
		}
		h.writeResponse(w, protocol.Response{
			Status: protocol.StatusConflict,
			Metadata: map[string]string{
				"your-version":   strconv.Itoa(expectedVersion),
				"server-version": strconv.Itoa(doc.Version),
			},
			Body: body,
		})
	case errors.Is(err, store.ErrNotModified):
		h.logger().Info("publish unchanged", "audit", true, "operation", "PUBLISH", "path", sanitize(req.Path), "version", doc.Version, "token_label", sanitize(tokenLabel), "success", true)
		h.writeResponse(w, protocol.Response{
			Status: protocol.StatusOK,
			Metadata: map[string]string{
				"version":  strconv.Itoa(doc.Version),
				"modified": doc.Modified.Format(time.RFC3339),
			},
		})
	case errors.Is(err, store.ErrArchived):
		h.logger().Info("publish rejected", "audit", true, "operation", "PUBLISH", "path", sanitize(req.Path), "token_label", sanitize(tokenLabel), "success", false, "reason", "archived")
		h.writeError(w, protocol.StatusArchived, "document is archived; unarchive first")
	case errors.Is(err, os.ErrNotExist):
		h.logger().Info("publish failed", "audit", true, "operation", "PUBLISH", "path", sanitize(req.Path), "token_label", sanitize(tokenLabel), "success", false, "reason", "not found")
		h.writeError(w, protocol.StatusNotFound, req.Path+" not found")
	case errors.Is(err, store.ErrSizeLimit):
		h.logger().Info("publish rejected", "audit", true, "operation", "PUBLISH", "path", sanitize(req.Path), "token_label", sanitize(tokenLabel), "success", false, "reason", "size limit exceeded")
		h.writeError(w, protocol.StatusServerError, "content exceeds size limit")
	case errors.Is(err, store.ErrInvalidMeta):
		h.logger().Info("publish rejected", "audit", true, "operation", "PUBLISH", "path", sanitize(req.Path), "token_label", sanitize(tokenLabel), "success", false, "reason", "invalid metadata")
		h.writeError(w, protocol.StatusBadRequest, err.Error())
	default:
		h.logger().Error("publish failed", "audit", true, "operation", "PUBLISH", "path", sanitize(req.Path), "token_label", sanitize(tokenLabel), "success", false, "error", err)
		h.writeError(w, protocol.StatusServerError, "internal error")
	}
}

func (h *Handler) handleAppend(w io.Writer, req protocol.Request) {
	if h.Store == nil {
		h.writeError(w, protocol.StatusServerError, "appending not configured")
		return
	}
	if _, ok := protocol.IsHashPath(req.Path); ok {
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

	// Contract before the empty-body check: a non-.md path is rejected as
	// bad-request rather than reported as a missing body.
	if err := store.ValidateDocumentContent(req.Path, []byte(req.Body)); err != nil {
		h.logger().Info("append rejected", "audit", true, "operation", "APPEND", "path", sanitize(req.Path), "token_label", sanitize(tokenLabel), "success", false, "reason", "invalid document")
		h.writeError(w, protocol.StatusBadRequest, err.Error())
		return
	}
	if req.Body == "" {
		h.writeError(w, protocol.StatusServerError, "append requires a body")
		return
	}

	pubMeta, err := extractPublisherMeta(req.Metadata)
	if err != nil {
		h.writeError(w, protocol.StatusBadRequest, err.Error())
		return
	}
	// No OKF type default here: the store applies it after merging the base
	// version's metadata, so an append never overwrites a declared type.
	if err := store.ValidateMeta(pubMeta); err != nil {
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
	h.logPrune("APPEND", req.Path, tokenLabel, doc)
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
		if errors.Is(err, os.ErrNotExist) {
			h.logger().Info("not found", "path", sanitize(req.Path))
			h.writeError(w, protocol.StatusNotFound, req.Path+" not found")
			return
		}
		if errors.Is(err, store.ErrSizeLimit) {
			h.logger().Info("append rejected", "audit", true, "operation", "APPEND", "path", sanitize(req.Path), "token_label", sanitize(tokenLabel), "success", false, "reason", "size limit exceeded")
			h.writeError(w, protocol.StatusServerError, "content exceeds size limit")
			return
		}
		if errors.Is(err, store.ErrInvalidMeta) {
			h.logger().Info("append rejected", "audit", true, "operation", "APPEND", "path", sanitize(req.Path), "token_label", sanitize(tokenLabel), "success", false, "reason", "merged metadata invalid")
			h.writeError(w, protocol.StatusBadRequest, err.Error())
			return
		}
		h.logger().Error("append failed", "path", sanitize(req.Path), "error", err)
		h.writeError(w, protocol.StatusServerError, "internal error")
		return
	}

	h.logger().Info("append", "audit", true, "operation", "APPEND", "path", sanitize(req.Path), "version", doc.Version, "token_label", sanitize(tokenLabel), "success", true, "size_bytes", len(req.Body))
	h.catalogPut(req.Path, doc.Content, doc.Metadata, doc.Modified)
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

// logPrune audit-logs retention pruning performed by a write. Version
// deletions must always be attributable, so this runs on every write outcome
// that carries a prune result — including conflict paths, where the write
// (and its prune) happened despite the conflict response.
func (h *Handler) logPrune(operation, reqPath, tokenLabel string, doc *store.Document) {
	if doc == nil || doc.Prune == nil {
		return
	}
	p := doc.Prune
	if p.Err != nil {
		h.logger().Error("prune incomplete", "audit", true, "operation", operation, "path", sanitize(reqPath), "pruned_from", p.From, "pruned_to", p.To, "token_label", sanitize(tokenLabel), "success", false, "error", p.Err)
		return
	}
	h.logger().Info("prune", "audit", true, "operation", operation, "path", sanitize(reqPath), "pruned_from", p.From, "pruned_to", p.To, "token_label", sanitize(tokenLabel), "success", true)
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
		size += store.SerializedMetaSize(k, v)
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
