package writepolicy

import (
	"context"
	"maps"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/contentcheck"
	"github.com/latebit-io/demarkus/server/internal/backend"
)

// classify stamps req with the classifier's verdict, returned as a note. A
// refused body or one with a secret is never sent; an append never replaces
// its base's injection verdict with a weaker one.
func (e *Enforcer) classify(ctx context.Context, req *backend.WriteRequest, appending bool) []string {
	if e.options.Classify == nil {
		return nil
	}
	body := string(req.Content)
	findings := e.content.scanner.Scan(body)
	if _, refused := e.content.verdict(findings, req.Metadata); refused != nil || contentcheck.Secrets(findings) {
		return nil
	}
	stamp := e.options.Classify(ctx, req.Path, body)
	if stamp == "" {
		return nil
	}
	note := protocol.ClassifierKey + ": " + stamp
	if appending && !protocol.IsInjectionStamp(stamp) {
		flagged, err := e.baseFlagged(ctx, req.Path)
		switch {
		case err != nil:
			return []string{note + " (not stamped: base verdict unread: " + err.Error() + ")"}
		case flagged:
			return nil
		}
	}
	meta := maps.Clone(req.Metadata)
	if meta == nil {
		meta = map[string]string{}
	}
	meta[protocol.ClassifierKey] = stamp
	req.Metadata = meta
	return []string{note}
}

// baseFlagged reports whether the document's current version carries an
// injection verdict, read the way Read reads the policy.
func (e *Enforcer) baseFlagged(ctx context.Context, path string) (bool, error) {
	doc, found, err := currentDocument(ctx, currentReader{e.current}, path)
	if err != nil || !found {
		return false, err
	}
	return protocol.ClassifierFlagged(doc.Metadata), nil
}
