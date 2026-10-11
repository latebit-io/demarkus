# Changelog

## 0.13.161

Recall guidance budgets by question shape: an answer in one section omits `budget` and fetches the section; an answer spanning documents or their relations passes `budget: 1500` on the first lookup and cites the appended blocks. The rule lives in the MCP server instructions; the skill and command defer to it.

## 0.13.158

mark_fetch and mark_explore return every metadata key on every call; the verbose argument is gone and no fetch instruction passes it.

## 0.13.153

First release: the Cowork port of `demarkus-memory`. MCP tools through the shared launcher, `/soul`, `/soul-context`, `/soul-journal`, `/soul-archive`, `/soul-curate`, and the `remember` skill; no hooks, no shell commands.
