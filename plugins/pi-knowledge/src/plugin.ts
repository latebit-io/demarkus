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

// runBin pipes a JSON payload (or none) to a demarkus-plugin subcommand and
// returns parsed stdout, or null on any failure (missing binary, timeout, parse).
function runBin<T>(args: string[], payload?: unknown): T | null {
  if (!existsSync(BIN)) return null;
  try {
    const opts: { encoding: "utf8"; timeout: number; input?: string } = { encoding: "utf8", timeout: 5000 };
    if (payload !== undefined) opts.input = JSON.stringify(payload);
    const out = execFileSync(BIN, args, opts).trim();
    if (!out) return null;
    return JSON.parse(out) as T;
  } catch {
    return null;
  }
}

// callGate asks `demarkus-plugin gate` to decide a mark_publish/mark_append call.
// Input is passed through verbatim (the binary unwraps the pi-mcp-adapter proxy
// shape itself). Returns allow on any failure (missing binary, timeout, parse).
export function callGate(toolName: string, input: Record<string, unknown>, cwd: string): GateDecision {
  const d = runBin<GateDecision>(["gate"], { tool: toolName, input, cwd });
  return d && typeof d.decision === "string" ? d : { decision: "allow" };
}

// callNudge asks `demarkus-plugin nudge` for a recall/promote/session-end
// reminder; returns the text or "" (no nudge / binary unavailable).
export function callNudge(req: Record<string, unknown>): string {
  const o = runBin<{ nudge?: string }>(["nudge"], req);
  return o?.nudge ?? "";
}

// callGuidance asks `demarkus-plugin guidance` for the session-start context for
// SURFACE, wrapping the plugin's bundled static guidance file. "" when there's
// nothing to inject (or the binary is unavailable).
export function callGuidance(surface: "memory" | "knowledge", guidanceFile: string): string {
  const o = runBin<{ context?: string }>(["guidance", "--surface", surface, "--guidance-file", guidanceFile]);
  return o?.context ?? "";
}
