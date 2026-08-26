# ADR 0001: Broker confidential web-client registry

Status: accepted (2026-06-11)

## Context

The broker's `/oauth/authorize` endpoint was loopback-only (RFC 8252 §7.3),
built for native/CLI agents whose MCP SDKs spin up a localhost listener. A
deployed web application (the Universe Library reading room) has no loopback,
so the authorization-code redirect flow was a dead end for server-side apps.
The device flow works without a redirect but gives web users the TV-login
experience (tab hop, code entry, polling); rejected as the primary web UX.

Every client was public/PKCE: `/oauth/authorize` treated `client_id` as
opaque, `/register` (RFC 7591 DCR) pins `token_endpoint_auth_method=none`,
and the token endpoint never checked a client secret.

The full decision trail lives in the demarkus-soul document
`/demarkus-library/adr/0004-broker-web-sso.md`; this ADR records the
broker-side mechanics.

## Decision

Add a registered **confidential web client** class alongside the existing
native path, as an explicit operator-curated registry, not open DCR.

- **Registry**: `webClients` in broker config: `clientID`, `clientSecretHash`
  (sha256-hex of the secret; plaintext never in config), `redirectURIs`
  (exact-match https allowlist), optional `name`. Validated at load: unique
  ids, 64-hex-char hash, ≥1 redirect, each redirect absolute https with no
  userinfo/fragment and no loopback host.
- **`/oauth/authorize`**: a registered `client_id` validates `redirect_uri`
  by exact match against its allowlist (no loopback exemption); an
  unregistered `client_id` keeps the loopback-only public path unchanged.
- **Token endpoint (`/device/token`)**: the `authorization_code` grant
  requires client authentication (HTTP Basic per RFC 6749 §2.3.1, or
  `client_secret_post`) when the presented `client_id` is registered. The
  secret is verified constant-time against the stored hash *before* the code
  is redeemed, so a failed authentication does not burn the code. Failure →
  `401 invalid_client` + `WWW-Authenticate: Basic`.
- **Refresh binding**: refresh tokens minted through a confidential exchange
  record the `clientID`; the `refresh_token` grant then requires the same
  client to authenticate. A leaked bound refresh token alone mints nothing.
  Tokens from the device flow and the loopback auth-code path stay unbound
  and refresh as before.
- **Discovery**: `token_endpoint_auth_methods_supported` advertises
  `["none","client_secret_basic","client_secret_post"]`.

## Notable choices

- **No `Confidential` flag in the auth-code store.** The plan called for
  stamping pending entries; it is redundant. `Redeem` already binds
  `client_id` constant-time and the registry is immutable for the process
  lifetime, so "entry's client_id is registered" is exactly "entry was issued
  confidential". The token handler's registry lookup of the presented
  client_id is therefore authoritative, and doing the secret check before
  `Redeem` preserves the store's keep-code-on-client-error retry semantics.
- **sha256, not bcrypt, for the secret hash.** The secret is
  operator-generated high-entropy randomness, not a human password; there is
  no low-entropy input for a fast hash to endanger, and it avoids a new
  direct dependency.
- **No per-client `scopes` field.** Scopes are declared-not-enforced
  broker-wide today; a per-client list would be dead config. Add it when
  scope enforcement exists.
- **PKCE stays required for confidential clients**: defense in depth; the
  authorize handler enforces S256 for both client classes.

## Consequences

- Native/CLI clients (Claude Code MCP SDK, demarkus-join) are untouched: no
  registry entry → identical behavior on every endpoint.
- The Universe Library (and future first-party web apps) can run standard
  redirect SSO against the broker with a deploy-time client registration.
- Operators must generate the client secret out of band and put only its
  sha256 into config; the chart needs a values surface for `webClients`
  (follow-up, not in this change).
- A client deregistered from config invalidates its bound refresh tokens at
  the refresh gate until re-registered or users re-authenticate.
