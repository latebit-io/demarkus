// Package handler serves Mark Protocol requests from a content directory.
package handler

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/render"
	"github.com/latebit-io/demarkus/protocol/storefmt"
	"github.com/latebit-io/demarkus/server/internal/auth"
	storagebackend "github.com/latebit-io/demarkus/server/internal/backend"
	"github.com/latebit-io/demarkus/server/internal/catalog"
)

// MaxDirectoryEntries is the maximum number of entries returned by LIST.
const MaxDirectoryEntries = protocol.MaxListPageSize

// LOOKUP result bounds.
const (
	// defaultLookupLimit is the result count used when a request omits limit.
	defaultLookupLimit = 10
	// maxLookupResults is the hard cap on LOOKUP results regardless of limit.
	maxLookupResults = 1000
	// maxLookupTerms bounds distinct query terms: each one costs a scored
	// pass per document, and 64KB of frontmatter would otherwise buy tens
	// of thousands.
	maxLookupTerms = 32
)

// controlKeys are request metadata keys consumed by the handler and never stored.
var controlKeys = map[string]bool{
	"auth":              true,
	"expected-version":  true,
	"if-none-match":     true,
	"if-modified-since": true,
}

// DocumentStore is the handler's view of a content store; storetest holds
// every backend to the same contract.
type DocumentStore = storagebackend.Store

// Config is what a Handler is built from. Store and Logger are required.
type Config struct {
	Store         DocumentStore
	GetTokenStore func() *auth.TokenStore // nil callback or nil return means writes are denied
	Logger        *slog.Logger
	ReadOnly      bool // reject all write operations
}

// Handler serves the Mark protocol over a DocumentStore. Build it with New.
type Handler struct {
	store         DocumentStore
	getTokenStore func() *auth.TokenStore
	logger        *slog.Logger
	readOnly      bool
}

// New refuses a config without a store or a logger, so neither can be missing
// when a request arrives.
func New(config Config) (*Handler, error) {
	if config.Store == nil {
		return nil, errors.New("handler: store is nil")
	}
	if config.Logger == nil {
		return nil, errors.New("handler: logger is nil")
	}
	return &Handler{
		store:         config.Store,
		getTokenStore: config.GetTokenStore,
		logger:        config.Logger,
		readOnly:      config.ReadOnly,
	}, nil
}

// WithLogger returns a copy that logs to logger, for per connection fields.
func (h *Handler) WithLogger(logger *slog.Logger) *Handler {
	scoped := *h
	scoped.logger = logger
	return &scoped
}

// tokenStore is the store pinned for this request, or nil without auth.
func (h *Handler) tokenStore() *auth.TokenStore {
	if h.getTokenStore == nil {
		return nil
	}
	return h.getTokenStore()
}

// Stream represents a bidirectional stream that can be read, written, and closed.
type Stream interface {
	io.ReadWriteCloser
}

// HandleStream reads a request from the stream and writes a response. ctx
// bounds every store call the request makes.
func (h *Handler) HandleStream(ctx context.Context, stream Stream) {
	// The response is already written or failed; a close error changes nothing.
	defer func() { _ = stream.Close() }()

	req, err := protocol.ParseRequest(stream)
	if err != nil {
		h.writeParseError(stream, err)
		return
	}

	// Reject path traversal attempts before any handler logic (including auth)
	// to prevent scope bypass via paths like /allowed/../secret.md.
	if storefmt.ContainsDotDot(req.Path) {
		h.logger.Warn("path traversal attempt blocked", "path", sanitize(req.Path))
		h.writeError(stream, protocol.StatusNotFound, req.Path+" not found")
		return
	}
	req.Path = storefmt.CanonicalPath(req.Path)

	// Token reloads take effect between requests, never midway through one.
	pinned := *h
	if h.getTokenStore != nil {
		tokenStore := h.getTokenStore()
		pinned.getTokenStore = func() *auth.TokenStore { return tokenStore }
	}
	h = &pinned

	h.logger.Info("request", "verb", sanitize(req.Verb), "path", sanitize(req.Path))

	// Health check endpoint: responds to FETCH /health with OK
	if req.Path == "/health" && req.Verb == protocol.VerbFetch {
		h.handleHealth(stream)
		return
	}

	switch {
	case isReadVerb(req.Verb):
		h.serveRead(ctx, stream, req)
	case isWriteVerb(req.Verb):
		h.serveWrite(ctx, stream, req)
	default:
		h.writeError(stream, protocol.StatusBadRequest, "unsupported verb: "+sanitize(req.Verb))
	}
}

// readCall is one read request pinned to one snapshot.
type readCall struct {
	w    io.Writer
	req  protocol.Request
	view storagebackend.ReadView
}

// serveRead answers a read verb from one snapshot, so a response never mixes
// two committed states.
func (h *Handler) serveRead(ctx context.Context, w io.Writer, req protocol.Request) {
	view, err := h.store.OpenReadView(ctx)
	if err != nil {
		h.logger.Error("open read view failed", "verb", req.Verb, "path", sanitize(req.Path), "error", err)
		h.writeError(w, protocol.StatusServerError, "internal error")
		return
	}
	defer func() {
		if err := view.Close(); err != nil {
			h.logger.Error("close read view failed", "verb", req.Verb, "path", sanitize(req.Path), "error", err)
		}
	}()
	call := &readCall{w: w, req: req, view: view}
	switch req.Verb {
	case protocol.VerbFetch:
		h.handleFetch(ctx, call)
	case protocol.VerbList:
		h.handleList(ctx, call)
	case protocol.VerbVersions:
		h.handleVersions(ctx, call)
	case protocol.VerbLookup:
		h.handleLookup(ctx, call)
	}
}

