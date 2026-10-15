---
# The facts below restate ../mocks/demarkus-memory/mark_fetch.md; edit both.
type: llm
---
The primer this brief summarizes held only these facts. Roadmap: next, the CLI on the new parser, then remove the legacy path. Journal 2026-10-11: the parser rewrite is finished, a recursive descent parser replaces the table driven one, every testdata fixture passes, errors carry line and column, the old parser still backs the CLI; next, wire the new parser into the CLI behind the existing flags, run the fixtures through the CLI, then delete the old parser and its tests.

Check only the sentence under "What's in flight" and the list items (lines starting with "-" or a number) under "Recent journal" and "Pick up where you left off"; skip every other paragraph. Pass when the checked lines restate the facts above; other wording, order, or the stated purpose of a listed step is fine. Fail when a checked line names a plan, step or fact that is not among them.
