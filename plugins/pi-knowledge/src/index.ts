// demarkus-knowledge — pi extension.
//
// The pi port of the claude-code demarkus-knowledge plugin. A knowledge system
// is an organizational, broker-fronted demarkus catalog reached over HTTPS via
// pi-mcp-adapter's OAuth — this extension owns no server and no binaries. It:
//   - before_agent_start → injects standing guidance (once) + a recall nudge
//   - tool_call          → publish tag-gate (tags + importance + required axes /
//                          fields) on writes to a joined knowledge system
//   - registerCommand    → /knowledge, /knowledge-join, /knowledge-doctor
//
// All gate/nudge/guidance LOGIC is native TypeScript; URL validation, registry,
// and policy mirroring reuse the bundled bash (scripts/*.sh).

import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { callGate, callGuidance, callNudge } from "./plugin.js";

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

const COMMANDS: Array<{ name: string; description: string }> = [
  { name: "knowledge", description: "List joined knowledge systems and show each one's root hub index" },
  { name: "knowledge-join", description: "Join an organizational demarkus knowledge system (broker + OAuth)" },
  { name: "knowledge-doctor", description: "Audit a joined knowledge system for catalog hygiene (read-only)" },
];

function commandBody(name: string): string {
  const raw = readFileSync(join(COMMANDS_DIR, `${name}.md`), "utf8");
  return raw
    .replace(/^---\n[\s\S]*?\n---\n/, "")
    .replace(/\$\{DEMARKUS_SCRIPTS\}/g, SCRIPTS_DIR)
    .trim();
}

// pi-mcp-adapter routes MCP calls through a proxy tool named "mcp":
//   { toolName: "mcp", input: { tool: "<prefixed name>", args: <json-string|object>, server?: string } }
// Unwrap that to the underlying tool name + parsed args so the gate sees the
// real publish call. Direct (non-proxied) tool calls pass through unchanged.
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

export default function demarkusKnowledgeExtension(pi: ExtensionAPI): void {
  let contextDelivered = false;

  pi.on("session_start", () => {
    contextDelivered = false;
  });

  pi.on("before_agent_start", async (event) => {
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

  pi.on("tool_call", async (event) => {
    const { toolName, input } = normalizeToolCall(event);
    // Shared decision via the demarkus-plugin binary (knowledge tag/axes/fields gate).
    const decision = await callGate(toolName, input, "");
    if (decision.decision === "block" || decision.decision === "ask") {
      const reason =
        decision.decision === "ask"
          ? `${decision.reason ?? "blocked"} Confirm with the user before proceeding.`
          : (decision.reason ?? "blocked");
      return { block: true, reason };
    }
    if (decision.decision === "warn" && decision.reason) {
      pi.sendMessage({ customType: CUSTOM, content: `⚠️ ${decision.reason}`, display: false }, { triggerTurn: false });
    }
    return undefined;
  });

  for (const { name, description } of COMMANDS) {
    pi.registerCommand(name, {
      description,
      handler: (args, ctx) => {
        let body: string;
        try {
          body = commandBody(name);
        } catch {
          ctx.ui.notify(`demarkus-knowledge: command '${name}' not found`, "error");
          return;
        }
        const content = args && args.trim() ? `${body}\n\n---\nUser arguments: ${args.trim()}` : body;
        // triggerTurn: the command injects the skill body and must start a turn
        // so the agent acts on it. Without it, an idle session just appends the
        // message to history and never runs (the command appears to do nothing).
        pi.sendMessage({ customType: `${CUSTOM}-${name}`, content, display: false }, { triggerTurn: true });
      },
    });
  }
}
