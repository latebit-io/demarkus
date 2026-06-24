// Session-start context assembly, ported from hooks/session-start.sh's
// emit_context + memory_offer + server_health_warning. Produces the standing
// guidance block injected once per session, led (when due) by a health warning
// and the one-time "make demarkus your only memory" offer.

import { existsSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { loadConfig } from "./config.js";
import { PLUGIN_HOME } from "./paths.js";

const HERE = dirname(fileURLToPath(import.meta.url));
const GUIDANCE_FILE = join(HERE, "..", "context", "session-guidance.md");
const MEMORY_OFFER_SENTINEL = join(PLUGIN_HOME, ".memory-offer-shown");

const MEMORY_OFFER =
  "[One-time setup offer — raise this with the user once, then drop it] demarkus-memory is now wired as your " +
  "persistent memory store. If the user would like demarkus to be their SINGLE source of durable memory, offer to " +
  "disable your harness's built-in memory tool so the two don't compete: ask first, and only if they agree, turn it " +
  "off and report what you changed. If they decline, say nothing further about it. Do not disable anything unasked.";

// memoryOffer — the offer text the first time it's requested, "" thereafter.
// Best-effort sentinel; if it can't be written we risk re-offering, never a hard fail.
function memoryOffer(): string {
  if (existsSync(MEMORY_OFFER_SENTINEL)) return "";
  try {
    mkdirSync(PLUGIN_HOME, { recursive: true });
    writeFileSync(MEMORY_OFFER_SENTINEL, "");
  } catch {
    /* best-effort */
  }
  return MEMORY_OFFER;
}

// serverHealthWarning — a one-line warning when the managed server doesn't look
// alive, "" when healthy or indeterminate. Port of lib.sh server_health_warning
// for the managed (default/isolated) modes; reuse mode is left to /soul-status.
export function serverHealthWarning(): string {
  const cfg = loadConfig();
  if (!cfg) return "";
  if (cfg.mode !== "default" && cfg.mode !== "isolated") return "";
  const pidFile = join(cfg.soulDir, ".pid");
  let alive = false;
  try {
    const pid = Number(readFileSync(pidFile, "utf8").trim());
    if (Number.isInteger(pid) && pid > 0) {
      process.kill(pid, 0); // throws if no such process
      alive = true;
    }
  } catch {
    alive = false;
  }
  if (alive) return "";
  return (
    `the demarkus-memory server is not running (no live process for ${cfg.soulDir}). Memory tools ` +
    "(mark_fetch/mark_publish/mark_lookup/...) will fail until it restarts — run /soul-init to restart, or " +
    "/soul-status to diagnose."
  );
}

function readGuidance(): string {
  try {
    return readFileSync(GUIDANCE_FILE, "utf8");
  } catch {
    return "";
  }
}

// buildSessionContext — the full payload for the first turn of a session:
// optional health warning, optional one-time offer, then the standing guidance.
// Returns "" when there is nothing to say.
export function buildSessionContext(): string {
  const parts: string[] = [];
  const warning = serverHealthWarning();
  if (warning) parts.push(`⚠️ ${warning}`);
  const offer = memoryOffer();
  if (offer) parts.push(offer);
  const guidance = readGuidance();
  if (guidance) parts.push(guidance);
  return parts.join("\n\n");
}
