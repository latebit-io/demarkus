// Thin bridge to the shared `demarkus-plugin` binary — the single source of
// truth for gate (and, in later phases, nudge/guidance) decisions across every
// harness. The pi extension stays an adapter: it hands the binary a normalized
// tool call and applies the returned decision. If the binary isn't installed yet
// (pre-provisioning), every call fails OPEN so nothing is ever wrongly blocked.

import { execFileSync } from "node:child_process";
import { existsSync } from "node:fs";
import { homedir } from "node:os";
import { join } from "node:path";

const BIN = join(homedir(), ".demarkus", "bin", "demarkus-plugin");

export interface GateDecision {
  decision: "allow" | "warn" | "block" | "ask";
  reason?: string;
}

// callGate asks `demarkus-plugin gate` to decide a mark_publish/mark_append call.
// Input is passed through verbatim (the binary unwraps the pi-mcp-adapter proxy
// shape itself). Returns allow on any failure (missing binary, timeout, parse).
export function callGate(toolName: string, input: Record<string, unknown>, cwd: string): GateDecision {
  if (!existsSync(BIN)) return { decision: "allow" };
  try {
    const payload = JSON.stringify({ tool: toolName, input, cwd });
    const out = execFileSync(BIN, ["gate"], { input: payload, encoding: "utf8", timeout: 5000 });
    const parsed = JSON.parse(out) as GateDecision;
    if (parsed && typeof parsed.decision === "string") return parsed;
    return { decision: "allow" };
  } catch {
    return { decision: "allow" };
  }
}
