// Package handler serves Mark Protocol requests from a content directory.
package handler

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/storefmt"
	"github.com/latebit-io/demarkus/server/internal/auth"
	storagebackend "github.com/latebit-io/demarkus/server/internal/backend"
	"github.com/latebit-io/demarkus/server/internal/fanout"
)

// MaxDirectoryEntries is the maximum number of entries returned by LIST.
const MaxDirectoryEntries = protocol.MaxListPageSize

// DocumentStore is the handler's view of a content store; storetest holds
// every backend to the same contract.
type DocumentStore = storagebackend.Store

// Config is what a Handler is built from. Store and Logger are required.
type Config struct {
	Store         DocumentStore
	GetTokenStore func() *auth.TokenStore // nil callback or nil return means writes are denied
	Logger        *slog.Logger
	ReadOnly      bool // reject all write operations
	// Watches serves WATCH; nil answers the verb bad-request, as a server
	// without it does.
	Watches *fanout.Fanout
	// RestrictGovernance makes governance documents (policy, layout, style,
	// world descriptor) writable only with the govern operation or grant.
	RestrictGovernance bool
	// LookupPrior is the world's layout prior for body LOOKUP ranking
	// (catalog.Options.Prior); nil ranks every directory alike.
	LookupPrior func(path string) float64
}

// Handler serves the Mark protocol over a DocumentStore. Build it with New.
type Handler struct {
	store         DocumentStore
	getTokenStore func() *auth.TokenStore
	logger        *slog.Logger
	readOnly      bool
	watches       *fanout.Fanout
	restrictGov   bool
	lookupPrior   func(path string) float64
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
		watches:       config.Watches,
		restrictGov:   config.RestrictGovernance,
		lookupPrior:   config.LookupPrior,
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
// bounds every store call the request makes; a WATCH runs until it ends.
func (h *Handler) HandleStream(ctx context.Context, stream Stream) {
	defer func() {
		if err := stream.Close(); err != nil {
			h.logger.Debug("close stream", "error", err)
		}
	}()

	req, ok := h.ReadRequest(stream)
	if !ok {
		return
	}
	h.Serve(ctx, stream, req)
}

// ReadRequest parses the request on stream and answers a malformed one
// itself; ok is false when nothing further is to be served.
func (h *Handler) ReadRequest(stream io.ReadWriter) (req protocol.Request, ok bool) {
	req, err := protocol.ParseRequest(stream)
	if err != nil {
		h.writeParseError(stream, err)
		return protocol.Request{}, false
	}
	return req, true
}

// Serve answers one parsed request on stream. ctx bounds every store call;
// for WATCH it bounds the stream's life.
func (h *Handler) Serve(ctx context.Context, stream io.Writer, req protocol.Request) {
	// Reject path traversal attempts before any handler logic (including auth)
	// to prevent scope bypass via paths like /allowed/../secret.md.
	if storefmt.ContainsDotDot(req.Path) {
		h.logger.Warn("path traversal attempt blocked", "path", sanitize(req.Path))
		h.writeError(stream, protocol.StatusNotFound, req.Path+" not found")
		return
	}
	if req.Verb == protocol.VerbWatch {
		// Not pinned to one token store: a watch outlives reloads and
		// rechecks every event against the current one.
		h.serveWatch(ctx, stream, req)
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
	// A reserved content address holds no document (ADR 0035): answered
	// without a view. LOOKUP's path is a scope, not a document.
	if req.Verb != protocol.VerbLookup && protocol.IsReservedHashPath(req.Path) {
		if h.authorizeRead(w, req) {
			h.writeError(w, protocol.StatusNotFound, req.Path+" not found")
		}
		return
	}
	// Written after the view closes: a peer that stops reading must not hold
	// the snapshot, which on a file store is the world's read lock.
	var response bytes.Buffer
	h.answerRead(ctx, &response, req)
	if _, err := response.WriteTo(w); err != nil {
		h.logger.Error("write response failed", "error", err)
	}
}

// answerRead writes the read's whole response to w under one view.
func (h *Handler) answerRead(ctx context.Context, w io.Writer, req protocol.Request) {
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
	if h.refuseHashPath(w, req.Path) {
		return
	}
	if int64(len(req.Body)) > protocol.MaxBodyLength {
		h.logger.Error("body too large", "path", sanitize(req.Path), "size_bytes", len(req.Body))
		h.writeError(w, protocol.StatusServerError, "content exceeds size limit")
		return
	}
	tokenLabel, ok := h.authorizeWrite(ctx, w, req)
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
		Body:     protocol.ErrorBody(status, message),
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
