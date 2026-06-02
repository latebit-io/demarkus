# demarkus-loadtest — capacity report

This tool exists to answer a concrete question: **how much traffic can a demarkus
server handle on a small box (target: 2 GB RAM, 1 vCPU)?** Rather than guess, it
measures. This report records both the reasoning and the first measured run.

## How to run it

```bash
make tools   # builds tools/bin/demarkus-loadtest

# Warm regime — N reused connections, requests multiplexed as QUIC streams:
demarkus-loadtest -url mark://HOST:6309/index.md -c 64 -d 30s -insecure

# Cold regime — a fresh connection (full TLS 1.3 handshake) per request:
demarkus-loadtest -url mark://HOST:6309/index.md -c 64 -d 30s -insecure -fresh

# Other read verbs:
demarkus-loadtest -url mark://HOST/ -verb lookup -query architecture -c 16 -d 10s -insecure
```

Flags: `-url -c -d -n -fresh -insecure -token -verb (fetch|list|versions|lookup)
-query -timeout`. The tool only issues read verbs, so a run never mutates the
store, and it disables caching so every request hits the wire. It reports
throughput, a status breakdown, and latency min/mean/p50/p90/p99/max.

## The bottleneck: CPU on QUIC, not RAM or disk

demarkus is a QUIC server serving markdown off a versioned file store. On a
1-vCPU box the binding constraint is almost never RAM or disk — it's CPU spent
on QUIC (userspace UDP + TLS 1.3 crypto). quic-go does packet framing,
congestion control, and AEAD encryption in userspace on the one core, so for
small markdown payloads that work dominates everything. Per-request server work
is otherwise tiny: `filepath.Clean` + `..` check, an auth-map lookup, a
page-cached file read, and a SHA-256 of the body (sub-millisecond for small
docs).

Two regimes matter, and they differ by an order of magnitude:

- **Reused connections (warm):** clients hold a QUIC connection and stream many
  requests. Limiter is encrypt + UDP per request.
- **Fresh connection per request (cold):** every request pays a TLS 1.3
  handshake — single-digit ms of server CPU each — capping throughput far below
  the warm ceiling.

How clients connect matters more than the VM spec.

## Measured run (2026-06-02)

Server pinned to a single core (`GOMAXPROCS=1`) over loopback, serving a
versioned store seeded with a ~600-byte `index.md`.

| Regime | Throughput | p50 | p99 | Notes |
|---|---|---|---|---|
| Warm (reused conns, 32 streams, rate-limit off) | **~7,700 FETCH req/s** | 4.2 ms | 5.3 ms | 0 errors |
| Cold (`-fresh`, rate-limit off) | **~1,390 req/s** | 14 ms | 21 ms | ~5.5× slower |
| Default config (rate-limit ON), single IP | **~41 req/s** | 0.8 ms | 5 ms | burst drained, then steady 50/s |

The 5.5× warm-vs-cold gap confirms the TLS handshake is the dominant server-side
CPU cost; connection reuse is the single biggest lever.

## The per-IP rate limiter — read this before quoting a number

The server ships a per-IP token-bucket limiter (`server/internal/ratelimit`):
**default 50 req/s, burst 100**, configurable via `DEMARKUS_RATE_LIMIT`
(`0` disables) and `DEMARKUS_RATE_BURST`. `Allow()` is non-blocking — over-limit
requests are *rejected* (they parse as an empty status), not queued.

So out of the box **a single client IP is capped at ~50 req/s regardless of core
count.** It is a per-abuser guard, not a global cap — aggregate across many
client IPs scales with CPU up to the warm/cold ceilings above. Any capacity
claim must state whether the limiter is on and that it is per-IP.

## Bottom line for a 2 GB / 1 vCPU VM

- **Per client IP, out of the box:** ~50 req/s (anti-abuse guard).
- **Aggregate across many IPs, or limiter off:** ~7,700 req/s warm, ~1,400 req/s
  cold (no connection reuse).
- **RAM (2 GB):** comfortable. The in-memory catalog + hash index is one small
  entry per current doc — hundreds of thousands of docs fit easily. The real
  watch-item is concurrent QUIC connection buffers (~hundreds of KB to ~1 MB
  each), so ~1–2k *concurrent connections* before memory pressure, not request
  rate. Bodies are capped at 1 MiB (`protocol.MaxBodyLength`).
- **Disk:** only relevant to writes (versioned, possibly fsync'd) — hundreds to
  low-thousands/s, and writes serialize under a lock. Don't size for high write
  QPS; a knowledge server's writes are rare.

For a personal or team knowledge server this is massively over-provisioned —
the core sits mostly idle. If you're sizing for something public-facing or
agent-swarm-heavy, the levers are connection reuse and the rate-limit config,
not a bigger box.

## Caveats on these numbers

Loopback (no real network RTT or NIC limits), client and server on the same
physical machine competing for the other cores, and a small ~600-byte document.
For numbers you can fully trust, run the tool **on the actual VM** with the
client on a separate host. The warm/cold ratio and the rate-limiter behavior
will hold regardless.
