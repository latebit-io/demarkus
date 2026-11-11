package handler

import (
	"context"
	"errors"
	"io"
	"maps"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/render"
	"github.com/latebit-io/demarkus/protocol/storefmt"
	storagebackend "github.com/latebit-io/demarkus/server/internal/backend"
)

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

func (h *Handler) handleFetch(ctx context.Context, call *readCall) {
	w, req, reader := call.w, call.req, call.view

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

	h.serveDocument(call, doc, req.Path)
}

// serveDocument handles the common document-serving logic: archived check,
// conditional request handling (etag / if-modified-since), and response
// assembly. doc.Content is body-only per the store contract.
func (h *Handler) serveDocument(call *readCall, doc *storefmt.Document, logPath string) {
	if doc.Archived {
		h.logger.Info("archived", "path", sanitize(logPath))
		h.writeError(call.w, protocol.StatusArchived, logPath+" is archived")
		return
	}

	h.serveStoredDocument(call, doc, logPath, nil)
}

func (h *Handler) serveStoredDocument(call *readCall, doc *storefmt.Document, logPath string, extra map[string]string) {
	w, req := call.w, call.req
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
	stampVersion(meta, doc)
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

func (h *Handler) handleFetchDirectory(ctx context.Context, call *readCall) {
	w, req, reader := call.w, call.req, call.view
	includeArchived := req.Metadata["include-archived"] == "true"

	// An explicit index.md is the directory's document. An archived one counts
	// as missing: its tombstone would hide a directory LIST still shows. A
	// fetch of the archived index.md by its own path still answers archived.
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
		h.serveDocument(call, doc, indexPath)
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
	h.serveStoredDocument(call, doc, basePath, map[string]string{
		"current-version": strconv.Itoa(current),
	})
}

func (h *Handler) handleVersions(ctx context.Context, call *readCall) {
	w, req, reader := call.w, call.req, call.view
	if !h.authorizeRead(w, req) {
		return
	}
	reqPath := req.Path

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

// stampVersion sets the keys that describe a stored version, on a fetch and
// on the response to the write that made it.
func stampVersion(meta map[string]string, doc *storefmt.Document) {
	meta["version"] = strconv.Itoa(doc.Version)
	meta["modified"] = doc.Modified.Format(time.RFC3339)
	meta["etag"] = doc.ETag
	meta["content-hash"] = storefmt.ContentHash(doc.Content)
}
