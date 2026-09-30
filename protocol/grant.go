package protocol

import (
	"context"
	"errors"
	"path"
	"strings"
	"time"
)

// Grant is what a caller in the server's own process verified for an
// identity: it rides the request context in place of a token and never
// crosses the wire, so no request from the network can put one there.
type Grant struct {
	// Label names the writer in the audit log, as a token's label would.
	Label string
	// Paths is the glob scope, in the token file's pattern language; none
	// grants no write.
	Paths []string
	// Expires is when the credential behind the grant lapses: a WATCH
	// served under it ends unauthorized then. Zero never lapses.
	Expires time.Time
}

// Gate admits one request a bearer listener read: the grant to serve it
// under, or a refusal (ErrNotPermitted, else unauthorized).
type Gate func(ctx context.Context, req Request) (Grant, error)

// ErrNotPermitted is a gate's refusal of a verified caller the world does
// not admit, answered not-permitted; any other refusal is unauthorized.
var ErrNotPermitted = errors.New("not permitted on this world")

type grantKey struct{}

// WithGrant attaches g to ctx for the request it serves.
func WithGrant(ctx context.Context, g Grant) context.Context {
	return context.WithValue(ctx, grantKey{}, g)
}

// GrantFrom returns the grant on ctx, if one was attached.
func GrantFrom(ctx context.Context) (Grant, bool) {
	g, ok := ctx.Value(grantKey{}).(Grant)
	return g, ok
}

// ValidatePathPattern checks a token or grant path pattern: path.Match
// syntax plus at most one ** wildcard, written /** (trailing) or /**/ (infix).
// Every producer of a pattern validates here so a typo fails at load.
func ValidatePathPattern(pattern string) error {
	if n := strings.Count(pattern, "**"); n > 1 {
		return errors.New("only one ** wildcard is supported per pattern")
	} else if n == 1 {
		stripped := strings.ReplaceAll(pattern, "/**/", "/")
		stripped = strings.TrimSuffix(stripped, "/**")
		if strings.Contains(stripped, "**") {
			return errors.New("** must be delimited by slashes (use /** or /**/)")
		}
	}
	clean := strings.ReplaceAll(pattern, "**", "placeholder")
	_, err := path.Match(clean, clean)
	return err
}
