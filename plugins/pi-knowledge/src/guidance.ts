// Session-start context assembly, ported from hooks/session-start.sh. When one
// or more knowledge systems are joined, name them and inject the standing
// guidance; when none are, emit a single one-time pointer to /knowledge-join,
// then stay quiet.

import { existsSync, readFileSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { listKnowledgeSystems, soulIsConfigured } from "./config.js";
import { HINT_SENTINEL } from "./paths.js";

const HERE = dirname(fileURLToPath(import.meta.url));
const GUIDANCE_FILE = join(HERE, "..", "context", "session-guidance.md");

function readGuidance(): string {
  try {
    return readFileSync(GUIDANCE_FILE, "utf8");
  } catch {
    return "";
  }
}

// buildSessionContext — the first-turn payload, "" when there is nothing to say.
export function buildSessionContext(): string {
  const systems = listKnowledgeSystems();

  if (systems.length === 0) {
    // Point at the join command exactly once, then stay silent.
    if (existsSync(HINT_SENTINEL)) return "";
    try {
      writeFileSync(HINT_SENTINEL, "");
    } catch {
      /* best-effort */
    }
    return (
      "demarkus-knowledge is installed but no knowledge system is joined yet. To connect this pi installation to " +
      "your organization's shared demarkus knowledge base, run `/knowledge-join <broker-url>`."
    );
  }

  const lines = [
    "# demarkus knowledge system(s) joined",
    "",
    "You are connected to the following organizational demarkus knowledge system(s), available as MCP servers (use `mark://<world>/<path>` URLs against them):",
    "",
    ...systems.map((slug) => `- **${slug}** (MCP server \`${slug}\`)`),
    "",
  ];
  const parts = [lines.join("\n"), readGuidance()];
  if (!soulIsConfigured()) {
    parts.push(
      "(No local demarkus soul is configured here, so the soul↔system guidance above is informational only — " +
        "everything durable goes to the knowledge system.)",
    );
  }
  return parts.filter((p) => p.trim().length > 0).join("\n\n");
}
