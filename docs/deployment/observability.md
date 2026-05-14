# Observability

demarkus emits structured logs to stdout/stderr. Each runtime image (server, broker, agent) writes one JSON record per event when `DEMARKUS_LOG_FORMAT=json` (the chart default in production deployments). Operators ingest those JSON records with whatever log shipper their stack already runs — Datadog Agent, OpenTelemetry Collector, Vector, Fluent Bit, Grafana Alloy, or anything else that can parse JSON from container stdout. The field schema below tells your log shipper what to extract; the rest is your backend's existing JSON-log documentation.

This document deliberately ships no pre-built collector configs. Every backend's config language drifts; an example `vector.yaml` written today is wrong six months later. The schema below stays stable because it's the actual code path, not a snapshot.

## Format and routing

All three services write to stdout/stderr via Go's `log/slog`:

| Service | Default format | Override |
|---|---|---|
| `demarkus-server` | `json` (chart default) | `server.logFormat` in values; `DEMARKUS_LOG_FORMAT` env var |
| `demarkus-broker` | `json` (hardcoded in binary) | not exposed; `tools/demarkus-broker/main.go:48` |
| `demarkus-agent` | `json` (chart default) | `logFormat` in values; `DEMARKUS_LOG_FORMAT` env var |

The binary's standalone default outside the chart is `text` for server and agent (human-readable for local dev). The Helm charts override to `json` so production deployments are machine-parseable without operator intervention.

Standard slog field shape:

```json
{
  "time": "2026-05-13T20:43:32.101Z",
  "level": "INFO",
  "msg": "request",
  "verb": "FETCH",
  "path": "/index.md"
}
```

`time`, `level`, `msg` are always present. Other fields are event-specific.

## Server (`demarkus-server`) — field schema

Server logs cover protocol request handling, startup, and TLS/auth lifecycle. The most operationally relevant events:

| msg | level | Key fields | What it means |
|---|---|---|---|
| `request` | INFO | `verb`, `path` | One per inbound protocol request. The base signal for request-rate dashboards. |
| `not found` | INFO | `path`, optional `version` | Client fetched a path that doesn't exist. High volume on misconfigured federation. |
| `unauthorized` | WARN | `operation`, `path` | Bearer token missing on a write op. Spikes indicate misconfigured clients. |
| `not permitted` | WARN | `operation`, `path` | Bearer token present but doesn't grant the requested op on the path. |
| `path traversal attempt blocked` | WARN | `path` | Defensive log — should never spike under legitimate traffic. Alert. |
| `archive` | INFO | `audit: true`, `operation: ARCHIVE`, `path`, `version`, `token_label`, `success` | Audit event for content deletion. The `audit: true` field is the marker for routing to a separate audit-log stream. |
| `chain verification failed` | WARN | `path`, `error` | Hash-chain integrity check failed for a versioned document. Should not occur in normal operation. |
| `body too large` | ERROR | `path`, `size_bytes` | Inbound write exceeded the body limit. |
| `hash index stale` | INFO | `hash`, `path` | Content-addressed lookup found a stale index entry. Low-frequency under normal write load. |
| `rate limited` | WARN | (none) | Request rejected by the rate limiter. |
| `server started` | INFO | `addr`, `root`, `idle_timeout`, `request_timeout` | Process startup. One per restart. |
| `auth: loaded tokens` / `auth: tokens reloaded` | INFO | `path` | Token file load on startup or SIGHUP. |
| `tls: certificate reloaded` | INFO | `path` | TLS cert hot-reload (SIGHUP). |
| `received signal, initiating graceful shutdown` | INFO | `signal` | Process shutdown begin. |
| `server stopped` | INFO | (none) | Process shutdown end. |

**Useful operator queries:**

- Request rate per verb: `count by verb where msg=request`
- 4xx-equivalent surface: `count where msg in (unauthorized, not permitted, body too large)`
- Audit trail: `where audit=true`
- Lifecycle: `where msg in (server started, server stopped, auth: tokens reloaded, tls: certificate reloaded)`

## Broker (`demarkus-broker`) — field schema

Broker logs cover OIDC mint/revoke/rotate, sweeper drift+expiry handling, and rate-limit decisions. All messages share the `broker:` prefix on `msg` so a single filter scopes a backend dashboard.

