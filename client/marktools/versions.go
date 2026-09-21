package marktools

import (
	"context"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/mcpfmt"
)

// Versions answers mark_versions: a document's history and chain state.
func (t *Tools) Versions(ctx context.Context, rawURL string) Result {
	target, bad := t.resolve(ctx, rawURL)
	if bad != nil {
		return *bad
	}
	result, err := t.backend.Versions(ctx, fetch.VersionsRequest{
		Host: target.Host, Path: target.Path, Token: t.readToken(ctx, target.Host),
	})
	if err != nil {
		return t.failed(SiteVersions, target.Host, err)
	}
	return text(mcpfmt.Full(result, "total", "current", "chain-valid", "chain-error"))
}
