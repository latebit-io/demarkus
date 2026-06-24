// Publish tag-gate, ported from hooks/publish-gate.sh. Enforces tags +
// importance + per-system required tag axes + required OKF fields on writes to a
// joined knowledge system. Strictness maps onto pi's tool_call like the memory
// gate (block/ask → block; warn → allow + injected reminder).

import {
  configuredRequireFields,
  configuredRequireTags,
  configuredStrictness,
  publishGateScope,
  tagsHaveAxis,
  type Strictness,
} from "./config.js";

export type GateDecision =
  | { action: "allow" }
  | { action: "block"; reason: string }
  | { action: "warn"; reason: string };

function tagsString(metadata: unknown): string {
  if (typeof metadata !== "object" || metadata === null) return "";
  const tags = (metadata as Record<string, unknown>).tags;
  return typeof tags === "string" ? tags : "";
}

function importanceOk(metadata: unknown): boolean {
  if (typeof metadata !== "object" || metadata === null) return true;
  const imp = (metadata as Record<string, unknown>).importance;
  if (imp === undefined || imp === null) return true;
  const n = typeof imp === "number" ? imp : Number(String(imp));
  return Number.isFinite(n) && n >= 0 && n <= 1;
}

function typeString(metadata: unknown): string {
  if (typeof metadata !== "object" || metadata === null) return "";
  const t = (metadata as Record<string, unknown>).type;
  return typeof t === "string" ? t.trim() : "";
}

function strictnessToAction(s: Strictness, reason: string): GateDecision {
  switch (s) {
    case "block":
      return { action: "block", reason };
    case "ask":
      return { action: "block", reason: `${reason} Confirm with the user before proceeding.` };
    case "warn":
      return { action: "warn", reason: `⚠️ ${reason}` };
  }
}

// publishGate — fires only on a registered knowledge system. Returns the
// decision for a non-compliant publish; otherwise { action: "allow" }.
export function publishGate(toolName: string, input: Record<string, unknown>): GateDecision {
  const scope = publishGateScope(toolName);
  if (!scope.startsWith("ks:")) return { action: "allow" };
  const slug = scope.slice(3);

  const metadata = input.metadata;
  const tags = tagsString(metadata).trim();
  const tagsOk = tags.length > 0;
  const impOk = importanceOk(metadata);
  const url = typeof input.url === "string" ? input.url : "";

  // Required tag axes — only checked when tags are present (a tagless publish is
  // already a violation).
  const missingAxes: string[] = [];
  if (tagsOk) {
    for (const axis of configuredRequireTags(slug)) {
      if (!tagsHaveAxis(tags, axis)) missingAxes.push(axis);
    }
  }

  // Required OKF fields — only `type` is inspected; index.md/log.md are exempt
  // (a hub is intentionally untyped). Any other declared field is skipped fail-open.
  const missingFields: string[] = [];
  const leaf = url.split("/").pop() ?? "";
  for (const field of configuredRequireFields(slug)) {
    if (field === "type") {
      if (leaf !== "index.md" && leaf !== "log.md" && typeString(metadata).length === 0) {
        missingFields.push("type");
      }
    }
    // unsupported fields: skipped fail-open (the gate can't see them)
  }

  if (tagsOk && impOk && missingAxes.length === 0 && missingFields.length === 0) {
    return { action: "allow" };
  }

  const target = url || "the document";
  const problems: string[] = [];
  if (!tagsOk) problems.push("no metadata.tags (it will be invisible to mark_lookup)");
  if (!impOk) problems.push("metadata.importance outside [0,1]");
  if (missingAxes.length > 0) {
    problems.push(
      `missing required tag axes for this knowledge system: ${missingAxes.join(" ")} ` +
        `(tag as "axis:value", e.g. ${missingAxes[0]}:<value>)`,
    );
  }
  if (missingFields.length > 0) {
    problems.push(
      `missing required OKF metadata fields: ${missingFields.join(" ")} ` +
        `(set a "${missingFields[0]}" key in the metadata object, e.g. metadata: {"type": "Reference"})`,
    );
  }
  const reason =
    `demarkus publish to ${target} (knowledge system '${slug}') has ${problems.join("; ")}. ` +
    "Re-issue mark_publish with a metadata object: tags (comma-separated subjects derived from the content) " +
    "and, if set, importance in [0,1]. Tags are what make this document findable via mark_lookup across the " +
    "shared catalog.";

  return strictnessToAction(configuredStrictness(slug), reason);
}