| msg | level | Key fields | What it means |
|---|---|---|---|
| `broker: config loaded` | INFO | (config keys) | Process startup. |
| `broker: listening` | INFO | `addr` | HTTP server bound. |
| `broker: rate limit enabled` / `disabled` | INFO | `tokens_per_min`, `tokens_burst`, `login_per_min`, `login_burst`, `trust_xff` | One per startup; documents the active rate-limit config. |
| `broker: rate limit exceeded` | WARN | `route`, `subject` or `ip`, `retryAfter` | A 429 response was returned. `subject` is set on `/tokens/*` routes (hashed); `ip` is set on `/auth/login`. |
| `broker: mint succeeded` | INFO | `subject` (hashed), `worlds` (count) | OIDC callback minted N tokens for the user across N worlds. The base signal for mint-rate dashboards. |
| `broker: mint failed` | ERROR | `err`, `subject` (hashed) | Hard mint failure; no tokens issued. |
| `broker: partial mint` | WARN | `err`, `subject` (hashed), `minted` (count) | Mint succeeded for some worlds but failed on others. Operator-investigable. |
| `broker: identity not authorized for any world` | INFO | `subject` (hashed) | User authenticated but predicate rejected all configured worlds. |
| `broker: rejected unverified identity` | INFO | `subject` (hashed) | OIDC `email_verified` claim was false. |
| `broker: revoke succeeded` | INFO | `label`, `subject` (hashed) | DELETE /tokens/:label call succeeded. |
| `broker: revoke owner mismatch` | WARN | `label`, `subject` (hashed) | DELETE call by a subject who doesn't own that label — 403 returned. Spikes indicate token leakage or buggy clients. |
| `broker: rotate succeeded` | INFO | `label` (old), `new_label`, `subject` (hashed) | POST /tokens/:label/rotate call. |
| `broker: rotate denied — caller no longer authorized for world` | INFO | `label`, `subject` (hashed), `world` | Re-authorization check failed at rotate time. The user lost access between mint and rotate. |
| `broker: swept` | INFO | `duration_ms`, `world`, `retired_expired`, `retired_drifted` | Periodic sweeper tick. The base signal for sweeper-health dashboards. |
| `broker: sweep failed` | ERROR | `err` | Sweeper iteration failed. |
| `broker: sweeper observing new leader` / `lost leadership` | INFO | `leader` / `identity` | Leader-election Lease transitions. Sub-second handoff on rolling restarts; if you see these spike, investigate replica churn. |
| `broker: state cookie invalid` / `state mismatch` | WARN | `err` or `want`/`got` | OIDC state validation failed at callback. Spikes indicate replay attempts or expired state cookies. |
| `broker: oauth exchange failed` | WARN | `err` | IdP token-exchange call failed. Driven by IdP availability. |
| `broker: id_token verification failed` | WARN | `err` | Bearer token verification failed on an authed route. |

