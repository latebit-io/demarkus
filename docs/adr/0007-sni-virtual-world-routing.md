# ADR 0007 — SNI selects a virtual Mark server

Status: proposed (2026-08-21). Acceptance depends on the multi-world knowledge
server implementation and its routing tests.

## Context

A Mark request contains a verb, path, metadata, and body. It does not carry a
world selector. Adding one would change every server and client and would let a
single QUIC connection switch security domains between streams.

Enterprise deployments need one replicated process to host several logical
servers on one UDP listener. Existing direct clients already derive TLS SNI from
the hostname in a `mark://` URI, and an L4 load balancer can pass QUIC and TLS
through unchanged. SNI can therefore select the logical server before any Mark
request is accepted.

Four names that often happen to be equal are distinct:

- A broker world name identifies an entry in broker configuration.
- A Mark authority identifies the logical server in URLs, caches, graphs, and
  token stores.
- A dial address identifies the network endpoint used to open a socket.
- A TLS server name selects a virtual server and is verified by the client.

Conflating these names prevents several worlds from sharing one dial address and
repeats the identity-versus-transport problem resolved by ADR 0005.

## Decision

### Route once, during the TLS handshake

A multiplexed Mark listener maps exact TLS server names to world runtimes. The
mapping is immutable for the process lifetime.

The listener selects one runtime during the TLS handshake and pins the accepted
QUIC connection to it. Every stream on that connection uses the same handler,
store, token set, policy, rate limits, and logs. Mark requests gain no world
field, path prefix, token-derived selector, or alternate ALPN.

A multiplexed listener has no default world. Empty, unknown, wildcard,
trailing-dot, port-bearing, Unicode, and IP-literal server names fail the
handshake. A defensive post-handshake lookup repeats the selection before the
connection is served.

The standalone `demarkus-server` keeps its fixed-runtime behavior. It may accept
development certificates, absent SNI, and IP authorities because it does not
use SNI as an isolation boundary.

### Routing authorities are canonical DNS names

Configured routing authorities are lowercase ASCII DNS A-labels without a
trailing dot or port. Configuration rejects duplicate authorities after ASCII
case folding. Incoming DNS SNI is ASCII-case-folded before exact lookup; no
other rewriting is performed.

This normalization is limited to virtual-server routing. It does not change the
general graph identity rule in ADR 0005 or normalize arbitrary existing Mark
URLs.

Certificates must be valid for every configured TLS server name. Certificate
selection may use one multi-SAN certificate, a wildcard certificate, or
multiple certificates, but certificate breadth never broadens the routing map.
An SNI value covered by a certificate but absent from the map is still rejected.

TLS and QUIC terminate in the knowledge-server pod. Any load balancer in front
of it operates at layer 4 and must preserve the client handshake.

### Transport endpoints are explicit

The transport endpoint model is:

```text
Endpoint {
    Authority
    DialAddress
    ServerName
}
```

`Authority` is used for Mark URLs, graph identity, caches, and token lookup.
`DialAddress` is used only to open the network connection. `ServerName` is sent
as SNI and used for certificate verification. Connection pools are keyed by at
least `(DialAddress, ServerName)`.

The common deployment needs no explicit endpoint override: each world authority
has a DNS alias to the shared UDP Service, so all three values derive from that
authority. Brokers and agents may configure the three values separately where
DNS aliases are unavailable. A broker world name remains a local routing label
and is not substituted for the Mark authority.

## Consequences

- Existing Mark wire requests and all seven verbs remain unchanged.
- Direct clients continue using `mark://world.example/path`; DNS and SNI route
  the connection to the correct runtime.
- Unknown or absent SNI cannot reach a default runtime on a multiplexed
  listener.
- A connection cannot cross worlds between streams, even if a request token is
  valid in another world.
- Client connection pools and transport configuration must stop treating the
  dial address as logical identity when endpoint overrides are used.
- Every replica must load every configured world before becoming ready, because
  any Service endpoint may receive any world authority.

## Alternatives rejected

- **Add a world field to Mark requests.** This changes the permanent protocol
  core and allows one connection to cross isolation boundaries.
- **Route from the token.** Token parsing would become routing, duplicate tokens
  could select the wrong world, and public reads would have no selector.
- **Use a path prefix.** Paths are world-local document identities; rewriting
  them would leak deployment topology into protocol semantics.
- **Use one ALPN per world.** It creates unbounded protocol identifiers and
  requires custom client configuration for every world.
- **Choose a default world.** Missing or malformed SNI would become an isolation
  bypass rather than a handshake error.
