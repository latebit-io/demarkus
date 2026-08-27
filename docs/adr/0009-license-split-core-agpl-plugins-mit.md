# ADR 0009: AGPL for the core, MIT for plugins and satellites

Status: accepted 2026-08-27

## Context

The monorepo licensed all implementation code AGPL-3.0-only. Ecosystem repos (demarkus-library, demarkus-knowledge-system-deploy, the pi plugin mirrors) shipped no license at all, and GitHub could not detect the monorepo's combined pointer file.

## Decision

- The core (protocol, server, knowledge server, client, tools) stays AGPL-3.0-only.
- Everything under `plugins/` is MIT (`plugins/LICENSE`); the pi mirror directories carry their own MIT copy so the subtree force-push ships it.
- demarkus-library is MIT.
- demarkus-knowledge-system-deploy is MIT (it is a template others copy).
- The protocol specification remains CC0-1.0 (`LICENSE-PROTOCOL`).

## Consequences

- Agent-harness integrations adopt plugins without AGPL obligations; network use of the servers still triggers AGPL source offers.
- Standalone plugin repos get GitHub-detectable full license texts.
- demarkus-hub and obsidian-demarkus still carry GPLv3; both are being phased out rather than relicensed.
