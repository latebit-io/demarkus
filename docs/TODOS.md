# TODO checklist

- ~~when starting the demarkus server, if the document directory does not exist throw an error and exit~~
- ~~recursive glob (`**`) in auth path matching — `/docs/**` now matches nested paths~~
- ~~auto-directory creation on publish — confirmed working, test added~~

## Known gaps

### Auth
- [x] Token expiration not enforced — `expires` field is loaded from TOML but `Authorize()` never checks it
- [x] No recursive glob (`**`) in path matching — `/docs/**` now matches nested paths
- [x] No replay protection — not needed: QUIC uses TLS 1.3 1-RTT handshakes (0-RTT is not enabled), so the transport layer prevents replay attacks
- [x] No rate limiting — per-IP token bucket rate limiter via `golang.org/x/time/rate`, configurable with `DEMARKUS_RATE_LIMIT` and `DEMARKUS_RATE_BURST`

### MCP
- [x] `mark_versions` tool missing from `demarkus-mcp` — added tool definition, handler, and tests

### CI/CD
- [ ] CI does not build TUI or MCP — only `demarkus` (CLI) is built, so build regressions in `demarkus-tui` and `demarkus-mcp` pass CI silently

### Testing
- [ ] No end-to-end tests — all tests use mock streams; no test exercises an actual QUIC connection

### Under review
- [ ] `SEARCH` verb — use case not yet clear enough to implement
