---
layout: default
title: The Library
permalink: /library/
---

# The library

`demarkus-library` is the web front-end for a demarkus universe: a
server-rendered reading room over one world or a whole knowledge system. Humans
read here; agents read the same documents over MCP.

- Source: [latebit-io/demarkus-library](https://github.com/latebit-io/demarkus-library)
- Ships with the [five-minute appliance](/install/stack/) and the [knowledge system deployment](/scenarios/knowledge-system/)

## The reading room

**Trail canvas.** Reading columns from root to focus, each pane scrolling on its
own, with the margin — trust signals, backlinks, properties — summoned per pane.
The trail URL serializes the whole reading context, so a paste drops someone into
your reading rather than on a bare document.

**Documents.** Goldmark-rendered, bluemonday-sanitized markdown: syntax
highlighting, alert callouts, lazy mermaid and KaTeX islands. Tag pages, search,
edition history, and raw source are routes.

**Hover cards.** In-app links load a small server fragment on hover — title,
status, opening line.

**The floor.** Universe view over the hub's published topology: every world an
entrance, plus a per-world map and link-graph canvas.

**Cataloging desk.** Create, edit, and append with live preview from the same
render pipeline. Writes are version-guarded: a stale save returns as a merge
candidate, never a silent overwrite.

**Federation.** `mark://` links outside the home world resolve as anonymous,
tokenless QUIC reads. Your read token never leaves your host.

## The librarian

An agent working the same read-only ports the room uses — fetch, browse, history,
lookup — answering as a streamed pane, with your current trail as context. It
can't see documents you can't: access is enforced at the world, not in the
prompt.

Feature-dark until you give it a provider. Without one the pane reads "not on
duty". On the appliance, pass `--librarian-key-file` at install, or add
`LLM_API_KEY` to `/etc/demarkus-library/env` later.

## Two transports

`DEMARKUS_TRANSPORT` picks the outbound adapter. Core and handlers are identical
either way.

| Mode | Behavior |
|---|---|
| `quic` (default) | One world, read directly over QUIC. No login. |
| `broker` | Reads go through the broker's MCP gateway with the reader's bearer. Room sits behind the org login. |

In broker mode the library is a registered confidential web client: auth code
with PKCE, tokens server-side, opaque session cookie, CSRF on state-changing
requests. No token reaches the browser.

## Run it

```bash
git clone https://github.com/latebit-io/demarkus-library
cd demarkus-library
go run ./cmd/demarkus-library
# open http://localhost:8080
```

Defaults to `quic` against the public [soul](/soul/), so a clone and one command
gives you a real room to browse.

| Var | Default | Meaning |
|---|---|---|
| `PORT` | `8080` | Listen port |
| `DEMARKUS_TRANSPORT` | `quic` | `quic` (direct world) or `broker` (gateway + login) |
| `DEMARKUS_HOST` | `soul.demarkus.io` | Home world in `quic` mode |
| `DEMARKUS_HUB` | home world | World publishing the topology behind the floor |
| `DEMARKUS_FEDERATION` | `true` | Follow `mark://` links to external hosts |
| `LLM_API_KEY` | _(empty)_ | Enables the librarian |
| `DEMARKUS_BRAND` / `DEMARKUS_LOGO` | _(empty)_ | Name and logo in nav, titles, login card |
| `DEMARKUS_THEME_CSS` | _(empty)_ | Override stylesheet, loaded last |

Every color and font routes through CSS custom properties on `:root`. A theme
file overriding only those tokens rebrands the room, light and dark.

## How it's built

Server-rendered Go (Echo v5, `html/template`, htmx) in a hexagonal
ports-and-adapters layout — the core knows nothing of Echo, QUIC, or goldmark.
SSR-first, htmx-hard, no JSON tier and no client-side state. Assets are vendored
and served from the binary: no CDN, no Node build, one Go binary.

## Where it runs

The [five-minute appliance](/install/stack/) installs it in broker mode behind
the stack's own login. Run it locally in `quic` mode to see the room without any
identity layer at all.