**Subject hashing.** The broker NEVER logs raw `claims.Subject` or emails. The `subject` field is `hashSubject(claims.Subject)` — a deterministic non-reversible hash. Tracing a specific user across log events is intentionally cumbersome to align with the capability model (the server is identity-blind; the broker knows identity but doesn't publish it through logs). Audit trails for a specific user live in the issuances Secret, not in logs.

**Useful operator queries:**

- Mint rate: `count where msg='broker: mint succeeded'`
- Authorization-failure rate: `count where msg in ('broker: identity not authorized for any world', 'broker: rejected unverified identity')`
- Rate-limit pressure: `count by route where msg='broker: rate limit exceeded'`
- Sweeper health: `where msg='broker: swept'` — should be one per `sweeper.interval` (default 5m) per leader
- Leadership churn: `where msg in ('broker: sweeper observing new leader', 'broker: sweeper lost leadership')`

## Agent (`demarkus-agent`) — field schema

Agent logs cover crawl scheduling, federation results, and hub-publish outcomes.

| msg | level | Key fields | What it means |
|---|---|---|---|
| `daemon: starting` | INFO | `interval` | Process startup in daemon mode. |
| `daemon: stopping` | INFO | (none) | Signal-driven shutdown. |
| `crawl: starting` | INFO | `seeds` (count) | One per crawl iteration. |
| `crawl: complete` | INFO | `duration_ms`, `servers`, `documents`, `hashes`, `errors` | Per-crawl summary. The base signal for crawl-throughput dashboards. |
| `crawl: error` | WARN | `detail` | One per per-server error encountered during a crawl. Multiple per `crawl: complete` is normal. |
| `crawl: failed` | ERROR | `err` | The entire crawl iteration failed. |
| `publish: complete` | INFO | `indexes`, `hubs` | Hub-index publish after a successful crawl. |
| `publish: hub push failed` | WARN | `err` | Per-hub publish failure. |

**Useful operator queries:**

- Crawl throughput: `latest(documents) where msg='crawl: complete'`
- Crawl-error rate: `sum(errors) where msg='crawl: complete'` — divide by `count where msg='crawl: complete'` for errors-per-iteration
- Schedule health: `count where msg='crawl: complete'` — should be one per `schedule.interval`

## Wiring it into your backend

Every backend's JSON-log ingestion is the same shape: read container stdout, parse the JSON body, surface `time` / `level` / `msg` as core fields, and let the rest of the keys flow through as searchable attributes. Pointers:

- **Datadog**: enable Datadog Agent log collection (`logs_enabled: true` + `logs_config.container_collect_all: true`, or the Operator equivalents `features.logCollection.enabled` + `containerCollectAll`); JSON parsing is automatic for structured log bodies. Add a `service:demarkus-server` (or `-broker` / `-agent`) tag via Datadog autodiscovery pod annotations. See [Datadog Agent: Kubernetes log collection](https://docs.datadoghq.com/containers/kubernetes/log/).
- **OpenTelemetry Collector**: use the `filelog` receiver scoped to container log paths, the `json_parser` operator on the message body, and the `attributes` processor to promote `level` / `msg` to log-record attributes. See [OTel filelog receiver](https://github.com/open-telemetry/opentelemetry-collector-contrib/tree/main/receiver/filelogreceiver) and the [OTLP logs exporter](https://github.com/open-telemetry/opentelemetry-collector/tree/main/exporter/otlpexporter) for the route to your backend.
- **Vector**: `[sources.kubernetes_logs]` for ingestion, `[transforms.parse_json]` for the body, then your sink of choice. See [Vector kubernetes_logs source](https://vector.dev/docs/reference/configuration/sources/kubernetes_logs/).
- **Fluent Bit**: `tail` input on `/var/log/containers/*.log` with a JSON parser, optionally enriched by the `kubernetes` filter for pod-metadata attributes. See [Fluent Bit Tail input](https://docs.fluentbit.io/manual/pipeline/inputs/tail) and [Kubernetes filter](https://docs.fluentbit.io/manual/pipeline/filters/kubernetes).
- **Grafana Alloy / Loki**: `discovery.kubernetes` for target discovery, `loki.process` with a `json` stage. See [Grafana Alloy Loki components](https://grafana.com/docs/alloy/latest/reference/components/loki/).

Each backend's docs evolve; the demarkus field schema above is what stays stable.

## What's NOT in the logs (by design)

- **Raw OIDC subject identifiers or email addresses.** Broker uses `hashSubject()` and never logs `claims.Email` directly. Audit trails for "what did user X do" live in the issuances Secret, not in logs.
- **Token material.** Tokens are written to disk + Secrets; they never appear in log lines. The chart's `helm.sh/resource-policy: keep` on world tokens Secrets (§6.3.D.2) is the operator-side guarantee that token state survives chart upgrades.
- **Request/response bodies.** The server logs `path` + `verb`, not content. PUBLISH bodies are not logged (they would leak document content). The `archive` audit event records what was deleted via `path` + `version`, not the document body.
- **`/metrics` endpoint or OTel SDK calls.** demarkus does not embed a metrics or tracing client. Observability is intentionally log-derived so the deployment surface stays narrow — your log shipper is the only data path out. If you want time-series metrics, derive them in your backend from the log events above (Datadog log-based metrics, Vector's log-to-metric transform, Loki's `log_queries_total` counter, etc.).
