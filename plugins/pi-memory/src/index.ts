// demarkus-memory — pi extension.
//
// The pi port of the claude-code demarkus-memory plugin. Same behavior, mapped
// onto pi's extension API:
//   - session_start      → provision the managed server + wire the MCP entry,
//                          plus a notify-only update check (throttled, daily)
//   - before_agent_start → inject standing guidance (once) + recall nudge
//   - tool_call          → publish tag-gate + destination gate + promote nudge
//   - session_shutdown   → journal nudge
//   - registerCommand    → generated /soul-* and /promote prompts
//
// The shared demarkus-plugin binary owns gate, nudge, and guidance decisions.
// This adapter maps Pi events and shares ~/.demarkus state with other harnesses.

import { readFileSync, readdirSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { SessionActivity } from "./nudges.js";
import { callGate, callGuidance, callNudge, callUpdateCheck } from "./plugin.js";
import { ensureMcpServerEntry, provisionServer } from "./setup.js";

// Minimal structural types — pi's ExtensionAPI is provided at runtime; we keep
// these local so the extension type-checks without the (optional) peer dep.
interface UI {
  notify(message: string, level?: "info" | "warning" | "error"): void;
}
interface ExtensionContext {
  cwd: string;
  ui: UI;
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
interface ExtensionAPI {
  on(
    event: "session_start",
    handler: (event: { reason: string }, ctx: ExtensionContext) => void | Promise<void>,
  ): void;
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
  on(event: "session_shutdown", handler: (event: unknown, ctx: ExtensionContext) => void | Promise<void>): void;
  registerCommand(
    name: string,
    spec: { description: string; handler: (args: string, ctx: ExtensionContext) => void | Promise<void> },
  ): void;
  sendMessage(message: { customType: string; content: string; display: boolean }, opts?: { triggerTurn?: boolean }): void;
}

const HERE = dirname(fileURLToPath(import.meta.url));
const COMMANDS_DIR = join(HERE, "..", "commands");
const SCRIPTS_DIR = join(HERE, "..", "scripts");
const GUIDANCE_FILE = join(HERE, "..", "context", "session-guidance.md");
const MANIFEST = join(HERE, "..", "package.json");
const PLUGIN_NAME = "demarkus-pi-memory";
const MANIFEST_URL = "https://raw.githubusercontent.com/latebit-io/demarkus/main/plugins/pi-memory/package.json";
const UPDATE_COMMAND = "pi update git:github.com/latebit-io/demarkus-pi-memory (or re-run pi install from an updated checkout)";
const CUSTOM = "demarkus-memory";

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
      const description = frontmatter?.[1].match(/^description:\s*(.+)$/m)?.[1]?.trim();
      if (!description) throw new Error(`command '${file}' has no frontmatter description`);
      return { name: file.replace(/\.md$/, ""), description };
    });
}

// Resolve the command template once, substituting arguments instead of
// appending a second copy when the source already contains $ARGUMENTS.
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
// Unwrap that to the underlying tool name + parsed args so the gates see the
// real publish/append call. Direct (non-proxied) tool calls pass through unchanged.
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

// The plugin's own version, from the manifest pi installed alongside src/.
async function checkForUpdate(): Promise<string> {
  let installed = "";
  try {
    installed = (JSON.parse(readFileSync(MANIFEST, "utf8")) as { version?: string }).version ?? "";
  } catch (e) {
    console.error(`[demarkus-memory] update check: unreadable manifest ${MANIFEST}: ${e}`);
    return "";
  }
  if (!installed) return "";
  return callUpdateCheck({
    plugin: PLUGIN_NAME,
    installed,
    manifestUrl: MANIFEST_URL,
    updateCommand: UPDATE_COMMAND,
  });
}

export default function demarkusMemoryExtension(pi: ExtensionAPI): void {
  let contextDelivered = false;
  let activity = new SessionActivity();

  pi.on("session_start", async (_event, ctx) => {
    contextDelivered = false;
    activity = new SessionActivity();

    // Provision first (this installs demarkus-plugin), then register the MCP
    // server via the binary.
    const warning = await provisionServer();
    if (warning) ctx.ui.notify(warning, "warning");
    const mcp = ensureMcpServerEntry();
    if (mcp.status === "error") {
      ctx.ui.notify(`demarkus-memory: MCP registration failed: ${mcp.message}`, "warning");
    }

    // After provisioning, which is what installs the binary this call needs.
    // Fire-and-forget: a notice must never delay the session.
    void checkForUpdate().then((message) => {
      if (message) ctx.ui.notify(message);
    });
  });

  pi.on("before_agent_start", async (event) => {
    const parts: string[] = [];
    if (!contextDelivered) {
      const context = await callGuidance("memory", GUIDANCE_FILE);
      // null = binary failed; leave the flag unset so we retry next turn rather
      // than dropping guidance for the whole session. "" = ran, nothing to say.
      if (context !== null) {
        if (context) parts.push(context);
        contextDelivered = true;
      }
    }
    const recall = await callNudge({ event: "recall", surface: "memory", prompt: event.prompt ?? "" });
    if (recall) parts.push(recall);

    if (parts.length === 0) return undefined;
    return { message: { customType: CUSTOM, content: parts.join("\n\n"), display: false } };
  });

  pi.on("tool_call", async (event, ctx) => {
    const { toolName, input } = normalizeToolCall(event);

    // The shared demarkus-plugin binary owns the gate decision (publish tag-gate
    // + destination gate), so the logic lives in one place for every harness.
    const decision = await callGate(toolName, input, ctx.cwd);
    if (decision.decision === "block" || decision.decision === "ask") {
      // pi's tool_call has no native "ask"; treat it as a block whose reason
      // tells the agent to confirm with the user first.
      const reason =
        decision.decision === "ask"
          ? `${decision.reason ?? "blocked"} Confirm with the user before proceeding.`
          : (decision.reason ?? "blocked");
      return { block: true, reason };
    }

    // Record activity only for calls that survive the gate, so a blocked
    // publish/append doesn't suppress the session-end journal nudge.
    activity.observe(toolName, input);

    // warn → allow + surface a reminder.
    if (decision.decision === "warn" && decision.reason) {
      pi.sendMessage({ customType: CUSTOM, content: `⚠️ ${decision.reason}`, display: false }, { triggerTurn: false });
    }

    // Promote nudge on a fresh high-signal ADR publish (allowed call).
    const nudge = await callNudge({ event: "promote", tool: toolName, input });
    if (nudge) pi.sendMessage({ customType: CUSTOM, content: nudge, display: false }, { triggerTurn: false });

    return undefined;
  });

  pi.on("session_shutdown", async (_event, ctx) => {
    const nudge = await callNudge({
      event: "session-end",
      changedFiles: activity.changedFiles,
      soulWrite: activity.hadSoulWrite,
    });
    if (nudge) ctx.ui.notify(nudge, "info");
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
          console.error(`[demarkus-memory] ${message}`);
          ctx.ui.notify(`demarkus-memory: ${message}`, "error");
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
