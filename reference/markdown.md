---
layout: default
title: Supported Markdown Features
permalink: /reference/markdown/
---

# Supported Markdown Features

What you can rely on when authoring documents for Demarkus.

## The short version

Demarkus does not parse or validate the markdown you write — the server treats your document body as an opaque blob and defers all rendering to the client. (On disk, the server prepends its own YAML frontmatter for `version`, `previous-hash`, `archived`, and any publisher `meta.*` keys, but that's stripped before the body is returned to clients.) What you see in the TUI depends entirely on the renderer. `demarkus-tui` uses [Glamour](https://github.com/charmbracelet/glamour) (v2), which is built on [goldmark](https://github.com/yuin/goldmark) and enables **CommonMark + GitHub Flavored Markdown + definition lists** by default.

Everything on this page is what Glamour renders. Other clients (plain CLI, MCP, Obsidian) hand back raw markdown unchanged — so the consumer of that markdown decides what features it understands.

## CommonMark

Everything in the [CommonMark spec](https://commonmark.org/) works:

- Headings (`#`, `##`, …, or Setext-style with `===` / `---`)
- Paragraphs, hard and soft line breaks
- Emphasis: `*italic*`, `**bold**`, `***both***`
- Inline code: `` `code` ``
- Fenced and indented code blocks (with optional language hint for syntax highlighting)
- Block quotes (`>`)
- Ordered and unordered lists, nested
- Inline links: `[text](url)` and `[text](url "title")`
- Inline images: `![alt](url)`
- Autolinks: `<https://example.com>`, `<user@example.com>`
- Horizontal rules (`---`, `***`, `___`)
- Reference-style links: `[text][label]` with `[label]: url` elsewhere in the document

## GitHub Flavored Markdown (GFM)

- **Tables** — pipe-delimited with a separator row:

  ```md
  | Column | Value |
  |--------|-------|
  | a      | 1     |
  ```

- **Strikethrough** — `~~text~~`
- **Task lists** — `- [ ]` and `- [x]`
- **Linkify / bare URLs** — `https://example.com` is rendered as a clickable link without needing angle brackets

## Definition lists

```md
Term
: Definition for the term
: A second definition
```

## Metadata

Demarkus separates *document content* (what you author) from *metadata* (attributes about the document). Metadata does not belong in the body.

### Server-managed frontmatter

The server persists each version with its own YAML frontmatter block prepended to the body:

```yaml
---
version: 3
archived: false
previous-hash: sha256-abc123…
meta.author: Fritz
meta.tags: architecture,notes
---
<your body here>
```

Reserved keys: `version`, `previous-hash`, `archived`, plus any `meta.*` keys supplied by the publisher. The handler strips this block before returning the body to clients, so you never see it when you FETCH.

### Publisher metadata

To attach structured metadata to a document, pass it as request metadata on `PUBLISH` — **not** by writing YAML inside the body. The server records it under `meta.*` in the on-disk frontmatter and surfaces it in response headers on FETCH.

Limits: up to 10 keys totaling 512 bytes.

### Response fields (computed, not stored)

On FETCH, the server returns `modified`, `etag`, and `content-hash` as protocol metadata. These are derived from the version file at read time and are not stored in frontmatter.

### If you write `---` at the top of your body

It's treated as body content — the server does not parse it and does not strip it. Glamour will render `---` as a horizontal rule, so an in-body frontmatter block shows up as two horizontal rules with text between them. Use publisher metadata instead.

## What is not supported

The TUI renderer does **not** support these, even though you'll see them in other markdown ecosystems:

- **Footnotes** (`[^1]`) — not enabled in the default Glamour parser
- **Math / LaTeX** (`$…$`, `$$…$$`) — no MathJax or KaTeX equivalent
- **Diagrams** — no Mermaid, PlantUML, or embedded SVG rendering
- **Wiki-style links** (`[[foo]]`) — Obsidian syntax; the Obsidian plugin writes them as standard `[text](url)` before publishing
- **Embeds / transclusion** — no `![[foo]]` or similar
- **Custom containers** (`::: note`, `:::warning`) — no admonitions
- **Raw HTML** — CommonMark allows it; Glamour passes most tags through unstyled. Do not rely on it for rendering fidelity
- **Emoji shortcodes** (`:smile:`) — use the actual Unicode character instead

## Link graph

Documents are crawlable. The link extractor (used by `demarkus graph`, document-graph views in the TUI, and federation indexing) parses with the default CommonMark parser and walks `ast.Link` nodes. It recognizes:

- Inline links: `[text](url)`
- Reference-style links: `[text][label]` + `[label]: url`

It does **not** track:

- Autolinks inside angle brackets: `<https://example.com>` (a different AST node type)
- Bare URLs that Glamour linkifies at render time
- Wiki-style `[[links]]`

If you want a link to appear in the graph, use `[text](url)` or a reference link.

## Document size

- Body: up to 1 MiB (`MaxBodyLength = 1048576` bytes)
- Frontmatter: up to 1 KiB of protocol overhead on disk per version
- Publisher metadata keys: up to 10 keys, 512 bytes total

## Why so minimal?

Demarkus is a protocol for versioned markdown, not a documentation platform. The goal is that *any* markdown renderer can consume a demarkus document sensibly — the lowest common denominator (CommonMark + GFM) is well-understood everywhere. Features like Mermaid or math can be layered by specific clients without forcing every consumer to support them.

If a feature you need isn't here, open an issue — the renderer is swappable.
