// demarkus-memory — the OpenCode port of the claude-code plugin (hook mapping
// in README.md). Single self-contained file: OpenCode dir-loads it flat, and
// install.sh places its assets under ~/.demarkus/opencode-memory/. All
// gate/nudge/guidance logic lives in the shared demarkus-plugin binary; this
// adapter fails open when the binary is absent (pre-provisioning).

import { execFile } from "node:child_process";
import { randomUUID } from "node:crypto";
import { existsSync, readdirSync, readFileSync } from "node:fs";
import { homedir } from "node:os";
import { join } from "node:path";

const BIN = join(homedir(), ".demarkus", "bin", "demarkus-plugin");
const ASSETS = join(homedir(), ".demarkus", "opencode-memory");
const COMMANDS_DIR = join(ASSETS, "commands");
const SCRIPTS_DIR = join(ASSETS, "scripts");
const GUIDANCE_FILE = join(ASSETS, "context", "session-guidance.md");
const MANIFEST = join(ASSETS, "package.json");
const MCP_SERVER_NAME = "demarkus-memory";
const PLUGIN_NAME = "demarkus-opencode-memory";
const RAW_BASE = "https://raw.githubusercontent.com/latebit-io/demarkus/main/plugins/opencode-memory";
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
function runBin<T>(args: string[], payload?: unknown, allowEmpty = false): Promise<T | null> {
  return new Promise((resolve) => {
    const operation = args[0] ?? "helper";
    if (!existsSync(BIN)) {
      reportFailure(operation, `${BIN} is not installed; this call is allowed to continue`);
      resolve(null);
      return;
    }
    const child = execFile(BIN, args, { encoding: "utf8", timeout: 5000 }, (err, stdout) => {
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

async function callGate(toolName: string, input: Record<string, unknown>, cwd: string): Promise<GateDecision> {
  const d = await runBin<GateDecision>(["gate"], { tool: toolName, input, cwd });
  return d && typeof d.decision === "string" ? d : { decision: "allow" };
}

async function callNudge(req: Record<string, unknown>): Promise<string> {
  const o = await runBin<{ nudge?: string }>(["nudge"], req, true);
  return o?.nudge ?? "";
}

// null = binary unavailable/failed (caller retries next turn); "" = ran, nothing to say.
async function callGuidance(): Promise<string | null> {
  const o = await runBin<{ context?: string }>(
    ["guidance", "--surface", "memory", "--guidance-file", GUIDANCE_FILE],
    undefined,
    true,
  );
  return o === null ? null : (o.context ?? "");
}

function isSoulWrite(toolName: string): boolean {
  return /mark_(publish|append)$/.test(toolName);
}

function isFileMutation(toolName: string): boolean {
  return /^(edit|write|patch|multiedit)$/i.test(toolName);
}

// Per-session adapter state: one guidance injection, one idle nudge, and the
// two session-end signals (files changed / memory written).
interface SessionState {
  guidanceDelivered: boolean;
  idleNudged: boolean;
  changedFiles: boolean;
  soulWrite: boolean;
}

// run executes a provisioning step; resolves "" on success, a warning on failure.
function run(cmd: string, args: string[], label: string, timeout: number): Promise<string> {
  return new Promise((resolve) => {
    execFile(cmd, args, { timeout }, (err, _out, stderr) => {
      if (err) {
        const tail = (stderr || "").trim().split("\n").slice(-3).join(" ");
        resolve(`demarkus-memory ${label} failed: ${tail || err.message}. Run /memory-init to recover.`);
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

// Update notice. The binary owns the throttle, fetch, and comparison; the
// adapter owns its release identity and where its version is recorded (the
// manifest install.sh copied alongside the assets).
async function checkForUpdate(): Promise<string> {
  let installed = "";
  try {
    installed = (JSON.parse(readFileSync(MANIFEST, "utf8")) as { version?: string }).version ?? "";
  } catch (e) {
    // ENOENT = installed before the manifest was shipped; anything else is real.
    if ((e as NodeJS.ErrnoException)?.code !== "ENOENT") {
      console.error(`[demarkus-memory] update check: unreadable manifest ${MANIFEST}: ${e}`);
    }
    return "";
  }
  if (!installed) return "";
  const out = await runBin<{ message?: string }>([
    "update-check",
    "--plugin", PLUGIN_NAME,
    "--installed", installed,
    "--manifest-url", `${RAW_BASE}/package.json`,
    "--update-command", `curl -fsSL ${RAW_BASE}/install.sh | bash`,
  ], undefined, true);
  return out?.message ?? "";
}

// Load a command markdown: parse the frontmatter description, strip the fence,
// resolve ${DEMARKUS_SCRIPTS} to the installed scripts dir.
export function loadCommand(file: string, commandsDir = COMMANDS_DIR): { description: string; template: string } {
  const raw = readFileSync(join(commandsDir, file), "utf8");
  const fm = raw.match(/^---\n([\s\S]*?)\n---\n/);
  const description = fm?.[1].match(/^description:\s*(.+)$/m)?.[1]?.trim() ?? "";
  const body = (fm ? raw.slice(fm[0].length) : raw).replace(/\$\{DEMARKUS_SCRIPTS\}/g, SCRIPTS_DIR).trim();
  // $ARGUMENTS lets the user pass arguments; harmless when empty.
  const template = `${body}\n\nUser arguments (may be empty): $ARGUMENTS\n`;
  return { description, template };
}

// Minimal structural types: OpenCode provides the real ones at runtime; keeping
// them local avoids a dependency on @opencode-ai/plugin for a dir-dropped file.
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
  const sessions = new Map<string, SessionState>();
  // Warn reasons carried from the before-hook to the after-hook (block/ask
  // never reach after: before throws), so the gate subprocess runs once per
  // write. Keyed sessionID:callID; entries for failed calls (no after-hook)
  // are pruned with their session on session.deleted.
  const pendingWarns = new Map<string, string>();
  const state = (id: string): SessionState => {
    let s = sessions.get(id);
    if (!s) {
      s = { guidanceDelivered: false, idleNudged: false, changedFiles: false, soulWrite: false };
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
    provisioning ??= provision()
      .then((warning) => {
        if (warning) void toast(warning, "warning");
      })
      .finally(() => {
        provisioning = null;
      });
  };
  ensureProvisioned();

  // Fire-and-forget alongside provisioning: an update notice must never delay
  // startup or block on the network.
  void checkForUpdate().then((message) => {
    if (message) void toast(message);
  });

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

      // Joined remote memories: the binary's `registry mcp add` writes pi's MCP
      // config, which OpenCode never reads, so wire every memory in the shared
      // catalog here. The binary owns the catalog format (TSV: slug host
      // insecure token-file, "#" comments); follow-up: query it via a
      // `registry soul-list` subcommand instead of parsing the file.
      try {
        const memories = readFileSync(join(homedir(), ".demarkus", "souls"), "utf8");
        for (const rawLine of memories.split("\n")) {
          const line = rawLine.trim();
          if (line.startsWith("#")) continue;
          const slug = line.split("\t")[0]?.trim();
          if (!slug || slug === MCP_SERVER_NAME) continue;
          config.mcp[slug] = config.mcp[slug] ?? {
            type: "local",
            command: [BIN, "mcp-serve", "--soul", slug], // pinned binary predates --memory
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
      let files: string[] = [];
      try {
        files = readdirSync(COMMANDS_DIR).filter((f) => f.endsWith(".md"));
      } catch (e) {
        console.error(`[demarkus-memory] commands unavailable (${ASSETS} not installed?): ${e}`);
      }
      for (const f of files) {
        const name = f.replace(/\.md$/, "");
        try {
          config.command[name] = config.command[name] ?? loadCommand(f);
        } catch (e) {
          console.error(`[demarkus-memory] failed to load command '${name}': ${e}`);
        }
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

      const [context, recall] = await Promise.all([
        s.guidanceDelivered ? Promise.resolve("") : callGuidance(),
        callNudge({ event: "recall", surface: "memory", prompt: promptText }),
      ]);

      const texts: string[] = [];
      // null = binary not ready; retry next turn instead of burning the flag.
      if (context !== null && !s.guidanceDelivered) {
        if (context) texts.push(context);
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
      if (!isSoulWrite(input.tool) || !adapters.has("memory")) return;
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
      if (!isSoulWrite(input.tool)) {
        if (isFileMutation(input.tool)) s.changedFiles = true;
        return;
      }
      s.soulWrite = true;

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
      if (s.idleNudged || !s.changedFiles || s.soulWrite) return;
      s.idleNudged = true;
      const nudge = await callNudge({
        event: "session-end",
        changedFiles: s.changedFiles,
        soulWrite: s.soulWrite,
      });
      if (nudge) await toast(nudge);
    },
  };
};

export default DemarkusMemoryPlugin;
