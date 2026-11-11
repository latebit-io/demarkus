// demarkus-knowledge — pi extension.
//
// The pi port of the claude-code demarkus-knowledge plugin. A knowledge system
// is an organizational, broker-fronted demarkus catalog reached over HTTPS with
// OAuth (pi's built-in MCP, or pi-mcp-adapter). It owns no server; the shared
// helper supplies policy. It:
//   - session_start      → connects the joined systems when pi's built-in MCP runs
//   - before_agent_start → injects standing guidance (once) + a recall nudge,
//                          and connects a system joined since the last turn
//   - tool_call          → publish tag-gate (tags + importance + required axes /
//                          fields) on writes to a joined knowledge system
//   - registerCommand    → /knowledge, /knowledge-join, /knowledge-doctor
//
// The shared demarkus-plugin binary owns gate, nudge, and guidance decisions.
// This adapter maps Pi events and registration onto that shared behavior.

import { readFileSync, readdirSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { NativeServers, piConfigs, type McpApi } from "./native.js";
import { callGate, callGuidance, callNudge } from "./plugin.js";
import { branchTaint, recordTaint, taintStamp } from "./taint.js";

interface UI {
  notify(message: string, level?: "info" | "warning" | "error"): void;
  confirm?(title: string, message: string): Promise<boolean>;
}
interface ExtensionContext {
  cwd: string;
  ui: UI;
  // Dialogs work (TUI, RPC); false in print and JSON modes.
  hasUI?: boolean;
  isProjectTrusted?(): boolean;
  sessionManager?: { getBranch(): unknown[] };
}
interface ToolCallEvent {
  toolName: string;
  input: Record<string, unknown>;
}
interface BeforeAgentStartEvent {
  prompt: string;
  systemPrompt: string;
}
type MessageResult = { message: { customType: string; content: string; display: boolean } };
interface ExtensionAPI extends McpApi {
  on(event: "session_start", handler: (event: { reason: string }, ctx: ExtensionContext) => void | Promise<void>): void;
  on(
    event: "before_agent_start",
    handler: (
      event: BeforeAgentStartEvent,
      ctx: ExtensionContext,
    ) => MessageResult | undefined | Promise<MessageResult | undefined>,
  ): void;
  on(
    event: "tool_call",
    handler: (
      event: ToolCallEvent,
      ctx: ExtensionContext,
    ) => { block: true; reason: string } | undefined | Promise<{ block: true; reason: string } | undefined>,
  ): void;
  registerCommand(
    name: string,
    spec: { description: string; handler: (args: string, ctx: ExtensionContext) => void },
  ): void;
  sendMessage(message: { customType: string; content: string; display: boolean }, opts?: { triggerTurn?: boolean }): void;
}

const HERE = dirname(fileURLToPath(import.meta.url));
const COMMANDS_DIR = join(HERE, "..", "commands");
const SCRIPTS_DIR = join(HERE, "..", "scripts");
const GUIDANCE_FILE = join(HERE, "..", "context", "session-guidance.md");
const CUSTOM = "demarkus-knowledge";

interface Command {
  name: string;
  description: string;
}

function commands(): Command[] {
  return readdirSync(COMMANDS_DIR)
    .filter((file) => file.endsWith(".md"))
    .sort()
    .map((file) => {
      const raw = readFileSync(join(COMMANDS_DIR, file), "utf8");
      const frontmatter = raw.match(/^---\n([\s\S]*?)\n---\n/);
      const description = frontmatter?.[1].match(/^description:\s*(.+)$/m)?.[1]?.trim().replace(/^(["'])(.*)\1$/, "$2");
      if (!description) throw new Error(`command '${file}' has no frontmatter description`);
      return { name: file.replace(/\.md$/, ""), description };
    });
}

function commandBody(name: string, args: string): string {
  const raw = readFileSync(join(COMMANDS_DIR, `${name}.md`), "utf8");
  const body = raw
    .replace(/^---\n[\s\S]*?\n---\n/, "")
    .replace(/\$\{DEMARKUS_SCRIPTS\}/g, SCRIPTS_DIR)
    .trim();
  const value = args.trim();
  if (body.includes("$ARGUMENTS")) return body.replaceAll("$ARGUMENTS", () => value);
  return value ? `${body}\n\nUser arguments: ${value}` : body;
}

// pi-mcp-adapter routes MCP calls through a proxy tool named "mcp":
//   { toolName: "mcp", input: { tool: "<prefixed name>", args: <json-string|object>, server?: string } }
// Unwrap that to the underlying tool name + parsed args so the gate sees the
// real publish call. Pi's built-in MCP calls mcp__<server>__<tool> directly,
// which passes through unchanged.
function normalizeToolCall(event: ToolCallEvent): { toolName: string; input: Record<string, unknown> } {
  let toolName = event.toolName;
  let input: Record<string, unknown> = event.input ?? {};
  if (toolName === "mcp" && typeof input.tool === "string") {
    toolName = input.tool;
    const rawArgs = input.args;
    if (typeof rawArgs === "string") {
      try {
        input = JSON.parse(rawArgs);
      } catch {
        input = {};
      }
    } else if (rawArgs && typeof rawArgs === "object") {
      input = rawArgs as Record<string, unknown>;
    } else {
      input = {};
    }
  }
  return { toolName, input };
}

type Verdict = { block: true; reason: string } | undefined;

// ask settles a gate ask: a confirm dialog stands in for a native ask where
// pi has a UI; elsewhere it blocks with a reason routed through the user. An
// approved publish carries the taint stamp, as on Claude Code.
async function ask(event: ToolCallEvent, ctx: ExtensionContext, toolName: string, input: Record<string, unknown>, reason: string, taint: string[]): Promise<Verdict> {
  if (!ctx.hasUI || !ctx.ui.confirm) return { block: true, reason: `${reason} Confirm with the user before proceeding.` };
  if (!(await ctx.ui.confirm("Approve this demarkus write?", reason))) {
    return { block: true, reason: "The user declined this demarkus write; do not retry it unchanged." };
  }
  const metadata = taintStamp(toolName, input, taint);
  if (metadata) {
    // input is event.input or the proxy's args; string args were parsed into a copy.
    input.metadata = metadata;
    if (event.toolName === "mcp" && typeof event.input.args === "string") event.input.args = JSON.stringify(input);
  }
  return undefined;
}

// Both Pi plugins see every tool call; with memory loaded it owns the gate,
// so one write asks once (OpenCode's ownsGate).
const GATE_ADAPTERS = Symbol.for("io.demarkus.pi.gate-adapters");
function gateAdapters(): Set<string> {
  const root = globalThis as typeof globalThis & { [key: symbol]: Set<string> | undefined };
  return (root[GATE_ADAPTERS] ??= new Set<string>());
}

export default function demarkusKnowledgeExtension(pi: ExtensionAPI): void {
  gateAdapters().add("knowledge");
  let contextDelivered = false;
  // Untrusted reads this session, for the gate's taint guard.
  let taint: string[] = [];
  const native = new NativeServers("knowledge");
  const report = (ctx: ExtensionContext, errors: string[]) => {
    for (const error of errors) ctx.ui.notify(`demarkus-knowledge: MCP server not connected: ${error}`, "warning");
  };
  // Pi's own mcp.json files, whose servers override the ones registered here.
  const piFiles = (ctx: ExtensionContext) => piConfigs(ctx.cwd, ctx.isProjectTrusted?.() ?? false);

  // A resumed, reloaded or forked branch keeps the reads it made; a new one is empty.
  pi.on("session_start", (_event, ctx) => {
    contextDelivered = false;
    taint = branchTaint(ctx.sessionManager?.getBranch() ?? [], (toolName, input) => normalizeToolCall({ toolName, input }));
    report(ctx, native.start(pi, piFiles(ctx)));
  });

  pi.on("before_agent_start", async (event, ctx) => {
    // A /knowledge-join since the last turn connects here, with no restart.
    report(ctx, native.sync(pi, piFiles(ctx)));
    const parts: string[] = [];
    if (!contextDelivered) {
      const context = await callGuidance("knowledge", GUIDANCE_FILE);
      // null = binary failed; leave the flag unset so we retry next turn rather
      // than dropping guidance for the whole session. "" = ran, nothing to say.
      if (context !== null) {
        if (context) parts.push(context);
        contextDelivered = true;
      }
    }
    const recall = await callNudge({ event: "recall", surface: "knowledge", prompt: event.prompt ?? "" });
    if (recall) parts.push(recall);

    if (parts.length === 0) return undefined;
    return { message: { customType: CUSTOM, content: parts.join("\n\n"), display: false } };
  });

  pi.on("tool_call", async (event, ctx) => {
    const { toolName, input } = normalizeToolCall(event);
    if (gateAdapters().has("memory")) {
      recordTaint(taint, toolName, input);
      return undefined;
    }
    // Shared decision via the demarkus-plugin binary (knowledge tag/axes/fields gate).
    const decision = await callGate(toolName, input, "", taint);
    if (decision.decision === "block") return { block: true, reason: decision.reason ?? "blocked" };
    if (decision.decision === "ask") {
      const verdict = await ask(event, ctx, toolName, input, decision.reason ?? "demarkus write needs approval.", taint);
      if (verdict) return verdict;
    }
    recordTaint(taint, toolName, input);
    if (decision.decision === "warn" && decision.reason) {
      pi.sendMessage({ customType: CUSTOM, content: `⚠️ ${decision.reason}`, display: false }, { triggerTurn: false });
    }
    return undefined;
  });

  for (const { name, description } of commands()) {
    pi.registerCommand(name, {
      description,
      handler: (args, ctx) => {
        let body: string;
        try {
          body = commandBody(name, args);
        } catch (error) {
          const missing = (error as NodeJS.ErrnoException).code === "ENOENT";
          const message = missing ? `command '${name}' not found` : `command '${name}' failed to load: ${error}`;
          console.error(`[demarkus-knowledge] ${message}`);
          ctx.ui.notify(`demarkus-knowledge: ${message}`, "error");
          return;
        }
        // triggerTurn: the command injects the skill body and must start a turn
        // so the agent acts on it. Without it, an idle session just appends the
        // message to history and never runs (the command appears to do nothing).
        pi.sendMessage({ customType: `${CUSTOM}-${name}`, content: body, display: false }, { triggerTurn: true });
      },
    });
  }
}