// writeCall is one authorized write request.
type writeCall struct {
	w          io.Writer
	req        protocol.Request
	tokenLabel string
}

// writeActivity names each write verb in configuration errors.
var writeActivity = map[string]string{
	protocol.VerbPublish: "publishing",
	protocol.VerbArchive: "archiving",
	protocol.VerbAppend:  "appending",
}

// serveWrite runs the checks every write shares, then the verb.
func (h *Handler) serveWrite(ctx context.Context, w io.Writer, req protocol.Request) {
	if h.readOnly {
		h.writeError(w, protocol.StatusNotPermitted, "server is read-only")
		return
	}
	if _, ok := protocol.IsHashPath(req.Path); ok {
		h.writeError(w, protocol.StatusBadRequest, "paths matching /sha256-<hash> are reserved")
		return
	}
	if int64(len(req.Body)) > protocol.MaxBodyLength {
		h.logger.Error("body too large", "path", sanitize(req.Path), "size_bytes", len(req.Body))
		h.writeError(w, protocol.StatusServerError, "content exceeds size limit")
		return
	}
	tokenLabel, ok := h.authorizeWrite(w, req)
	if !ok {
		return
	}
	call := &writeCall{w: w, req: req, tokenLabel: tokenLabel}
	switch req.Verb {
	case protocol.VerbPublish:
		h.handlePublish(ctx, call)
	case protocol.VerbArchive:
		h.handleArchive(ctx, call)
	case protocol.VerbAppend:
		h.handleAppend(ctx, call)
	}
}

// authorizeWrite checks the publish capability every write verb needs and
// answers the denial itself. Without a token store no write is allowed.
func (h *Handler) authorizeWrite(w io.Writer, req protocol.Request) (tokenLabel string, ok bool) {
	ts := h.tokenStore()
	if ts == nil {
		h.writeError(w, protocol.StatusNotPermitted, writeActivity[req.Verb]+" requires auth configuration")
		return "", false
	}
	tokenLabel, err := ts.Authorize(req.Metadata["auth"], req.Path, "publish")
	if err != nil {
		h.writeAuthDenied(w, req, err)
		return "", false
	}
	return tokenLabel, true
}

func isWriteVerb(verb string) bool {
	return verb == protocol.VerbPublish || verb == protocol.VerbArchive || verb == protocol.VerbAppend
}

// writeParseError follows SPEC 7: a grammar error is the client's; a read
// failure or a size limit is not.
func (h *Handler) writeParseError(w io.Writer, err error) {
	if errors.Is(err, protocol.ErrMalformedRequest) {
		h.logger.Warn("malformed request", "error", err)
		h.writeError(w, protocol.StatusBadRequest, "bad request")
		return
	}
	h.logger.Error("parse request failed", "error", err)
	h.writeError(w, protocol.StatusServerError, "could not read request")
}

