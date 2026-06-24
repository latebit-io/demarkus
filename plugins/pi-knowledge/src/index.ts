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
import { publishGate } from "./gate.js";
import { buildSessionContext } from "./guidance.js";
import { recallNudge } from "./nudges.js";

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
  on(event: "session_start", handler: (event: { reason: string }, ctx: ExtensionContext) => void): void;
  on(
    event: "before_agent_start",
    handler: (event: BeforeAgentStartEvent, ctx: ExtensionContext) => MessageResult | undefined,
  ): void;
  on(
    event: "tool_call",
    handler: (event: ToolCallEvent, ctx: ExtensionContext) => { block: true; reason: string } | undefined,
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

export default function demarkusKnowledgeExtension(pi: ExtensionAPI): void {
  let contextDelivered = false;

  pi.on("session_start", () => {
    contextDelivered = false;
  });

  pi.on("before_agent_start", (event) => {
    const parts: string[] = [];
    if (!contextDelivered) {
      const context = buildSessionContext();
      if (context) parts.push(context);
      contextDelivered = true;
    }
    const recall = recallNudge(event.prompt ?? "");
    if (recall) parts.push(recall);

    if (parts.length === 0) return undefined;
    return { message: { customType: CUSTOM, content: parts.join("\n\n"), display: false } };
  });

  pi.on("tool_call", (event) => {
    const decision = publishGate(event.toolName, event.input ?? {});
    if (decision.action === "block") return { block: true, reason: decision.reason };
    if (decision.action === "warn") {
      pi.sendMessage({ customType: CUSTOM, content: decision.reason, display: false }, { triggerTurn: false });
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
        pi.sendMessage({ customType: `${CUSTOM}-${name}`, content, display: false });
      },
    });
  }
}
