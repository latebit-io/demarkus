# TODO checklist

- ~~when starting the demarkus server, if the document directory does not exist throw an error and exit~~
- ~~recursive glob (`**`) in auth path matching — `/docs/**` now matches nested paths~~
- ~~auto-directory creation on publish — confirmed working, test added~~

## Known gaps

### Auth
- [x] Token expiration not enforced — `expires` field is loaded from TOML but `Authorize()` never checks it
- [x] No recursive glob (`**`) in path matching — `/docs/**` now matches nested paths
- [ ] No replay protection — auth tokens have no timestamp validation or nonce mechanism
- [ ] No rate limiting — public-facing servers have no request flood protection

### MCP
- [ ] `mark_versions` tool missing from `demarkus-mcp` — documented in README/CLAUDE.md but not implemented. Either add the tool or update docs to match actual tool set

### CI/CD
- [ ] CI does not build TUI or MCP — only `demarkus` (CLI) is built, so build regressions in `demarkus-tui` and `demarkus-mcp` pass CI silently

### Testing
- [ ] No end-to-end tests — all tests use mock streams; no test exercises an actual QUIC connection

### Under review
- [ ] `SEARCH` verb — use case not yet clear enough to implement
