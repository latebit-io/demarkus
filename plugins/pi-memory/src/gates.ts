// Write-time gates, ported from hooks/publish-gate.sh and hooks/dest-gate.sh.
//
// In pi the only pre-call lever is tool_call returning { block, reason }; there
// is no post-call context channel and no native "ask". So the three strictness
// levels map as:
//   block → block the call (agent self-corrects)
//   ask   → block the call too, with a reason that tells the agent to confirm
//           with the user before re-issuing (closest pi has to Claude's "ask")
//   warn  → allow the call, and surface a reminder message (non-blocking)
// The decision object below is harness-agnostic; index.ts applies it.

import {
  configuredDestStrictness,
  configuredStrictness,
  parseMcpTool,
  projectSoulBinding,
  publishGateScope,
  soulTargetId,
  type Strictness,
} from "./config.js";

export type GateDecision =
  | { action: "allow" }
  | { action: "block"; reason: string }
  | { action: "warn"; reason: string };

// tags must be a non-empty string after trim.
function tagsOk(metadata: unknown): boolean {
  if (typeof metadata !== "object" || metadata === null) return false;
  const tags = (metadata as Record<string, unknown>).tags;
  return typeof tags === "string" && tags.trim().length > 0;
}

// importance: absent is fine; otherwise a number (or numeric string) in [0,1].
function importanceOk(metadata: unknown): boolean {
  if (typeof metadata !== "object" || metadata === null) return true;
  const imp = (metadata as Record<string, unknown>).importance;
  if (imp === undefined || imp === null) return true;
  const n = typeof imp === "number" ? imp : Number(String(imp));
  return Number.isFinite(n) && n >= 0 && n <= 1;
}

function strictnessToAction(s: Strictness, reason: string, askSuffix: string): GateDecision {
  switch (s) {
    case "block":
      return { action: "block", reason };
    case "ask":
      return { action: "block", reason: `${reason} ${askSuffix}` };
    case "warn":
      return { action: "warn", reason: `⚠️ ${reason}` };
  }
}

// publishTagGate — fires only on this plugin's own soul publishes. Returns the
// decision for a publish whose metadata is missing tags or has out-of-range
// importance; otherwise { action: "allow" }.
export function publishTagGate(toolName: string, input: Record<string, unknown>): GateDecision {
  const parsed = parseMcpTool(toolName);
  if (!parsed || parsed.verb !== "publish") return { action: "allow" };
  if (publishGateScope(toolName) !== "local") return { action: "allow" };

  const metadata = input.metadata;
  const tOk = tagsOk(metadata);
  const iOk = importanceOk(metadata);
  if (tOk && iOk) return { action: "allow" };

  const target = typeof input.url === "string" && input.url ? input.url : "the document";
  const problems: string[] = [];
  if (!tOk) problems.push("no metadata.tags (it will be invisible to mark_lookup)");
  if (!iOk) problems.push("metadata.importance outside [0,1]");
  const reason =
    `demarkus publish to ${target} has ${problems.join("; ")}. ` +
    "Re-issue mark_publish with a metadata object: tags (comma-separated subjects derived from the content) " +
    "and, if set, importance in [0,1]. Tags are what make this document findable via mark_lookup.";

  return strictnessToAction(configuredStrictness(), reason, "Confirm with the user before proceeding.");
}

// destinationGate — fires only when (a) the tool targets a soul this plugin
// routes AND (b) the project is bound to a DIFFERENT soul. Otherwise allow.
export function destinationGate(toolName: string, input: Record<string, unknown>, cwd: string): GateDecision {
  const parsed = parseMcpTool(toolName);
  if (!parsed || (parsed.verb !== "publish" && parsed.verb !== "append")) return { action: "allow" };

  const targetId = soulTargetId(toolName);
  if (!targetId) return { action: "allow" }; // knowledge system or unrelated server

  const bound = projectSoulBinding(cwd);
  if (!bound) return { action: "allow" }; // no binding → nothing to enforce
  if (targetId === bound) return { action: "allow" };

  const target = typeof input.url === "string" && input.url ? input.url : "this document";
  const reason =
    `demarkus write to ${target} is going to soul '${targetId}', but this project is bound to soul '${bound}'. ` +
    `Re-issue the write against the bound soul's tools (the ${bound} MCP server's mark_publish / mark_append). ` +
    "To change which soul this project uses, run /soul-join in this repo; to relax this check, set " +
    "DEMARKUS_MEMORY_DEST_STRICTNESS=warn (or ask).";

  return strictnessToAction(configuredDestStrictness(), reason, "Confirm with the user before proceeding.");
}
