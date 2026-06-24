// Thin bridge to the shared `demarkus-plugin` binary — the single source of truth
// for gate / nudge / guidance decisions across every harness. The pi extension
// stays an adapter: it hands the binary a normalized request and applies the
// result. Calls are async (non-blocking execFile) so a per-tool-call gate never
// stalls pi's event loop. If the binary isn't installed yet (pre-provisioning),
// the gate fails OPEN — matching the bash adapters' `[ -x BIN ] || exit 0`, so an
// unprovisioned session never wrongly blocks legitimate writes.

import { execFile } from "node:child_process";
import { existsSync } from "node:fs";
import { homedir } from "node:os";
import { join } from "node:path";

const BIN = join(homedir(), ".demarkus", "bin", "demarkus-plugin");

export interface GateDecision {
  decision: "allow" | "warn" | "block" | "ask";
  reason?: string;
}

// runBin pipes an optional JSON payload to a demarkus-plugin subcommand and
// resolves parsed stdout, or null on any failure (missing binary, timeout, parse).
function runBin<T>(args: string[], payload?: unknown): Promise<T | null> {
  return new Promise((resolve) => {
    if (!existsSync(BIN)) {
      resolve(null);
      return;
    }
    const child = execFile(BIN, args, { encoding: "utf8", timeout: 5000 }, (err, stdout) => {
      if (err) {
        resolve(null);
        return;
      }
      const out = (stdout || "").trim();
      if (!out) {
        resolve(null);
        return;
      }
      try {
        resolve(JSON.parse(out) as T);
      } catch {
        resolve(null);
      }
    });
    if (payload !== undefined && child.stdin) {
      child.stdin.end(JSON.stringify(payload));
    }
  });
}

// callGate asks `demarkus-plugin gate` to decide a mark_publish/mark_append call.
// Input is passed verbatim (the binary unwraps the pi-mcp-adapter proxy itself).
// Allow on any failure (missing binary, timeout, parse).
export async function callGate(toolName: string, input: Record<string, unknown>, cwd: string): Promise<GateDecision> {
  const d = await runBin<GateDecision>(["gate"], { tool: toolName, input, cwd });
  return d && typeof d.decision === "string" ? d : { decision: "allow" };
}

// callNudge asks `demarkus-plugin nudge` for a recall/promote/session-end
// reminder; resolves the text or "" (no nudge / binary unavailable).
export async function callNudge(req: Record<string, unknown>): Promise<string> {
  const o = await runBin<{ nudge?: string }>(["nudge"], req);
  return o?.nudge ?? "";
}

// callGuidance asks `demarkus-plugin guidance` for the session-start context for
// SURFACE, wrapping the plugin's bundled static guidance file. Resolves the text
// ("" when the binary ran but had nothing to inject) or null when the binary is
// unavailable / failed — so the caller can retry rather than burn its one-shot
// guidance flag on a transient failure.
export async function callGuidance(surface: "memory" | "knowledge", guidanceFile: string): Promise<string | null> {
  const o = await runBin<{ context?: string }>(["guidance", "--surface", surface, "--guidance-file", guidanceFile]);
  return o === null ? null : (o.context ?? "");
}
