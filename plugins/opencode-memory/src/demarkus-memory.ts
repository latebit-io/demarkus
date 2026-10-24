// OpenCode adapter; policy and provisioning stay in demarkus-plugin.

import { execFile } from "node:child_process";
import { randomUUID } from "node:crypto";
import { existsSync, readFileSync } from "node:fs";
import { homedir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import type { Plugin } from "@opencode/plugin";
import { assertPackageInstall, loadCommand as readCommand, loadCommands, registerPrompts } from "./package.ts";

const BIN = join(homedir(), ".demarkus", "bin", "demarkus-plugin");
const ASSETS = fileURLToPath(new URL("../", import.meta.url));
const COMMANDS_DIR = join(ASSETS, "commands");
const SCRIPTS_DIR = join(ASSETS, "scripts");
const GUIDANCE_FILE = join(ASSETS, "context", "session-guidance.md");
const MCP_SERVER_NAME = "demarkus-memory";
const GATE_ADAPTERS = Symbol.for("io.demarkus.opencode.gate-adapters");

interface GateDecision {
  decision: "allow" | "warn" | "block" | "ask";
  reason?: string;
}

const reportedFailures = new Set<string>();

function reportFailure(operation: string, detail: string): void {
  const message = `${operation}: ${detail}`;
  if (reportedFailures.has(message)) return;
  reportedFailures.add(message);
  console.error(`[demarkus-memory] ${message}`);
}

// runBin pipes an optional JSON payload to a demarkus-plugin subcommand and
// resolves parsed stdout, {} for an allowed empty response, or null on failure.
function runBin<T>(args: string[], payload?: unknown, allowEmpty = false, bin = BIN): Promise<T | null> {
  return new Promise((resolve) => {
    const operation = args[0] ?? "helper";
    if (!existsSync(bin)) {
      reportFailure(operation, `${bin} is not installed; this call is allowed to continue`);
      resolve(null);
      return;
    }
    const child = execFile(bin, args, { encoding: "utf8", timeout: 5000 }, (err, stdout) => {
      if (err) {
        reportFailure(operation, err.message);
        resolve(null);
        return;
      }
      const output = (stdout || "").trim();
      if (!output) {
        if (allowEmpty) {
          resolve({} as T);
          return;
        }
        reportFailure(operation, "helper returned an empty response");
        resolve(null);
        return;
      }
      try {
        resolve(JSON.parse(output) as T);
      } catch (error) {
        reportFailure(operation, `invalid helper response: ${error}`);
        resolve(null);
      }
    });
    if (payload !== undefined && child.stdin) {
      child.stdin.end(JSON.stringify(payload));
    }
  });
}

// callGate asks `demarkus-plugin gate` to decide a mark_publish/mark_append call.
// Fails closed: only a missing helper allows; crash, timeout or bad output blocks.
export async function callGate(toolName: string, input: Record<string, unknown>, cwd: string, bin = BIN): Promise<GateDecision> {
  if (!existsSync(bin)) return { decision: "allow" };
  const d = await runBin<GateDecision>(["gate"], { tool: toolName, input, cwd }, false, bin);
  if (d && typeof d.decision === "string") return d;
  return { decision: "block", reason: `[demarkus-memory] gate failed (crash, timeout or bad output); write blocked. Retry, or reinstall ${bin}` };
}

async function callNudge(req: Record<string, unknown>): Promise<string> {
  const o = await runBin<{ nudge?: string }>(["nudge"], req, true);
  return o?.nudge ?? "";
}

// context for the model, status for the user; "" = nothing to say.
type Guidance = { context: string; status: string };

// null = binary unavailable/failed (caller retries next turn).
async function callGuidance(directory: string): Promise<Guidance | null> {
  const o = await runBin<{ context?: string; status?: string }>(
    ["guidance", "--surface", "memory", "--guidance-file", GUIDANCE_FILE, "--project-dir", directory],
    undefined,
    true,
  );
  return o === null ? null : { context: o.context ?? "", status: o.status ?? "" };
}

function isMemoryWrite(toolName: string): boolean {
  return /mark_(publish|append)$/.test(toolName);
}

// Mirrors config.IsJournalWrite in the plugin binary: route today, or a url
// under a journal/ directory. A plan or ADR write is not a journal entry.
function isJournalWrite(args: Record<string, unknown> | undefined): boolean {
  const url = typeof args?.url === "string" ? args.url : "";
  return args?.route === "today" || url.includes("/journal/");
}

// trackActivity records one completed tool call in the session's signals and
// reports whether it was a memory write. An edit after a journal entry leaves
// the entry stale.
function trackActivity(s: SessionState, tool: string, args: Record<string, unknown> | undefined): boolean {
  if (!isMemoryWrite(tool)) {
    if (isFileMutation(tool)) {
      s.changedFiles = true;
      s.journalWrite = false;
    }
    return false;
  }
  if (isJournalWrite(args)) s.journalWrite = true;
  return true;
}

function isFileMutation(toolName: string): boolean {
  return /^(edit|write|patch|multiedit)$/i.test(toolName);
}

// Per-session adapter state: one guidance injection, one idle nudge, and the
// two session-end signals (files changed / journal written).
interface SessionState {
  guidanceDelivered: boolean;
  idleNudged: boolean;
  changedFiles: boolean;
  journalWrite: boolean;
}

// run executes a provisioning step; resolves "" on success, a warning on failure.
function run(cmd: string, args: string[], label: string, timeout: number): Promise<string> {
  return new Promise((resolve) => {
    execFile(cmd, args, { timeout }, (err, _out, stderr) => {
      if (err) {
        const tail = (stderr || "").trim().split("\n").slice(-3).join(" ");
        resolve(`demarkus-memory ${label} failed: ${tail || err.message}. Run /soul-init to recover.`);
        return;
      }
      resolve("");
    });
  });
}

// bootstrap.sh installs the pinned demarkus-plugin binary (no-op when current),
// then the binary provisions everything else (server, mcp, token, seed).
// Bootstrap bound must cover curl's real worst case: --max-time 300 is per
// attempt and --retry 3 resets it, two downloads sequential (~2400s ceiling).
async function provision(): Promise<string> {
  const bootErr = await run("bash", [join(SCRIPTS_DIR, "bootstrap.sh")], "bootstrap", 2_500_000);
  if (bootErr) return bootErr;
  return run(BIN, ["provision"], "provisioning", 600_000);
}

export function loadCommand(file: string, commandsDir = COMMANDS_DIR): { description: string; template: string } {
  return readCommand(file, commandsDir, SCRIPTS_DIR);
}

// The legacy adapter only needs the toast subset of the V1 client.
type ToastClient = {
  tui?: { showToast?: (args: { body: { message: string; variant: string } }) => Promise<unknown> };
};

type GateGlobal = typeof globalThis & { [key: symbol]: unknown };

function gateAdapters(directory: string): Set<string> {
  const root = globalThis as GateGlobal;
  const current = root[GATE_ADAPTERS];
  const registry = current instanceof Map ? (current as Map<string, Set<string>>) : new Map<string, Set<string>>();
  if (!(current instanceof Map)) root[GATE_ADAPTERS] = registry;
  let adapters = registry.get(directory);
  if (!adapters) {
    adapters = new Set<string>();
    registry.set(directory, adapters);
  }
  return adapters;
}

export const DemarkusMemoryPlugin = async ({ client, directory }: { client: ToastClient; directory: string }) => {
  const adapters = gateAdapters(directory);
  adapters.add("memory");
  let disposed = false;
  const sessions = new Map<string, SessionState>();
  // Warn reasons carried from the before-hook to the after-hook (block/ask
  // never reach after: before throws), so the gate subprocess runs once per
  // write. Keyed sessionID:callID; entries for failed calls (no after-hook)
  // are pruned with their session on session.deleted.
  const pendingWarns = new Map<string, string>();
  const state = (id: string): SessionState => {
    let s = sessions.get(id);
    if (!s) {
      s = { guidanceDelivered: false, idleNudged: false, changedFiles: false, journalWrite: false };
      sessions.set(id, s);
    }
    return s;
  };

  const toast = async (message: string, variant: "info" | "warning" = "info") => {
    try {
      await client.tui?.showToast?.({ body: { message, variant } });
    } catch {
      console.error(`[demarkus-memory] ${message}`);
    }
  };

  // Provision on init and again on every session.created (matching the
  // claude/pi per-session cadence) so a dead managed server respawns without an
  // OpenCode restart. Fire-and-forget with an in-flight guard: a first-run
  // binary download must not block startup or stack concurrent runs.
  let provisioning: Promise<void> | null = null;
  const ensureProvisioned = () => {
    if (disposed) return;
    provisioning ??= provision()
      .then((warning) => {
        if (warning && !disposed) void toast(warning, "warning");
      })
      .finally(() => {
        provisioning = null;
      });
  };
  ensureProvisioned();

  return {
    // Register the MCP servers and the slash commands by mutating the merged
    // config. Commands come from the installed markdown so the prompt bodies
    // live in exactly one place.
    config: async (config: Record<string, any>) => {
      config.mcp = config.mcp ?? {};
      config.mcp[MCP_SERVER_NAME] = config.mcp[MCP_SERVER_NAME] ?? {
        type: "local",
        command: [BIN, "mcp-serve"],
        enabled: true,
      };

      // OpenCode never reads the MCP config `registry mcp add` writes, so wire
      // every joined memory from the shared catalog here (TSV, "#" comments).
      // Follow-up: read it via `registry memory-default --list` instead.
      try {
        const memories = readFileSync(join(homedir(), ".demarkus", "souls"), "utf8");
        for (const rawLine of memories.split("\n")) {
          const line = rawLine.trim();
          if (line.startsWith("#")) continue;
          const [slugRaw, hostRaw] = line.split("\t");
          const slug = slugRaw?.trim();
          const host = hostRaw?.trim() ?? "";
          if (!slug || slug === MCP_SERVER_NAME || config.mcp[slug]) continue;
          // Broker rows (https host) are OAuth HTTP endpoints at <host>/mcp, the
          // same derivation as registry.ValidateBrokerEndpoint; never mcp-serve.
          config.mcp[slug] = host.startsWith("https://")
            ? { type: "remote", url: host + "/mcp", enabled: true }
            : {
                type: "local",
                command: [BIN, "mcp-serve", "--memory", slug],
                enabled: true,
              };
        }
      } catch (e) {
        // ENOENT = nothing joined yet. Anything else must be loud: it makes
        // every joined memory silently vanish from the MCP config.
        if ((e as NodeJS.ErrnoException)?.code !== "ENOENT") {
          console.error(`[demarkus-memory] memories catalog unreadable, remote memories not wired: ${e}`);
        }
      }

      config.command = config.command ?? {};
      for (const [name, command] of Object.entries(loadCommands(ASSETS))) {
        config.command[name] ??= command;
      }
    },

    // Standing guidance once per session + recall nudge, appended as synthetic
    // text parts on the user message.
    "chat.message": async (
      _input: unknown,
      output: { message: { id?: string; sessionID?: string }; parts: Array<Record<string, unknown>> },
    ) => {
      const s = state(output.message.sessionID ?? "default");
      const promptText = output.parts
        .filter((p) => p.synthetic !== true && p.type === "text" && typeof p.text === "string")
        .map((p) => p.text)
        .join("\n");

      const [guidance, recall] = await Promise.all([
        s.guidanceDelivered ? Promise.resolve(null) : callGuidance(directory),
        callNudge({ event: "recall", surface: "memory", prompt: promptText }),
      ]);

      const texts: string[] = [];
      // null = binary not ready; retry next turn instead of burning the flag.
      if (guidance !== null && !s.guidanceDelivered) {
        if (guidance.context) texts.push(guidance.context);
        if (guidance.status) void toast(guidance.status);
        s.guidanceDelivered = true;
      }
      if (recall) texts.push(recall);

      // Parts are schema-validated on save: id/sessionID/messageID are required,
      // so borrow them from the message (falling back to the first real part).
      const base = output.parts[0] ?? {};
      for (const text of texts) {
        output.parts.push({
          id: `prt_${randomUUID().replace(/-/g, "")}`,
          sessionID: output.message.sessionID ?? base.sessionID,
          messageID: output.message.id ?? base.messageID,
          type: "text",
          text,
          synthetic: true,
        });
      }
    },

    // Gate memory writes before execution. block → throw (OpenCode cancels the
    // call and shows the reason). ask → no native ask here, so block with a
    // reason that routes through the user. allow/warn is stashed for the
    // after-hook so the gate runs once per write.
    "tool.execute.before": async (
      input: { tool: string; sessionID?: string; callID?: string },
      output: { args: Record<string, unknown> },
    ) => {
      if (!isMemoryWrite(input.tool) || !adapters.has("memory")) return;
      const decision = await callGate(input.tool, output.args ?? {}, directory);
      if (decision.decision === "block") {
        throw new Error(decision.reason ?? "demarkus-memory gate blocked this write");
      }
      if (decision.decision === "ask") {
        throw new Error(`${decision.reason ?? "blocked"} Confirm with the user before retrying this write.`);
      }
      // Warn is advisory: only stash it when the IDs exist to key it safely
      // (a degenerate shared key could replay a stale warn on another session).
      if (decision.decision === "warn" && decision.reason && input.sessionID && input.callID) {
        pendingWarns.set(`${input.sessionID}:${input.callID}`, decision.reason);
      }
    },

    // After an allowed call: track activity signals, surface a warn-strictness
    // reminder, and the promote nudge on a fresh ADR publish. Tool args ride on
    // input here (output carries title/output/metadata).
    "tool.execute.after": async (
      input: { tool: string; sessionID?: string; callID?: string; args?: Record<string, unknown> },
      output: { output: string },
    ) => {
      if (!adapters.has("memory")) return;
      const s = state(input.sessionID ?? "default");
      if (!trackActivity(s, input.tool, input.args)) return;

      const key = `${input.sessionID}:${input.callID}`;
      const warn = pendingWarns.get(key);
      pendingWarns.delete(key);
      if (warn) {
        output.output += `\n\n⚠️ ${warn}`;
      }
      const nudge = await callNudge({ event: "promote", tool: input.tool, input: input.args ?? {} });
      if (nudge) output.output += `\n\n${nudge}`;
    },

    // Session-end journal nudge. session.idle fires after every turn, so a
    // per-session sentinel keeps this to one toast; no extra turn can be
    // forced (unlike Claude's Stop block). session.deleted prunes state.
    event: async ({
      event,
    }: {
      event: { type: string; properties?: { sessionID?: string; info?: { id?: string } } };
    }) => {
      if (event.type === "session.created") {
        ensureProvisioned();
        return;
      }
      if (event.type === "session.deleted") {
        // session.deleted carries the Session under properties.info.
        const id = event.properties?.info?.id;
        if (id) {
          sessions.delete(id);
          for (const key of pendingWarns.keys()) {
            if (key.startsWith(`${id}:`)) pendingWarns.delete(key);
          }
        }
        return;
      }
      if (event.type !== "session.idle") return;
      const s = state(event.properties?.sessionID ?? "default");
      // Pre-filter with the same signals the binary checks, so the common
      // nothing-to-say case doesn't spawn a subprocess after every turn.
      if (s.idleNudged || !s.changedFiles || s.journalWrite) return;
      s.idleNudged = true;
      const nudge = await callNudge({
        event: "session-end",
        cwd: directory,
        changedFiles: s.changedFiles,
        journalWrite: s.journalWrite,
      });
      if (nudge) await toast(nudge);
    },

    dispose: () => {
      if (disposed) return;
      disposed = true;
      sessions.clear();
      pendingWarns.clear();
      adapters.delete("memory");
      if (adapters.size === 0) {
        ((globalThis as GateGlobal)[GATE_ADAPTERS] as Map<string, Set<string>>).delete(directory);
      }
    },
  };
};

// V2 loads an object with setup(); V1 (1.18.29+) calls server(). Keep the
// V1 adapter intact while mapping each of its hooks to V2's domain APIs.
type V2ToolEvent = { tool: string; sessionID: string; id: string; input: unknown };
type V2ToolAfter = V2ToolEvent & (
  | { status: "completed"; result: { content?: string | ReadonlyArray<{ type: string; text?: string }> } }
  | { status: "error"; error: unknown }
);

type V2Services = {
  legacy?: typeof DemarkusMemoryPlugin;
  gate?: typeof callGate;
  guidance?: typeof callGuidance;
  nudge?: typeof callNudge;
};

/** Registers V2 memory hooks without persisting guidance in user prompts. */
export async function setupV2(ctx: Plugin.Context, services: V2Services = {}): Promise<() => void> {
  await registerPrompts(ctx, ASSETS);
  const directory = ctx.location.directory;
  // Reuse the V1 MCP builder so local and joined memories stay in sync.
  const legacy = await (services.legacy ?? DemarkusMemoryPlugin)({
    directory,
    client: { tui: { showToast: async ({ body }) => {
      const message = `[demarkus-memory] ${body.message}`;
      if (body.variant === "warning") console.error(message);
      else console.info(message);
    } } },
  });
  const sessions = new Map<string, SessionState>();
  const pendingWarns = new Map<string, string>();
  const pendingContext = new Map<string, string[]>();
  // Per session: the status and facts in it describe one session's start.
  const guidance = new Map<string, Guidance>();
  const state = (id: string): SessionState => {
    let value = sessions.get(id);
    if (!value) {
      value = { guidanceDelivered: false, idleNudged: false, changedFiles: false, journalWrite: false };
      sessions.set(id, value);
    }
    return value;
  };

  const controller = new AbortController();
  const cleanup = () => {
    controller.abort();
    sessions.clear();
    pendingWarns.clear();
    pendingContext.clear();
    guidance.clear();
    legacy.dispose();
  };
  try {
    const config: Record<string, any> = {};
    await legacy.config(config);
    await ctx.mcp.transform((editor) => {
      for (const [name, server] of Object.entries(config.mcp ?? {})) {
        if (!editor.get(name)) {
          const { enabled: _enabled, ...rest } = server as Record<string, unknown>;
          editor.set(name, rest as Parameters<typeof editor.set>[1]);
        }
      }
    });
    await ctx.session.hook("prompt", async (event) => {
      if (controller.signal.aborted) return;
      const recall = await (services.nudge ?? callNudge)({ event: "recall", surface: "memory", prompt: event.prompt.text });
      if (recall && !controller.signal.aborted) pendingContext.set(event.sessionID, [...(pendingContext.get(event.sessionID) ?? []), recall]);
    });
    await ctx.session.hook("context", async (event) => {
      if (controller.signal.aborted) return;
      let g = guidance.get(event.sessionID);
      if (g === undefined) {
        // null = binary not ready; retry on the next context build.
        const fresh = await (services.guidance ?? callGuidance)(directory);
        if (controller.signal.aborted) return;
        if (fresh !== null) {
          guidance.set(event.sessionID, fresh);
          g = fresh;
          if (fresh.status) console.info(fresh.status);
        }
      }
      if (g?.context) event.system.push({ type: "text", text: g.context });
      for (const text of pendingContext.get(event.sessionID) ?? []) event.system.push({ type: "text", text });
      pendingContext.delete(event.sessionID);
    });
    await ctx.tool.hook("execute.before", async (event: V2ToolEvent) => {
      if (!isMemoryWrite(event.tool) || controller.signal.aborted) return;
      const decision = await (services.gate ?? callGate)(event.tool, (event.input ?? {}) as Record<string, unknown>, directory);
      if (controller.signal.aborted) return;
      if (decision.decision === "block") throw new Error(decision.reason ?? "demarkus-memory gate blocked this write");
      if (decision.decision === "ask") throw new Error(`${decision.reason ?? "blocked"} Confirm with the user before retrying this write.`);
      if (decision.decision === "warn" && decision.reason) pendingWarns.set(`${event.sessionID}:${event.id}`, decision.reason);
    });
    await ctx.tool.hook("execute.after", async (event: V2ToolAfter) => {
      if (controller.signal.aborted) return;
      const key = `${event.sessionID}:${event.id}`;
      const warn = pendingWarns.get(key);
      pendingWarns.delete(key);
      if (event.status !== "completed") return;
      const s = state(event.sessionID);
      if (!trackActivity(s, event.tool, event.input as Record<string, unknown> | undefined)) return;
      const nudge = await (services.nudge ?? callNudge)({ event: "promote", tool: event.tool, input: event.input ?? {} });
      if (controller.signal.aborted) return;
      const additions = [warn && `⚠️ ${warn}`, nudge].filter(Boolean).join("\n\n");
      if (!additions) return;
      const content = event.result.content;
      event.result = {
        ...event.result,
        content: typeof content === "string"
          ? `${content}\n\n${additions}`
          : [...(content ?? []), { type: "text", text: additions }],
      };
    });
  } catch (error) {
    cleanup();
    throw error;
  }
  void (async () => {
    try {
      for await (const event of ctx.event.subscribe({ signal: controller.signal })) {
        if (controller.signal.aborted) return;
        if (event.type === "session.created") {
          await legacy.event({ event: { type: event.type } });
          continue;
        }
        if (event.type === "session.deleted") {
          const id = event.data?.sessionID;
          if (id) {
            sessions.delete(id);
            pendingContext.delete(id);
            guidance.delete(id);
            for (const key of pendingWarns.keys()) if (key.startsWith(`${id}:`)) pendingWarns.delete(key);
          }
          continue;
        }
        if (event.type !== "session.idle" || !event.data?.sessionID) continue;
        const s = state(event.data.sessionID);
        if (s.idleNudged || !s.changedFiles || s.journalWrite) continue;
        s.idleNudged = true;
        const nudge = await (services.nudge ?? callNudge)({ event: "session-end", cwd: directory, changedFiles: true, journalWrite: false });
        if (nudge && !controller.signal.aborted) pendingContext.set(event.data.sessionID, [...(pendingContext.get(event.data.sessionID) ?? []), nudge]);
      }
    } catch (error) {
      if (!controller.signal.aborted) console.error(`[demarkus-memory] event subscription failed: ${error}`);
    }
  })();
  return cleanup;
}

export default {
  id: "demarkus-memory",
  setup: (ctx: Plugin.Context) => {
    assertPackageInstall();
    return setupV2(ctx);
  },
  server: DemarkusMemoryPlugin,
} satisfies Plugin.Plugin & { server: typeof DemarkusMemoryPlugin };