func isReadVerb(verb string) bool {
	return verb == protocol.VerbFetch || verb == protocol.VerbList ||
		verb == protocol.VerbVersions || verb == protocol.VerbLookup
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

// versionedFetch is parseVersionPath minus directories: /api/v2 under a
// directory /api is a path, since a directory can never hold versions.
func (h *Handler) versionedFetch(ctx context.Context, reqPath string, reader storagebackend.Reader) (basePath string, version int) {
	basePath, version = parseVersionPath(reqPath)
	if version == 0 {
		return reqPath, 0
	}
	isDir, err := reader.IsDir(ctx, basePath)
	if err != nil && !errors.Is(err, storagebackend.ErrNotFound) {
		h.logger.Warn("isdir check failed; treating as a version path", "path", sanitize(basePath), "error", err)
	}
	if isDir {
		return reqPath, 0
	}
	return basePath, version
}

func (h *Handler) handleFetchByHash(ctx context.Context, call *readCall, hash string) {
	w, req, reader := call.w, call.req, call.view
	docPath, err := reader.LookupHash(ctx, hash)
	if errors.Is(err, storagebackend.ErrNotFound) {
		h.logger.Info("hash not found", "hash", hash)
		h.writeError(w, protocol.StatusNotFound, "content not found for hash "+hash)
		return
	}
	if err != nil {
		h.logger.Error("hash lookup failed", "hash", hash, "error", err)
		h.writeError(w, protocol.StatusServerError, "internal error")
		return
	}

	// Check read auth on the resolved path — knowing a hash must not bypass access control.
	pathReq := req
	pathReq.Path = docPath
	if !h.authorizeRead(w, pathReq) {
		return
	}

	doc, err := reader.Get(ctx, docPath, 0)
	if err != nil {
		h.logger.Error("fetch by hash failed", "hash", hash, "path", sanitize(docPath), "error", err)
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
	if err := h.checkReadAuth(req.Path, req.Metadata["auth"]); err != nil {
		h.writeAuthDenied(w, req, err)
		return false
	}
	return true
}

// writeAuthDenied answers a failed authorization: a missing, unknown or
// expired token is unauthorized, anything else is not permitted.
func (h *Handler) writeAuthDenied(w io.Writer, req protocol.Request, err error) {
	if auth.IsUnauthenticated(err) {
		h.logger.Warn("unauthorized", "operation", req.Verb, "path", sanitize(req.Path))
		h.writeError(w, protocol.StatusUnauthorized, "authentication required")
		return
	}
	h.logger.Warn("not permitted", "operation", req.Verb, "path", sanitize(req.Path))
	h.writeError(w, protocol.StatusNotPermitted, "insufficient permissions")
}

// checkReadAuth decides a read without writing a response: nil when allowed,
// otherwise the auth verdict.
func (h *Handler) checkReadAuth(reqPath, token string) error {
	directoryPath := strings.HasSuffix(reqPath, "/")
	reqPath = storefmt.CanonicalPath(reqPath)
	if directoryPath && reqPath != "/" {
		reqPath += "/"
	}
	return h.tokenStore().AuthorizeRead(token, reqPath)
}

func (h *Handler) handleFetch(ctx context.Context, call *readCall) {
	w, req, reader := call.w, call.req, call.view
	// Check for content-addressed hash: FETCH /sha256-<64hex>
	// Read auth for hash paths is checked after resolving to a real path.
	if hash, ok := protocol.IsHashPath(req.Path); ok {
		h.handleFetchByHash(ctx, call, hash)
		return
	}

	// For versioned paths, check read auth on the base path so that
	// /doc.md/v2 is gated by the same token as /doc.md.
	if basePath, version := h.versionedFetch(ctx, req.Path, reader); version > 0 {
		authReq := req
		authReq.Path = basePath
		if !h.authorizeRead(w, authReq) {
			return
		}
		h.handleFetchVersion(ctx, call, basePath, version)
		return
	}

	if !h.authorizeRead(w, req) {
		return
	}

	doc, err := reader.Get(ctx, req.Path, 0)
	if err != nil {
		if errors.Is(err, storagebackend.ErrNotFound) {
			// Check if the path is a directory — serve index.md or auto-generate listing.
			isDir, dirErr := reader.IsDir(ctx, req.Path)
			if dirErr != nil && !errors.Is(dirErr, storagebackend.ErrNotFound) {
				h.logger.Error("isdir check failed", "path", sanitize(req.Path), "error", dirErr)
				h.writeError(w, protocol.StatusServerError, "internal error")
				return
			}
			if isDir {
				h.handleFetchDirectory(ctx, call)
				return
			}
			h.logger.Info("not found", "path", sanitize(req.Path))
			h.writeError(w, protocol.StatusNotFound, req.Path+" not found")
			return
		}
		h.logger.Error("fetch failed", "path", sanitize(req.Path), "error", err)
		h.writeError(w, protocol.StatusServerError, "internal error")
		return
	}

	h.serveDocument(w, req, doc, req.Path)
}

// serveDocument handles the common document-serving logic: archived check,
// conditional request handling (etag / if-modified-since), and response
// assembly. doc.Content is body-only per the store contract.
func (h *Handler) serveDocument(w io.Writer, req protocol.Request, doc *storefmt.Document, logPath string) {
	if doc.Archived {
		h.logger.Info("archived", "path", sanitize(logPath))
		h.writeError(w, protocol.StatusArchived, logPath+" is archived")
		return
	}

	h.serveStoredDocument(w, req, doc, logPath, nil)
}

func (h *Handler) serveStoredDocument(w io.Writer, req protocol.Request, doc *storefmt.Document, logPath string, extra map[string]string) {
	if doc.ETag == "" {
		h.logger.Error("stored document missing etag", "path", sanitize(logPath), "version", doc.Version)
		h.writeError(w, protocol.StatusServerError, "internal error")
		return
	}

	if ifNoneMatch, ok := req.Metadata["if-none-match"]; ok && ifNoneMatch == doc.ETag {
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

	// Copy publisher metadata first, then set server-owned keys so they can't be overwritten.
	meta := make(map[string]string)
	copyPublisherMeta(meta, doc.Metadata)
	meta["modified"] = doc.Modified.Format(time.RFC3339)
	meta["etag"] = doc.ETag
	meta["version"] = strconv.Itoa(doc.Version)
	meta["content-hash"] = storefmt.ContentHash(doc.Content)
	maps.Copy(meta, extra)
	h.writeResponse(w, protocol.Response{Status: protocol.StatusOK, Metadata: meta, Body: string(doc.Content)})
}

func (h *Handler) writeNotModified(w io.Writer) {
	resp := protocol.Response{
		Status:   protocol.StatusNotModified,
		Metadata: map[string]string{},
	}
	h.writeResponse(w, resp)
}

func (h *Handler) handleList(ctx context.Context, call *readCall) {
	w, req := call.w, call.req
	if !h.authorizeRead(w, req) {
		return
	}
	reqPath := req.Path
	if _, ok := protocol.IsHashPath(reqPath); ok {
		h.writeError(w, protocol.StatusNotFound, reqPath+" not found")
		return
	}
	includeArchived, err := parseListIncludeArchived(req.Metadata["include-archived"])
	if err != nil {
		h.writeError(w, protocol.StatusBadRequest, err.Error())
		return
	}
	pageSize, err := parseListPageSize(req.Metadata["page-size"])
	if err != nil {
		h.writeError(w, protocol.StatusBadRequest, err.Error())
		return
	}
	after, err := decodeListCursor(req.Metadata["cursor"], reqPath, includeArchived)
	if err != nil {
		h.writeError(w, protocol.StatusBadRequest, err.Error())
		return
	}
	// One entry past the page tells a full page from the last one.
	window := storefmt.ListOptions{IncludeArchived: includeArchived, After: after, Limit: pageSize + 1}
	entries, err := h.readableEntries(ctx, call, window)
	if err != nil {
		if errors.Is(err, storagebackend.ErrNotFound) {
			h.logger.Info("not found", "path", sanitize(reqPath))
			h.writeError(w, protocol.StatusNotFound, reqPath+" not found")
			return
		}
		h.logger.Error("list failed", "path", sanitize(reqPath), "error", err)
		h.writeError(w, protocol.StatusServerError, "internal error")
		return
	}

	page, err := buildDirectoryPage(reqPath, entries, pageSize)
	if err != nil {
		h.logger.Error("build list page failed", "path", sanitize(reqPath), "error", err)
		h.writeError(w, protocol.StatusServerError, "internal error")
		return
	}
	nextCursor := ""
	if !page.Complete {
		nextCursor, err = encodeListCursor(reqPath, includeArchived, page.LastName)
		if err != nil {
			h.logger.Error("encode list cursor failed", "path", sanitize(reqPath), "error", err)
			h.writeError(w, protocol.StatusServerError, "internal error")
			return
		}
	}
	metadata := render.ListMetadata(page.EntryCount, nextCursor)

	resp := protocol.Response{
		Status:   protocol.StatusOK,
		Metadata: metadata,
		Body:     page.Body,
	}
	h.writeResponse(w, resp)
}

// readableEntries returns up to window.Limit entries the caller may read, in
// order. Denied entries do not count, so it reads further windows until the
// page fills or the directory ends.
func (h *Handler) readableEntries(ctx context.Context, call *readCall, window storefmt.ListOptions) ([]storefmt.DirEntry, error) {
	reqPath, token := call.req.Path, call.req.Metadata["auth"]
	var visible []storefmt.DirEntry
	for {
		chunk, err := call.view.ListEntries(ctx, reqPath, window)
		if err != nil {
			return nil, err
		}
		readable, err := h.filterReadableEntries(reqPath, chunk, token)
		if err != nil {
			return nil, fmt.Errorf("filter list authorization: %w", err)
		}
		visible = append(visible, readable...)
		if len(visible) >= window.Limit || len(chunk) < window.Limit {
			return visible[:min(len(visible), window.Limit)], nil
		}
		window.After = chunk[len(chunk)-1].Name
	}
}

func (h *Handler) filterReadableEntries(reqPath string, entries []storefmt.DirEntry, token string) ([]storefmt.DirEntry, error) {
	visible := make([]storefmt.DirEntry, 0, len(entries))
	for _, entry := range entries {
		entryPath := path.Join(reqPath, entry.Name)
		if entry.IsDir {
			entryPath += "/"
		}
		err := h.checkReadAuth(entryPath, token)
		switch {
		case err == nil:
			visible = append(visible, entry)
		case !auth.IsDenial(err):
			// A denial hides the entry; anything else must fail the whole
			// listing rather than hide inventory.
			return nil, err
		}
	}
	return visible, nil
}

// buildDirectoryIndex renders the bounded first page used by directory FETCH.
func buildDirectoryIndex(reqPath string, entries []storefmt.DirEntry) (body string, entryCount int, err error) {
	page, err := buildDirectoryPage(reqPath, entries, MaxDirectoryEntries)
	return page.Body, page.EntryCount, err
}

// lookupRows converts catalog results into the rows protocol/render draws.
func lookupRows(results []catalog.Result) []render.LookupRow {
	rows := make([]render.LookupRow, 0, len(results))
	for i := range results {
		r := &results[i]
		rows = append(rows, render.LookupRow{
			Path:       r.Path,
			Anchor:     r.Anchor,
			Importance: r.Importance,
			Title:      r.DisplayTitle(),
			Tags:       r.Tags,
			Snippet:    r.Snippet,
		})
	}
	return rows
}

func (h *Handler) handleFetchDirectory(ctx context.Context, call *readCall) {
	w, req, reader := call.w, call.req, call.view
	includeArchived := req.Metadata["include-archived"] == "true"

	// Try index.md first — if the directory has an explicit index, serve it as
	// a normal document. An ARCHIVED index.md is treated like a missing one:
	// serveDocument would tombstone it, blocking the whole directory view while
	// LIST still shows the directory's live entries. Fetching the archived
	// index.md itself (by its own path) still returns the tombstone.
	indexPath := path.Join(req.Path, "index.md")
	doc, err := reader.Get(ctx, indexPath, 0)
	if err != nil && !errors.Is(err, storagebackend.ErrNotFound) {
		h.logger.Error("fetch index failed", "path", sanitize(indexPath), "error", err)
		h.writeError(w, protocol.StatusServerError, "internal error")
		return
	}
	if err == nil && !doc.Archived {
		authReq := req
		authReq.Path = indexPath
		if !h.authorizeRead(w, authReq) {
			return
		}
		h.serveDocument(w, req, doc, indexPath)
		return
	}
	// No (visible) index.md — generate a directory listing.
	window := storefmt.ListOptions{IncludeArchived: includeArchived, Limit: MaxDirectoryEntries + 1}
	entries, err := h.readableEntries(ctx, call, window)
	if err != nil {
		if errors.Is(err, storagebackend.ErrNotFound) {
			h.logger.Info("not found", "path", sanitize(req.Path))
			h.writeError(w, protocol.StatusNotFound, req.Path+" not found")
			return
		}
		h.logger.Error("fetch directory failed", "path", sanitize(req.Path), "error", err)
		h.writeError(w, protocol.StatusServerError, "internal error")
		return
	}
	h.serveGeneratedDirectory(w, req, entries)
}

func (h *Handler) serveGeneratedDirectory(w io.Writer, req protocol.Request, entries []storefmt.DirEntry) {
	body, entryCount, err := buildDirectoryIndex(req.Path, entries)
	if err != nil {
		h.logger.Error("build generated directory failed", "path", sanitize(req.Path), "error", err)
		h.writeError(w, protocol.StatusServerError, "internal error")
		return
	}
	etag := storefmt.StoredETag([]byte(body))
	if ifNoneMatch, ok := req.Metadata["if-none-match"]; ok && ifNoneMatch == etag {
		h.writeNotModified(w)
		return
	}
	h.writeResponse(w, protocol.Response{
		Status: protocol.StatusOK,
		Metadata: map[string]string{
			"entries":      strconv.Itoa(entryCount),
			"etag":         etag,
			"content-hash": storefmt.ContentHash([]byte(body)),
		},
		Body: body,
	})
}

func (h *Handler) handleFetchVersion(ctx context.Context, call *readCall, basePath string, version int) {
	w, req, reader := call.w, call.req, call.view
	doc, err := reader.Get(ctx, basePath, version)
	if err != nil {
		if errors.Is(err, storagebackend.ErrNotFound) {
			h.logger.Info("not found", "path", sanitize(basePath), "version", version)
			h.writeError(w, protocol.StatusNotFound, req.Path+" not found")
			return
		}
		h.logger.Error("fetch version failed", "path", sanitize(basePath), "version", version, "error", err)
		h.writeError(w, protocol.StatusServerError, "internal error")
		return
	}

	// Indicate current version so client knows if this is historical.
	versions, err := reader.Versions(ctx, basePath)
	if err != nil || len(versions) == 0 {
		h.logger.Error("read current version failed", "path", sanitize(basePath), "error", err)
		h.writeError(w, protocol.StatusServerError, "internal error")
		return
	}
	current := versions[0].Version
	h.serveStoredDocument(w, req, doc, basePath, map[string]string{
		"current-version": strconv.Itoa(current),
	})
}

func (h *Handler) handleVersions(ctx context.Context, call *readCall) {
	w, req, reader := call.w, call.req, call.view
	if !h.authorizeRead(w, req) {
		return
	}
	reqPath := req.Path
	if _, ok := protocol.IsHashPath(reqPath); ok {
		h.writeError(w, protocol.StatusNotFound, reqPath+" not found")
		return
	}

	versions, err := reader.Versions(ctx, reqPath)
	if err != nil {
		if errors.Is(err, storagebackend.ErrNotFound) {
			h.logger.Info("not found", "path", sanitize(reqPath))
			h.writeError(w, protocol.StatusNotFound, reqPath+" not found")
			return
		}
		h.logger.Error("versions failed", "path", sanitize(reqPath), "error", err)
		h.writeError(w, protocol.StatusServerError, "internal error")
		return
	}

	// Verify hash chain integrity and report result.
	chainValid := true
	if err := reader.VerifyChain(ctx, reqPath); err != nil {
		if !errors.Is(err, storefmt.ErrIntegrity) {
			h.logger.Error("chain verification failed", "path", sanitize(reqPath), "error", err)
			h.writeError(w, protocol.StatusServerError, "internal error")
			return
		}
		h.logger.Warn("chain verification failed", "path", sanitize(reqPath), "error", err)
		chainValid = false
	}

	rows := make([]render.VersionEntry, 0, len(versions))
	for _, v := range versions {
		rows = append(rows, render.VersionEntry{Version: v.Version, Modified: v.Modified})
	}
	h.writeResponse(w, render.VersionsResponse(reqPath, rows, chainValid))
}

func (h *Handler) handleLookup(ctx context.Context, call *readCall) {
	w, req, reader := call.w, call.req, call.view
	lookup := storagebackend.CatalogReader(call.view)

	query := strings.TrimSpace(req.Metadata["query"])
	// "*" is the match-all query (whole catalog under scope, importance
	// order); anything else needs at least 2 characters of subject.
	if query != "*" && len([]rune(query)) < 2 {
		h.writeError(w, protocol.StatusBadRequest, "query must be at least 2 characters")
		return
	}
	// Bounded here, not per backend, so both reject the same queries.
	if n := catalog.CountTerms(query, maxLookupTerms); n > maxLookupTerms {
		h.writeError(w, protocol.StatusBadRequest,
			fmt.Sprintf("query has too many terms: %d > %d", n, maxLookupTerms))
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
	matchValue, matchCarried := req.Metadata["match"]
	mode, err := catalog.ParseMode(matchValue)
	if err != nil {
		h.writeError(w, protocol.StatusBadRequest, err.Error())
		return
	}

	// Scope must be the whole-server root or an existing directory.
	if req.Path != "/" {
		isDir, derr := reader.IsDir(ctx, req.Path)
		if derr != nil && !errors.Is(derr, storagebackend.ErrNotFound) {
			h.logger.Error("lookup isdir check failed", "path", sanitize(req.Path), "error", derr)
			h.writeError(w, protocol.StatusServerError, "internal error")
			return
		}
		if !isDir {
			h.logger.Info("lookup scope not found", "path", sanitize(req.Path))
			h.writeError(w, protocol.StatusNotFound, req.Path+" not found")
			return
		}
	}

	results, err := lookup.Lookup(ctx, query, catalog.Options{Scope: req.Path, Filter: preds, Max: maxLookupResults, Match: mode})
	if err != nil {
		h.logger.Error("lookup failed", "path", sanitize(req.Path), "error", err)
		h.writeError(w, protocol.StatusServerError, "internal error")
		return
	}

	// Filter by read authorization before truncating to limit, so protected
	// documents the requester cannot see never displace authorized matches and
	// never reveal their existence (no path, no title, not counted).
	token := req.Metadata["auth"]
	rows := make([]catalog.Result, 0, len(results))
	for i := range results {
		// An authorization error denies the row; the reason is logged, not
		// shown to the requester.
		if authErr := h.checkReadAuth(results[i].Path, token); authErr != nil {
			h.logger.Warn("lookup read authorization failed", "path", sanitize(results[i].Path), "error", authErr)
			continue
		}
		rows = append(rows, results[i])
		if len(rows) >= limit {
			break
		}
	}

	// Echo match only when asked, so a request without it gets the
	// response it always got, byte for byte.
	echo := ""
	if matchCarried {
		echo = string(mode)
	}
	h.writeResponse(w, render.LookupResponse(query, req.Path, lookupRows(rows), echo))
}

func (h *Handler) handleArchive(ctx context.Context, call *writeCall) {
	w, req, tokenLabel := call.w, call.req, call.tokenLabel

	archive, err := h.store.SetArchived(ctx, storagebackend.ArchiveRequest{Path: req.Path, Archived: true})
	doc := archive.Document
	if err != nil {
		h.writeArchiveError(call, "ARCHIVE", err)
		return
	}

	h.logger.Info("archive", "audit", true, "operation", "ARCHIVE", "path", sanitize(req.Path), "version", doc.Version, "token_label", sanitize(tokenLabel), "success", true)
	resp := protocol.Response{
		Status: protocol.StatusOK,
		Metadata: map[string]string{
			"version":  strconv.Itoa(doc.Version),
			"archived": "true",
		},
	}
	h.writeResponse(w, resp)
}

// writeArchiveError answers a failed archive transition in either direction.
func (h *Handler) writeArchiveError(call *writeCall, operation string, err error) {
	w, reqPath, label := call.w, sanitize(call.req.Path), sanitize(call.tokenLabel)
	if errors.Is(err, storagebackend.ErrNotFound) {
		h.logger.Info("archive failed", "audit", true, "operation", operation, "path", reqPath, "token_label", label, "success", false, "reason", "not found")
		h.writeError(w, protocol.StatusNotFound, call.req.Path+" not found")
		return
	}
	if r := classifyRefusal(err); r != nil {
		h.logger.Info("archive rejected", "audit", true, "operation", operation, "path", reqPath, "token_label", label, "success", false, "reason", r.reason)
		h.writeError(w, r.status, r.message)
		return
	}
	h.logger.Error("archive failed", "audit", true, "operation", operation, "path", reqPath, "token_label", label, "success", false, "error", err)
	h.writeError(w, protocol.StatusServerError, "internal error")
}

// handleUnarchive is PUBLISH with an empty body: it restores an archived
// document and is a no-op on a live one.
func (h *Handler) handleUnarchive(ctx context.Context, call *writeCall) {
	w, req, tokenLabel := call.w, call.req, call.tokenLabel
	archive, err := h.store.SetArchived(ctx, storagebackend.ArchiveRequest{Path: req.Path})
	if err != nil {
		h.writeArchiveError(call, "UNARCHIVE", err)
		return
	}
	if archive.Changed {
		h.logger.Info("unarchive", "audit", true, "operation", "UNARCHIVE", "path", sanitize(req.Path), "version", archive.Document.Version, "token_label", sanitize(tokenLabel), "success", true)
	}
	h.writeResponse(w, protocol.Response{
		Status:   protocol.StatusOK,
		Metadata: map[string]string{"version": strconv.Itoa(archive.Document.Version)},
	})
}

func (h *Handler) handlePublish(ctx context.Context, call *writeCall) {
	w, req, tokenLabel := call.w, call.req, call.tokenLabel

	// Contract before the empty-body shortcut: a non-.md path is rejected
	// even with an empty body.
	if err := storefmt.ValidateDocumentContent(req.Path, []byte(req.Body)); err != nil {
		h.logger.Info("publish rejected", "audit", true, "operation", "PUBLISH", "path", sanitize(req.Path), "token_label", sanitize(tokenLabel), "success", false, "reason", "invalid document")
		h.writeError(w, protocol.StatusBadRequest, err.Error())
		return
	}

	if req.Body == "" {
		h.handleUnarchive(ctx, call)
		return
	}

	pubMeta, err := extractPublisherMeta(req.Metadata)
	if err != nil {
		h.writeError(w, protocol.StatusBadRequest, err.Error())
		return
	}
	pubMeta = storefmt.ApplyOKFTypeDefault(req.Path, pubMeta)
	if err := storefmt.ValidateMeta(pubMeta); err != nil {
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

	doc, err := h.store.Publish(ctx, storagebackend.WriteRequest{Path: req.Path, ExpectedVersion: expectedVersion, Content: []byte(req.Body), Metadata: pubMeta})
	h.logPrune("PUBLISH", req.Path, tokenLabel, doc)
	if err != nil {
		h.writePublishError(w, req, expectedVersion, doc, err, tokenLabel)
		return
	}

	h.logger.Info("publish", "audit", true, "operation", "PUBLISH", "path", sanitize(req.Path), "version", doc.Version, "token_label", sanitize(tokenLabel), "success", true, "size_bytes", len(req.Body))
	resp := protocol.Response{
		Status: protocol.StatusCreated,
		Metadata: map[string]string{
			"version":  strconv.Itoa(doc.Version),
			"modified": doc.Modified.Format(time.RFC3339),
		},
	}
	h.writeResponse(w, resp)
}

// refusal is a store error the client can act on, with its response.
type refusal struct {
	status  string
	message string
	reason  string
}

// classifyRefusal maps backend-neutral refusals; nil means a server fault.
func classifyRefusal(err error) *refusal {
	var rejection storagebackend.Rejection
	switch {
	case errors.Is(err, storefmt.ErrPathCollision):
		return &refusal{status: protocol.StatusBadRequest, message: err.Error(), reason: "path collision"}
	case errors.Is(err, storagebackend.ErrReadOnly):
		return &refusal{status: protocol.StatusNotPermitted, message: "server is read-only", reason: "read-only store"}
	case errors.Is(err, storagebackend.ErrQuota):
		return &refusal{status: protocol.StatusNotPermitted, message: err.Error(), reason: "quota"}
	case errors.As(err, &rejection):
		return &refusal{status: protocol.StatusBadRequest, message: rejection.RejectionMessage(), reason: "rejected"}
	case errors.Is(err, storagebackend.ErrRejected):
		return &refusal{status: protocol.StatusBadRequest, message: err.Error(), reason: "rejected"}
	}
	return nil
}

// writePublishError maps a Publish failure onto the PUBLISH response, covering
// every sentinel in the DocumentStore error contract.
func (h *Handler) writePublishError(w io.Writer, req protocol.Request, expectedVersion int, doc *storefmt.Document, err error, tokenLabel string) {
	refused := classifyRefusal(err)
	switch {
	case errors.Is(err, storefmt.ErrConflict):
		h.logger.Info("publish conflict", "audit", true, "operation", "PUBLISH", "path", sanitize(req.Path), "expected_version", expectedVersion, "server_version", doc.Version, "token_label", sanitize(tokenLabel), "success", false)
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
	case errors.Is(err, storefmt.ErrNotModified):
		h.logger.Info("publish unchanged", "audit", true, "operation", "PUBLISH", "path", sanitize(req.Path), "version", doc.Version, "token_label", sanitize(tokenLabel), "success", true)
		h.writeResponse(w, protocol.Response{
			Status: protocol.StatusOK,
			Metadata: map[string]string{
				"version":  strconv.Itoa(doc.Version),
				"modified": doc.Modified.Format(time.RFC3339),
			},
		})
	case errors.Is(err, storefmt.ErrArchived):
		h.logger.Info("publish rejected", "audit", true, "operation", "PUBLISH", "path", sanitize(req.Path), "token_label", sanitize(tokenLabel), "success", false, "reason", "archived")
		h.writeError(w, protocol.StatusArchived, "document is archived; unarchive first")
	case errors.Is(err, storagebackend.ErrNotFound):
		h.logger.Info("publish failed", "audit", true, "operation", "PUBLISH", "path", sanitize(req.Path), "token_label", sanitize(tokenLabel), "success", false, "reason", "not found")
		h.writeError(w, protocol.StatusNotFound, req.Path+" not found")
	case errors.Is(err, storefmt.ErrSizeLimit):
		h.logger.Info("publish rejected", "audit", true, "operation", "PUBLISH", "path", sanitize(req.Path), "token_label", sanitize(tokenLabel), "success", false, "reason", "size limit exceeded")
		h.writeError(w, protocol.StatusServerError, "content exceeds size limit")
	case errors.Is(err, storefmt.ErrInvalidMeta):
		h.logger.Info("publish rejected", "audit", true, "operation", "PUBLISH", "path", sanitize(req.Path), "token_label", sanitize(tokenLabel), "success", false, "reason", "invalid metadata")
		h.writeError(w, protocol.StatusBadRequest, err.Error())
	case refused != nil:
		h.logger.Info("publish rejected", "audit", true, "operation", "PUBLISH", "path", sanitize(req.Path), "token_label", sanitize(tokenLabel), "success", false, "reason", refused.reason)
		h.writeError(w, refused.status, refused.message)
	default:
		h.logger.Error("publish failed", "audit", true, "operation", "PUBLISH", "path", sanitize(req.Path), "token_label", sanitize(tokenLabel), "success", false, "error", err)
		h.writeError(w, protocol.StatusServerError, "internal error")
	}
}

func (h *Handler) handleAppend(ctx context.Context, call *writeCall) {
	w, req, tokenLabel := call.w, call.req, call.tokenLabel

	// Contract before the empty-body check: a non-.md path is rejected as
	// bad-request rather than reported as a missing body.
	if err := storefmt.ValidateDocumentContent(req.Path, []byte(req.Body)); err != nil {
		h.logger.Info("append rejected", "audit", true, "operation", "APPEND", "path", sanitize(req.Path), "token_label", sanitize(tokenLabel), "success", false, "reason", "invalid document")
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
	if err := storefmt.ValidateMeta(pubMeta); err != nil {
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

	doc, err := h.store.Append(ctx, storagebackend.WriteRequest{Path: req.Path, ExpectedVersion: expectedVersion, Content: []byte(req.Body), Metadata: pubMeta})
	h.logPrune("APPEND", req.Path, tokenLabel, doc)
	if err != nil {
		h.writeAppendError(w, req, expectedVersion, doc, err, tokenLabel)
		return
	}

	h.logger.Info("append", "audit", true, "operation", "APPEND", "path", sanitize(req.Path), "version", doc.Version, "token_label", sanitize(tokenLabel), "success", true, "size_bytes", len(req.Body))
	resp := protocol.Response{
		Status: protocol.StatusCreated,
		Metadata: map[string]string{
			"version":  strconv.Itoa(doc.Version),
			"modified": doc.Modified.Format(time.RFC3339),
		},
	}
	h.writeResponse(w, resp)
}

func (h *Handler) writeAppendError(w io.Writer, req protocol.Request, expectedVersion int, doc *storefmt.Document, err error, tokenLabel string) {
	refused := classifyRefusal(err)
	switch {
	case errors.Is(err, storefmt.ErrConflict):
		h.logger.Info("append conflict", "audit", true, "operation", "APPEND", "path", sanitize(req.Path), "expected_version", expectedVersion, "server_version", doc.Version, "token_label", sanitize(tokenLabel), "success", false)
		body := fmt.Sprintf("# Version Conflict\n\nThe document has been modified since you last fetched it.\n\nYour version: %d\nServer version: %d\n\nFetch the latest version and verify whether your append was applied before retrying.\n", expectedVersion, doc.Version)
		h.writeResponse(w, protocol.Response{
			Status: protocol.StatusConflict,
			Metadata: map[string]string{
				"your-version":   strconv.Itoa(expectedVersion),
				"server-version": strconv.Itoa(doc.Version),
			},
			Body: body,
		})
	case errors.Is(err, storefmt.ErrArchived):
		h.logger.Info("append rejected", "audit", true, "operation", "APPEND", "path", sanitize(req.Path), "token_label", sanitize(tokenLabel), "success", false, "reason", "archived")
		h.writeError(w, protocol.StatusArchived, "document is archived; unarchive first")
	case errors.Is(err, storagebackend.ErrNotFound):
		h.logger.Info("not found", "path", sanitize(req.Path))
		h.writeError(w, protocol.StatusNotFound, req.Path+" not found")
	case errors.Is(err, storefmt.ErrSizeLimit):
		h.logger.Info("append rejected", "audit", true, "operation", "APPEND", "path", sanitize(req.Path), "token_label", sanitize(tokenLabel), "success", false, "reason", "size limit exceeded")
		h.writeError(w, protocol.StatusServerError, "content exceeds size limit")
	case errors.Is(err, storefmt.ErrInvalidMeta):
		h.logger.Info("append rejected", "audit", true, "operation", "APPEND", "path", sanitize(req.Path), "token_label", sanitize(tokenLabel), "success", false, "reason", "merged metadata invalid")
		h.writeError(w, protocol.StatusBadRequest, err.Error())
	case refused != nil:
		h.logger.Info("append rejected", "audit", true, "operation", "APPEND", "path", sanitize(req.Path), "token_label", sanitize(tokenLabel), "success", false, "reason", refused.reason)
		h.writeError(w, refused.status, refused.message)
	default:
		h.logger.Error("append failed", "path", sanitize(req.Path), "error", err)
		h.writeError(w, protocol.StatusServerError, "internal error")
	}
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
		h.logger.Error("write response failed", "error", err)
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

// logPrune audit-logs retention pruning. Deletions must be attributable, so
// it runs on every write outcome carrying a prune result, conflict paths
// included: there the write and its prune happened despite the response.
func (h *Handler) logPrune(operation, reqPath, tokenLabel string, doc *storefmt.Document) {
	if doc == nil || doc.Prune == nil {
		return
	}
	p := doc.Prune
	if p.Err != nil {
		h.logger.Error("prune incomplete", "audit", true, "operation", operation, "path", sanitize(reqPath), "pruned_from", p.From, "pruned_to", p.To, "token_label", sanitize(tokenLabel), "success", false, "error", p.Err)
		return
	}
	h.logger.Info("prune", "audit", true, "operation", operation, "path", sanitize(reqPath), "pruned_from", p.From, "pruned_to", p.To, "token_label", sanitize(tokenLabel), "success", true)
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
		if protocol.IsReservedMetadataKey(k) {
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
		size += storefmt.SerializedMetaSize(k, v)
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
		if protocol.IsReservedMetadataKey(k) || controlKeys[k] {
			continue
		}
		if !protocol.IsValidMetaKey(k) || !protocol.IsValidMetaValue(v) {
			continue
		}
		dst[k] = v
	}
}
