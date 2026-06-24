// TypeScript port of the read-only logic in scripts/lib.sh — the registry,
// per-slug strictness / required-tag-axes / required-fields, MCP-tool scope
// resolution, and the axis-presence check. Provisioning + registry/policy
// mutation stay in the bundled bash.

import { existsSync, readFileSync } from "node:fs";
import {
  PLUGIN_KNOWLEDGE_REGISTRY,
  PLUGIN_REQUIRE_FIELDS_FILE,
  PLUGIN_REQUIRE_TAGS_FILE,
  PLUGIN_STRICTNESS_FILE,
  SOUL_CONFIG,
} from "./paths.js";

export type Strictness = "warn" | "block" | "ask";

function readText(path: string): string {
  try {
    return readFileSync(path, "utf8");
  } catch {
    return "";
  }
}

// Normalize a comma/whitespace/newline-separated list to a clean token array.
function tokens(raw: string): string[] {
  return raw
    .split(/[\s,]+/)
    .map((t) => t.trim())
    .filter((t) => t.length > 0);
}

export function listKnowledgeSystems(): string[] {
  return readText(PLUGIN_KNOWLEDGE_REGISTRY)
    .split("\n")
    .map((l) => l.trim())
    .filter((l) => l.length > 0);
}

export function isRegisteredKnowledgeSystem(slug: string): boolean {
  return slug.length > 0 && listKnowledgeSystems().includes(slug);
}

export function soulIsConfigured(): boolean {
  return existsSync(SOUL_CONFIG);
}

// configured_strictness [slug]: env override, then per-slug file, then warn.
// Any unrecognized value floors at warn so a typo can't silently escalate.
export function configuredStrictness(slug: string): Strictness {
  let s = process.env.DEMARKUS_KNOWLEDGE_STRICTNESS || "";
  if (!s && slug) s = readText(`${PLUGIN_STRICTNESS_FILE}.${slug}`);
  s = s.replace(/\s/g, "");
  return s === "block" || s === "ask" || s === "warn" ? s : "warn";
}

export function configuredRequireTags(slug: string): string[] {
  return slug ? tokens(readText(`${PLUGIN_REQUIRE_TAGS_FILE}.${slug}`)) : [];
}

export function configuredRequireFields(slug: string): string[] {
  return slug ? tokens(readText(`${PLUGIN_REQUIRE_FIELDS_FILE}.${slug}`)) : [];
}

// tags_have_axis: the comma-separated TAGS list contains an "AXIS:value" token.
export function tagsHaveAxis(tags: string, axis: string): boolean {
  return tags.split(",").some((tok) => tok.trim().startsWith(`${axis}:`));
}

// --- MCP tool-name parsing (see pi-memory/src/config.ts for the shapes) ------

export interface ParsedTool {
  server: string;
  verb: string;
}

export function parseMcpTool(toolName: string): ParsedTool | null {
  const m = toolName.match(/mark_([a-z_]+)$/);
  if (!m) return null;
  const verb = m[1];
  let server = toolName.slice(0, toolName.length - `mark_${verb}`.length);
  server = server.replace(/^mcp__/, "").replace(/__$/, "").replace(/_$/, "");
  return { server, verb };
}

function norm(s: string): string {
  return s.replace(/-/g, "_").toLowerCase();
}

// publish_gate_scope: "ks:<slug>" when the tool targets a registered knowledge
// system, "" otherwise. The local demarkus-memory soul is explicitly NOT ours to
// gate — that is the demarkus-memory plugin's publish gate.
export function publishGateScope(toolName: string): string {
  const parsed = parseMcpTool(toolName);
  if (!parsed || parsed.verb !== "publish") return "";
  const n = norm(parsed.server);
  if (n === "demarkus_memory" || n.endsWith("_demarkus_memory")) return "";
  const match = listKnowledgeSystems().find((slug) => norm(slug) === n);
  return match ? `ks:${match}` : "";
}
