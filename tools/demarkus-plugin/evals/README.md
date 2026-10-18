# Memory plugin eval suite

`make plugin-eval` runs `run.sh`: the shipped plugin is copied beside this
suite, prompts are regenerated, hooks run a working-tree `demarkus-plugin`, and
`claude plugin eval` judges each case. Paid, about a dollar a run; results land
in `results/` (ignored).

## Mocks are generated

The MCP server is mocked. `tools/evalmocks` renders the mocks from the code so
a case fails when the code lacks what the prompt expects, instead of passing
against a hand-written answer:

- `mocks/demarkus-memory/_tools.json` wraps the `demarkus-mcp` lean tools/list
  golden (`tools-host-lean.json`), the profile the plugin starts the server
  with.
- Every other `mocks/demarkus-memory/<tool>.md`, suite or case level, is a
  `marktools` call over a fixture store that answers with the server's wire
  goldens (`protocol/wiretest/testdata`). The runner's frontmatter and
  `{{input.*}}` placeholders are declared in the table.

Edit the table in `tools/evalmocks/mocks.go`, then `cd tools && go run
./evalmocks write`. `go run ./evalmocks check` runs in pre-commit and in the
CI tools job, the one that also watches `client/**` and `protocol/**`, and
fails on a stale mock or a mock file the table does not list. The doctor and
curate reports are still hand-written, listed in `handWritten`.
