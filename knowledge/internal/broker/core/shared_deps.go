package core

import (
	"log/slog"
	"time"
)

// SharedDeps is what both listeners share. Run builds it once so the
// management API and the gateway verify with one composed verifier and
// one identity has one subject bucket across /me/install and /mcp.
type SharedDeps struct {
	Verifier       Verifier
	SubjectLimiter *RateLimitRegistry
	Clock          func() time.Time
	Log            *slog.Logger
}
