# Upgrade capture

`home` is the `~/.demarkus` and `~/.mark` a released `demarkus-plugin` wrote: provisioned, written through the released MCP tools, with registry rows added. `CAPTURE` records the date, the plugin version and the home the paths were written under. Tests read it with current code, so an upgrade never needs a user step:

- `server/cmd/demarkus-server/upgrade_test.go`
- `tools/demarkus-plugin/upgrade_test.go`
- `client/memory/graphstore/upgrade_test.go`

There is one capture, replaced in place. Support spans the current and previous release, so when a release changes one of these formats, run `bash scripts/capture-upgrade-fixture.sh <version>` with the release before the change; git history keeps the older captures. The script runs in Docker, so a soul on the host is never touched. The tokens inside are throwaway, minted in a container that no longer exists, and the `.gitignore` here keeps files the repository ignores elsewhere.
