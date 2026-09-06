# ADR 0011: Allowlisted private-use scheme redirects for MCP hosts

Status: proposed (2026-09-06).

## Context

The broker's RFC 7591 registration endpoint accepts two redirect shapes: http
loopback (RFC 8252 section 7.3) and absolute https (the web-client shape from
ADR 0001). Claude Code registers a loopback callback and works. Cursor
registers `cursor://anysphere.cursor-mcp/oauth/callback`, a private-use URI
scheme (RFC 8252 section 7.1), and `parseRedirectURIs` fails the whole
registration with `invalid_redirect_uri`. Cursor retries the same registration
on every Connect and never reaches the browser.

Dropping unsupported entries instead of failing does not help: Cursor sends the
custom-scheme callback and uses it at authorize time, so a registration without
it fails one step later at the redirect trust check.

## Decision

`validateClientRedirectURI` accepts a third shape: an exact match against a
package-level allowlist of known private-use scheme callbacks. The allowlist
ships with the Cursor entry. Matching is byte-for-byte on the full URI, never
on the scheme alone, so an unknown host cannot register `cursor://evil/...`.

The authorize leg is unchanged. It already trusts whatever redirect the DCR
record holds for the client id, so admitting the URI at registration is the
whole change.

No config knob. A second host that needs its own callback adds a line to the
allowlist and a test. A knob nobody turns off is not worth its surface.

## Consequences

- Cursor completes the MCP OAuth flow against the broker without a local
  proxy.
- Custom-scheme callbacks can be claimed by another app on the same OS. RFC
  8252 accepts this for public clients because PKCE binds the code to the
  verifier the real client holds. The broker already requires PKCE S256 on the
  authorization code grant, and the allowlist keeps the exposure to callbacks
  we chose to trust.
- Adding a host is a code change and a release, not an operator setting.
