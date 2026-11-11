package handler

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/render"
	"github.com/latebit-io/demarkus/protocol/storefmt"
	"github.com/latebit-io/demarkus/server/internal/auth"
	storagebackend "github.com/latebit-io/demarkus/server/internal/backend"
)

const (
	listCursorVersion = byte(1)
	listCursorArchive = byte(1)
)

var errListPageCannotProgress = errors.New("one LIST entry exceeds the response size limit")

type directoryPage struct {
	Body       string
	EntryCount int
	Complete   bool
	LastName   string
}

func parseListPageSize(raw string) (int, error) {
	if raw == "" {
		return MaxDirectoryEntries, nil
	}
	size, err := strconv.Atoi(raw)
	if err != nil || size < 1 || size > MaxDirectoryEntries {
		return 0, fmt.Errorf("page-size must be between 1 and %d", MaxDirectoryEntries)
	}
	return size, nil
}

func parseListIncludeArchived(raw string) (bool, error) {
	switch raw {
	case "", "false":
		return false, nil
	case "true":
		return true, nil
	default:
		return false, errors.New("include-archived must be true or false")
	}
}

func encodeListCursor(reqPath string, includeArchived bool, after string) (string, error) {
	if after == "" {
		return "", errors.New("LIST cursor requires a non-empty entry name")
	}
	if len(reqPath) > protocol.MaxRequestPathLength || len(reqPath) > int(^uint16(0)) {
		return "", errors.New("LIST cursor path exceeds limit")
	}
	flags := byte(0)
	if includeArchived {
		flags = listCursorArchive
	}
	raw := make([]byte, 4, 4+len(reqPath)+len(after))
	raw[0] = listCursorVersion
	raw[1] = flags
	binary.BigEndian.PutUint16(raw[2:4], uint16(len(reqPath)))
	raw = append(raw, reqPath...)
	raw = append(raw, after...)
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func decodeListCursor(encoded, reqPath string, includeArchived bool) (string, error) {
	if encoded == "" {
		return "", nil
	}
	maxRaw := 4 + 2*protocol.MaxRequestPathLength
	if base64.RawURLEncoding.DecodedLen(len(encoded)) > maxRaw {
		return "", errors.New("LIST cursor exceeds limit")
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(raw) < 5 {
		return "", errors.New("invalid LIST cursor")
	}
	if raw[0] != listCursorVersion || raw[1]&^listCursorArchive != 0 {
		return "", errors.New("unsupported LIST cursor")
	}
	pathLen := int(binary.BigEndian.Uint16(raw[2:4]))
	if pathLen == 0 || 4+pathLen >= len(raw) {
		return "", errors.New("invalid LIST cursor")
	}
	if string(raw[4:4+pathLen]) != reqPath || (raw[1]&listCursorArchive != 0) != includeArchived {
		return "", errors.New("LIST cursor does not match this request")
	}
	after := string(raw[4+pathLen:])
	if after == "" {
		return "", errors.New("invalid LIST cursor")
	}
	return after, nil
}

// buildDirectoryPage renders entries, already windowed past the cursor, up to
// pageSize and the body limit; anything left over makes the page incomplete.
func buildDirectoryPage(reqPath string, entries []storefmt.DirEntry, pageSize int) (directoryPage, error) {
	for i := 1; i < len(entries); i++ {
		if entries[i-1].Name >= entries[i].Name {
			return directoryPage{}, errors.New("LIST entries are not strictly ordered")
		}
	}

	var body strings.Builder
	body.WriteString(render.IndexHeading(reqPath))

	page := directoryPage{}
	next := 0
	for next < len(entries) && page.EntryCount < pageSize {
		line := render.EntryLine(entries[next].Name, entries[next].IsDir)
		moreAfter := next+1 < len(entries)
		reserve := 0
		if moreAfter {
			reserve = len(render.ListTruncatedNote)
		}
		if body.Len()+len(line)+reserve > protocol.MaxBodyLength {
			if page.EntryCount == 0 {
				return directoryPage{}, errListPageCannotProgress
			}
			break
		}
		body.WriteString(line)
		page.EntryCount++
		page.LastName = entries[next].Name
		next++
	}
	page.Complete = next == len(entries)
	if !page.Complete {
		body.WriteString(render.ListTruncatedNote)
	}
	page.Body = body.String()
	return page, nil
}

func (h *Handler) handleList(ctx context.Context, call *readCall) {
	w, req := call.w, call.req
	if !h.authorizeRead(w, req) {
		return
	}
	reqPath := req.Path
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
