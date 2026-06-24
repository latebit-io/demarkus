// Shared ~/.demarkus paths for the demarkus-knowledge pi extension.
//
// This plugin owns NO server and NO binaries — a knowledge system is reached
// over HTTPS via pi-mcp-adapter's OAuth. It writes only its own files here (the
// registry + per-slug policy mirrors) and READS the soul's config file (written
// by demarkus-memory) purely to decide whether to surface the soul↔system note.

import { homedir } from "node:os";
import { join } from "node:path";

export const PLUGIN_HOME = join(homedir(), ".demarkus");

// Registry of joined knowledge-system MCP slugs (one per line), by /knowledge-join.
export const PLUGIN_KNOWLEDGE_REGISTRY = join(PLUGIN_HOME, "knowledge-systems");

// Per-slug publish-gate knobs, mirrored from each system's policy.md
// (PLUGIN_*_FILE + "." + slug). The gate runs offline, so the enforceable knobs
// travel as local files.
export const PLUGIN_STRICTNESS_FILE = join(PLUGIN_HOME, "plugin-knowledge.strictness");
export const PLUGIN_REQUIRE_TAGS_FILE = join(PLUGIN_HOME, "plugin-knowledge.require-tags");
export const PLUGIN_REQUIRE_FIELDS_FILE = join(PLUGIN_HOME, "plugin-knowledge.require-fields");

// Written by demarkus-memory when a local soul is configured (read-only here).
export const SOUL_CONFIG = join(PLUGIN_HOME, "plugin-memory.conf");

// One-time "no system joined yet" pointer sentinel.
export const HINT_SENTINEL = join(PLUGIN_HOME, ".knowledge-join-hint-shown");
