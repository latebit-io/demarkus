package writepolicy

import (
	"context"
	"maps"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/contentcheck"
	"github.com/latebit-io/demarkus/server/internal/backend"
)

// classify stamps req with the classifier's verdict, returned as a note. A
// refused body or one with a secret is never sent; an append stamps only
// injection, never weakening its base's verdict.
func (e *enforcer) classify(ctx context.Context, req *backend.WriteRequest, appending bool) []string {
	if e.options.Classify == nil {
		return nil
	}
	body := string(req.Content)
	findings := e.content.scanner.Scan(body)
	if _, refused := e.content.verdict(findings, req.Metadata); refused != nil || contentcheck.Secrets(findings) {
		return nil
	}
	stamp := e.options.Classify(ctx, req.Path, body)
	if stamp == "" || appending && !protocol.IsInjectionStamp(stamp) {
		return nil
	}
	meta := maps.Clone(req.Metadata)
	if meta == nil {
		meta = map[string]string{}
	}
	meta[protocol.ClassifierKey] = stamp
	req.Metadata = meta
	return []string{protocol.ClassifierKey + ": " + stamp}
}
