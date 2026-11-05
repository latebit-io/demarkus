# Context: /cwd/

## Hub (/cwd/index.md)

<doc-0de55699de19db8a>
# Cwd

- [Journal](journal/): dated session notes
- [Roadmap](roadmap.md): what is next
</doc-0de55699de19db8a>

## Journal 2026-10-11 (/cwd/journal/2026-10-11.md)

<doc-94692675718ff7f8>
# Cwd journal: 2026-10-11

Finished the parser rewrite: the recursive descent parser replaces the table driven one, every fixture in testdata passes, and the error messages now carry a line and column. The old parser still backs the CLI. Next: wire the new parser into the CLI behind the existing flags, run the fixtures through the CLI, then delete the old parser and its tests.
</doc-94692675718ff7f8>

## Active work (/cwd/roadmap.md)

<doc-cba0d6bf59a3d8d0>
# Roadmap

What is next for cwd, one bullet per item.

- Next: CLI on the new parser, then remove the legacy path.
</doc-cba0d6bf59a3d8d0>
