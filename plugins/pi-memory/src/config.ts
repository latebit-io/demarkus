// TypeScript port of the read-only logic in scripts/lib.sh — config, registries,
// strictness, and MCP-tool-name scope resolution. The pi gates/nudges consult
// these; provisioning + registry mutation stays in the bundled bash.

import { existsSync, readFileSync } from "node:fs";
import {
  KNOWLEDGE_REGISTRY,
  LOCAL_SOUL_ID,
  PLUGIN_CONFIG,
  PLUGIN_DEST_STRICTNESS_FILE,
  PLUGIN_STRICTNESS_FILE,
  PROJECT_SOULS,
  PROMOTE_TARGETS,
  SOULS_REGISTRY,
} from "./paths.js";

export type Strictness = "warn" | "block" | "ask";

function readTrimmed(path: string): string {
  try {
    return readFileSync(path, "utf8").trim();
  } catch {
    return "";
  }
}

function readLines(path: string): string[] {
  try {
    return readFileSync(path, "utf8").split("\n");
  } catch {
    return [];
  }
}

// Non-blank, non-comment rows with surrounding whitespace trimmed.
function records(path: string): string[] {
  return readLines(path)
    .map((l) => l.trim())
    .filter((l) => l.length > 0 && !l.startsWith("#"));
}

// --- strictness ---------------------------------------------------------------

// configured_strictness: env DEMARKUS_MEMORY_STRICTNESS, then file, then warn.
// Any unrecognized value floors at warn so a typo can't silently escalate.
export function configuredStrictness(): Strictness {
  const s = (process.env.DEMARKUS_MEMORY_STRICTNESS || readTrimmed(PLUGIN_STRICTNESS_FILE)).replace(/\s/g, "");
  return s === "block" || s === "ask" || s === "warn" ? s : "warn";
}

// configured_dest_strictness: env, then file, then block (the intended default —
// an explicit binding means a misroute is an error, not a warning).
export function configuredDestStrictness(): Strictness {
  const s = (process.env.DEMARKUS_MEMORY_DEST_STRICTNESS || readTrimmed(PLUGIN_DEST_STRICTNESS_FILE)).replace(/\s/g, "");
  return s === "warn" || s === "ask" || s === "block" ? s : "block";
}

// --- soul catalog + bindings --------------------------------------------------

export function localSoulPresent(): boolean {
  return existsSync(PLUGIN_CONFIG);
}

export function knowledgeEndpoints(): string[] {
  return records(KNOWLEDGE_REGISTRY);
}

export function knowledgePresent(): boolean {
  return knowledgeEndpoints().length > 0;
}

// Remote-soul slugs (tab-separated rows, field 1).
export function listRemoteSouls(): string[] {
  return records(SOULS_REGISTRY).map((r) => r.split("\t")[0]);
}

export function isRegisteredRemoteSoul(slug: string): boolean {
  return slug.length > 0 && listRemoteSouls().includes(slug);
}

// project_soul_binding: the catalog slug bound to repo DIR, exact match.
export function projectSoulBinding(dir: string): string {
  for (const row of records(PROJECT_SOULS)) {
    const [d, slug] = row.split("\t");
    if (d === dir) return slug ?? "";
  }
  return "";
}

export function promoteTargets(): string[] {
  return records(PROMOTE_TARGETS);
}

// promote_destination_present: any brokered knowledge system OR a plain target.
export function promoteDestinationPresent(): boolean {
  return knowledgePresent() || promoteTargets().length > 0;
}

// --- MCP tool-name parsing ----------------------------------------------------
//
// Across harnesses a demarkus MCP tool name takes one of these shapes:
//   - "mcp__<server>__mark_<verb>"   (Claude Code style)
//   - "<server>_mark_<verb>"         (pi-mcp-adapter, prefix "server"; the
//                                      server name has its hyphens turned to "_")
//   - "mark_<verb>"                  (pi-mcp-adapter, prefix "none")
// We extract the verb and the server segment so scope can be resolved.

export interface ParsedTool {
  server: string; // "" when un-prefixed (pi prefix "none")
  verb: string; // e.g. "publish", "append", "fetch"
}

export function parseMcpTool(toolName: string): ParsedTool | null {
  const m = toolName.match(/mark_([a-z_]+)$/);
  if (!m) return null;
  const verb = m[1];
  let server = toolName.slice(0, toolName.length - `mark_${verb}`.length);
  // strip the Claude "mcp__" prefix and the trailing separators ("__" or "_")
  server = server.replace(/^mcp__/, "").replace(/__$/, "").replace(/_$/, "");
  return { server, verb };
}

// Normalize a server segment so "demarkus-memory" and "demarkus_memory" compare
// equal regardless of which harness's separator convention produced it.
function norm(s: string): string {
  return s.replace(/-/g, "_").toLowerCase();
}

function serverIsLocalSoul(server: string): boolean {
  if (server === "") return true; // un-prefixed (prefix "none"): assume the local soul
  const n = norm(server);
  return n === norm(LOCAL_SOUL_ID) || n.endsWith(`_${norm(LOCAL_SOUL_ID)}`);
}

// publish_gate_scope: "local" when the tool targets a soul this plugin owns
// (local managed soul, or a registered remote soul), else "".
export function publishGateScope(toolName: string): "local" | "" {
  const parsed = parseMcpTool(toolName);
  if (!parsed) return "";
  if (serverIsLocalSoul(parsed.server)) return "local";
  // remote soul: server segment equals a registered slug (separator-insensitive)
  if (listRemoteSouls().some((slug) => norm(slug) === norm(parsed.server))) return "local";
  return "";
}

// soul_target_id: canonical id of the soul a write targets, for binding compare.
// "" when the tool targets a knowledge system or an unrelated server.
export function soulTargetId(toolName: string): string {
  const parsed = parseMcpTool(toolName);
  if (!parsed) return "";
  if (serverIsLocalSoul(parsed.server)) return LOCAL_SOUL_ID;
  const match = listRemoteSouls().find((slug) => norm(slug) === norm(parsed.server));
  return match ?? "";
}

// --- managed-server config (PLUGIN_CONFIG: SOUL_DIR / PORT / MODE) ------------

export interface PluginConfig {
  soulDir: string;
  port: string;
  mode: string;
}

// load_config: parse the shell-style conf written by setup.sh. We only need the
// three values; each is a single `KEY=value` line (printf %q-quoted, but our
// values are plain paths/ints/words, so a simple unquote suffices).
export function loadConfig(): PluginConfig | null {
  if (!existsSync(PLUGIN_CONFIG)) return null;
  const out: Record<string, string> = {};
  for (const line of readLines(PLUGIN_CONFIG)) {
    const m = line.match(/^(SOUL_DIR|PORT|MODE)=(.*)$/);
    if (!m) continue;
    let v = m[2].trim();
    // %q may single-quote or backslash-escape; strip a surrounding pair / leading $'
    if (v.startsWith("$'") && v.endsWith("'")) v = v.slice(2, -1);
    else if (v.startsWith("'") && v.endsWith("'")) v = v.slice(1, -1);
    v = v.replace(/\\(.)/g, "$1");
    out[m[1]] = v;
  }
  if (!out.SOUL_DIR || !out.PORT || !out.MODE) return null;
  return { soulDir: out.SOUL_DIR, port: out.PORT, mode: out.MODE };
}
