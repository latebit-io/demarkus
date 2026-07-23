---
layout: default
title: Scenarios
permalink: /scenarios/
---

# Scenarios

One protocol at three scales. Store, verbs, version history, and MCP surface are
identical at every tier. Only reach and identity change.

## Personal: local memory

A server on your own machine. No account, no cloud, no network exposure. Writes
gated by a capability token.

- [Agent memory (the soul pattern)](/scenarios/agent-memory/): persistent memory for Claude Code and other MCP agents
- [Personal knowledge base](/scenarios/personal-wiki/): local wiki, full history, TUI browser

## Team: a shared world

One server a group reads, a [reading room](/library/) for humans, MCP for agents.
Capability tokens per person, or the appliance's built-in login.

- [Five-minute appliance](/install/stack/): world, broker, library, login, and HTTPS in one command
- [Team knowledge base](/scenarios/team/): the manual build with per-person tokens
- [Public hub](/scenarios/public-hub/): public server, Let's Encrypt, open reads

## Enterprise: a knowledge system

Many worlds behind one broker, your identity provider in front. Teams join with
one command; the broker mints per-world tokens. The [indexing
agent](/ecosystem/#indexing-agent) keeps the cross-world graph current.

- [Organizational knowledge system](/scenarios/knowledge-system/): architecture, reference deployment, how users join
- [The library and the librarian](/library/): human front-end over the universe

## They compose

A developer runs a personal soul and joins the org system in the same agent, in
Claude Code or [pi](https://pi.dev). Both plugin pairs share one `~/.demarkus`
state. `/promote` lifts a ready note from the private tier to the shared one
through a curation gate; `/soul-refresh` pulls the authoritative copy back as it
changes. The two plugins partition by server scope.

For the full agent-driven flow, how data lands in memory and gets promoted up to
the knowledge system, see [How knowledge flows](/how-it-works/).
