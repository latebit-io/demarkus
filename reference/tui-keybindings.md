---
layout: default
title: TUI Keybindings
permalink: /reference/tui-keybindings/
---

# TUI Keybindings

Full keyboard reference for `demarkus-tui`.

## Navigation

| Key | Action |
|-----|--------|
| `Tab` | Cycle through links on the current page |
| `Enter` | Follow selected link / fetch URL in address bar |
| `[` or `Alt+Left` | Go back |
| `]` or `Alt+Right` | Go forward |
| `f` | Focus address bar |
| `Esc` | Blur address bar / exit bookmarks / dismiss help |

## Scrolling

| Key | Action |
|-----|--------|
| `j` or `Down` | Scroll down |
| `k` or `Up` | Scroll up |
| `g` | Go to top |
| `G` | Go to bottom |

## Bookmarks

| Key | Action |
|-----|--------|
| `b` | Toggle bookmark for current page |
| `B` | View all bookmarks |

## Graph View

Press `d` to open the document graph — a tree of all linked documents reachable from the current URL. The graph is **persistent**: crawl results are stored at `~/.mark/graph.json` and accumulate across sessions. When you open the graph view, the stored graph loads instantly while a live crawl runs in the background to discover new links.

The CLI command `demarkus graph` also persists to the same store.

| Key | Action |
|-----|--------|
| `d` | Open graph view |
| `j` or `Down` | Select next node |
| `k` or `Up` | Select previous node |
| `Enter` | Navigate to selected node |
| `d` or `Esc` | Exit graph view, return to document |

## General

| Key | Action |
|-----|--------|
| `?` | Toggle help screen |
| `q` or `Ctrl+C` | Quit |

## Address Bar

When the address bar is focused (`f` to activate):

| Key | Action |
|-----|--------|
| `Enter` | Fetch the URL |
| `Esc` or `Tab` | Return focus to document |
