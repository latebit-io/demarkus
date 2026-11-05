package writepolicy

import (
	"context"
	"maps"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/contentcheck"
	"github.com/latebit-io/demarkus/server/internal/backend"
)

// secretScan keeps credentials off the wire to the classifier.
var secretScan = contentcheck.New(contentcheck.Options{PII: contentcheck.Off, Steering: contentcheck.Off})

// classify stamps req with the classifier's verdict and returns it as a note.
// A body with a secret is not sent: the gate refuses it unless the tier
// warns. An append stamps only injection, never weakening its base's verdict.
func (e *enforcer) classify(ctx context.Context, req *backend.WriteRequest, appending bool) []string {
	if e.options.Classify == nil || contentcheck.Secrets(secretScan.Scan(string(req.Content))) {
		return nil
	}
	stamp := e.options.Classify(ctx, req.Path, string(req.Content))
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
