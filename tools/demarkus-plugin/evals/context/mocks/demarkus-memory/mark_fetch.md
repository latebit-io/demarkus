# Context: /cwd/

## Hub (/cwd/index.md)

# Cwd

- [Journal](journal/): dated session notes
- [Roadmap](roadmap.md): what is next

## Journal 2026-10-11 (/cwd/journal/2026-10-11.md)

# Cwd journal: 2026-10-11

Finished the parser rewrite: the recursive descent parser replaces the table driven one, every fixture in testdata passes, and the error messages now carry a line and column. The old parser still backs the CLI. Next: wire the new parser into the CLI behind the existing flags, run the fixtures through the CLI, then delete the old parser and its tests.

## Active work (/cwd/roadmap.md)

# Roadmap

- Next: CLI on the new parser, then remove the legacy path.
