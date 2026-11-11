package handler

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/storefmt"
	storagebackend "github.com/latebit-io/demarkus/server/internal/backend"
)

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
	if err := storefmt.ValidateDocumentText(req.Path, req.Body); err != nil {
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
	pubMeta = stampUser(ctx, pubMeta)
	// The store contract stores a PUBLISH's metadata as given (storetest
	// conformance), so the request is typed here; APPEND is typed in the commit.
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

	doc, warnings, err := h.commit(ctx, call, storagebackend.WriteRequest{Path: req.Path, ExpectedVersion: expectedVersion, Content: []byte(req.Body), Metadata: pubMeta})
	h.logPrune("PUBLISH", req.Path, tokenLabel, doc)
	h.logUnknownPrune("PUBLISH", call, err)
	if err != nil {
		h.writePublishError(call, expectedVersion, doc, err)
		return
	}

	h.logger.Info("publish", "audit", true, "operation", "PUBLISH", "path", sanitize(req.Path), "version", doc.Version, "token_label", sanitize(tokenLabel), "success", true, "size_bytes", len(req.Body), "warnings", len(warnings))
	h.writeResponse(w, writtenResponse(doc, warnings))
}

// commit sends a PUBLISH or APPEND to the store. A store behind a write gate
// (backend.NotingStore) is told what the sender may read and answers with
// what the gate let through; a bare one has no notes.
func (h *Handler) commit(ctx context.Context, call *writeCall, write storagebackend.WriteRequest) (*storefmt.Document, []string, error) {
	appending := call.req.Verb == protocol.VerbAppend
	noting, gated := h.store.(storagebackend.NotingStore)
	switch {
	case gated && appending:
		return noting.AppendNoted(ctx, write, storagebackend.Sender{Readable: h.readableBy(call.req)})
	case gated:
		return noting.PublishNoted(ctx, write, storagebackend.Sender{Readable: h.readableBy(call.req)})
	case appending:
		doc, err := h.store.Append(ctx, write)
		return doc, nil, err
	}
	doc, err := h.store.Publish(ctx, write)
	return doc, nil, err
}

// writtenResponse is the created response for a landed write: the version
// it made, so a writer need not fetch to cite it; what the gate let through
// rides along under the reserved warning key.
func writtenResponse(doc *storefmt.Document, warnings []string) protocol.Response {
	resp := protocol.Response{Status: protocol.StatusCreated, Metadata: make(map[string]string, 5)}
	stampVersion(resp.Metadata, doc)
	if len(warnings) > 0 {
		resp.Metadata["warning"] = strings.Join(warnings, "; ")
	}
	return resp
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
func (h *Handler) writePublishError(call *writeCall, expectedVersion int, doc *storefmt.Document, err error) {
	w, req, tokenLabel := call.w, call.req, call.tokenLabel
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
	if err := storefmt.ValidateDocumentText(req.Path, req.Body); err != nil {
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
	pubMeta = stampUser(ctx, pubMeta)
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

	doc, warnings, err := h.commit(ctx, call, storagebackend.WriteRequest{Path: req.Path, ExpectedVersion: expectedVersion, Content: []byte(req.Body), Metadata: pubMeta})
	h.logPrune("APPEND", req.Path, tokenLabel, doc)
	h.logUnknownPrune("APPEND", call, err)
	if err != nil {
		h.writeAppendError(call, expectedVersion, doc, err)
		return
	}

	h.logger.Info("append", "audit", true, "operation", "APPEND", "path", sanitize(req.Path), "version", doc.Version, "token_label", sanitize(tokenLabel), "success", true, "size_bytes", len(req.Body), "warnings", len(warnings))
	h.writeResponse(w, writtenResponse(doc, warnings))
}

func (h *Handler) writeAppendError(call *writeCall, expectedVersion int, doc *storefmt.Document, err error) {
	w, req, tokenLabel := call.w, call.req, call.tokenLabel
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

// logUnknownPrune attributes a prune a write may yet make: its commit outcome
// is unknown, and a late commit's deletions must still name the writer.
func (h *Handler) logUnknownPrune(operation string, call *writeCall, err error) {
	keep := storefmt.RetentionValue(call.req.Metadata)
	if keep <= 0 || !errors.Is(err, storagebackend.ErrOutcomeUnknown) {
		return
	}
	h.logger.Warn("prune outcome unknown", "audit", true, "operation", operation, "path", sanitize(call.req.Path), "retention", keep, "token_label", sanitize(call.tokenLabel), "error", err)
}
