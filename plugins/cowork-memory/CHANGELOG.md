# Changelog

## 0.13.166

`/soul-doctor` and `/promote-scan` call the new `mark_doctor` tool instead of the plugin binary: the server reads the scope once and returns the hygiene report, so a bound broker store can be audited too, and `promotion: true` lists promotion candidates with the already-promoted set excluded. The style rules every publish is held to now run inside `mark_publish` on every surface; the Claude Code hook is unchanged.

## 0.13.161

Recall guidance budgets by question shape: an answer in one section omits `budget` and fetches the section; an answer spanning documents or their relations passes `budget: 1500` on the first lookup and cites the appended blocks. The rule lives in the MCP server instructions; the skill and command defer to it.

## 0.13.158

mark_fetch and mark_explore return every metadata key on every call; the verbose argument is gone and no fetch instruction passes it.

## 0.13.153

First release: the Cowork port of `demarkus-memory`. MCP tools through the shared launcher, `/soul`, `/soul-context`, `/soul-journal`, `/soul-archive`, `/soul-curate`, and the `remember` skill; no hooks, no shell commands.
