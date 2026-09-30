package protocol

import (
	"context"
	"errors"
	"path"
	"strings"
)

// Grant is publish capability a caller in the server's own process has
// verified for a writer: it rides the request context in place of a token
// and never crosses the wire. A stream from the network carries none.
type Grant struct {
	// Label names the writer in the audit log, as a token's label would.
	Label string
	// Paths is the glob scope, in the token file's pattern language.
	Paths []string
}

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
