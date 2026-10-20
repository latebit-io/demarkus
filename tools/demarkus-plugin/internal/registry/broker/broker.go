// Package broker validates a demarkus broker endpoint (https plus RFC 9728
// metadata) and derives the MCP slug it registers under.
package broker

import (
	"context"
	"fmt"
	"net/http"
	neturl "net/url"
	"os"
	"strings"
	"time"

	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/registry/catalog"
)

// Endpoint is a validated demarkus broker MCP endpoint (knowledge
// or memory; the validation is product-agnostic).
type Endpoint struct {
	URL    string
	Slug   string
	McpURL string
}

// Validate validates a broker URL (https + RFC 9728
// metadata) and derives a slug; shared by /knowledge-join and the
// /soul-join broker path. Errors are user-presentable verbatim.
func Validate(rawURL string) (*Endpoint, error) {
	// Parse BEFORE any request: userinfo must never reach the wire (Go
	// turns it into a Basic Authorization header) or an error message.
	raw := strings.TrimRight(strings.TrimSpace(rawURL), "/")
	parsed, err := neturl.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("broker URL does not parse: %s", raw)
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	switch parsed.Scheme {
	case "https":
	case "http":
		if os.Getenv("DEMARKUS_KNOWLEDGE_JOIN_ALLOW_HTTP") != "1" {
			return nil, fmt.Errorf("broker URL must use https:// (got: %s)", raw)
		}
	default:
		return nil, fmt.Errorf("broker URL must start with https:// (got: %s)", raw)
	}
	// Userinfo has no meaning here (OAuth owns auth) and would leak into
	// the world-readable catalog and derive a bogus slug.
	if parsed.User != nil {
		return nil, fmt.Errorf("broker URL must not carry userinfo (user:password@); authentication is OAuth in the MCP client")
	}
	// Query/fragment would corrupt the appended discovery and /mcp
	// paths. "#" is checked on the raw string: Parse erases an empty
	// fragment marker ("...#") but its path survives untrimmed.
	if parsed.RawQuery != "" || parsed.ForceQuery || strings.Contains(raw, "#") {
		return nil, fmt.Errorf("broker URL must not carry a query or fragment (got: %s)", raw)
	}
	u := parsed.String()
	slug := catalog.DeriveSlug(parsed.Hostname())
	if slug == "" {
		return nil, fmt.Errorf("could not derive a usable MCP server slug from %s (host: %s)", u, parsed.Hostname())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := Reach(ctx, u); err != nil {
		return nil, err
	}
	return &Endpoint{URL: u, Slug: slug, McpURL: u + "/mcp"}, nil
}

// Reach confirms a broker answers at its public RFC 9728 metadata, the one
// request that needs no OAuth; the join and the session status both use it.
func Reach(ctx context.Context, u string) error {
	meta := u + "/.well-known/oauth-protected-resource"
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, meta, http.NoBody)
	if err != nil {
		return fmt.Errorf("broker URL does not parse: %s", u)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("broker unreachable at %s (DNS / TCP / TLS failure; check the URL and network)", meta)
	}
	_ = resp.Body.Close()
	switch resp.StatusCode {
	case 200:
		return nil
	case 401, 403:
		return fmt.Errorf("broker rejected metadata fetch with HTTP %d (the metadata endpoint should be public; confirm this is a Slice 7+ demarkus-knowledge-broker)", resp.StatusCode)
	case 404:
		return fmt.Errorf("broker has no /.well-known/oauth-protected-resource (HTTP 404); not a Slice 7+ demarkus-knowledge-broker, or the URL points at the management API instead of the MCP host")
	default:
		return fmt.Errorf("broker metadata fetch returned HTTP %d (expected 200)", resp.StatusCode)
	}
}
