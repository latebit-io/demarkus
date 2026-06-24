// demarkus-memory — pi extension.
//
// The pi port of the claude-code demarkus-memory plugin. Same behavior, mapped
// onto pi's extension API:
//   - session_start      → provision the managed server + wire the MCP entry
//   - before_agent_start → inject standing guidance (once) + recall nudge
//   - tool_call          → publish tag-gate + destination gate + promote nudge
//   - session_shutdown   → journal nudge
//   - registerCommand    → the /soul-* and /promote slash commands (each runs
//                          the bundled skill of the same name)
//
// All gate/nudge/guidance LOGIC is native TypeScript (src/*.ts). Server
// provisioning + registry mutation reuse the bundled bash (scripts/*.sh), which
// shares ~/.demarkus state with the claude-code plugin.

import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { destinationGate, publishTagGate, type GateDecision } from "./gates.js";
import { buildSessionContext } from "./guidance.js";
import { promoteNudge, recallNudge, SessionActivity } from "./nudges.js";
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
const CUSTOM = "demarkus-memory";

// Slash commands → bundled skills. Each command injects its skill's body so the
// instructions live in exactly one place (the SKILL.md).
const COMMANDS: Array<{ name: string; description: string }> = [
  { name: "soul", description: "Show the local demarkus soul index" },
  { name: "soul-context", description: "Restore session context from the current project's soul" },
  { name: "soul-journal", description: "Append an entry to today's journal for the current project" },
  { name: "soul-init", description: "Detect and configure the demarkus-memory soul" },
  { name: "soul-join", description: "Join a remote demarkus soul and bind it to this project" },
  { name: "soul-default", description: "Set this project's default demarkus write target" },
  { name: "soul-status", description: "Show the demarkus-memory connection state and verify it's healthy" },
  { name: "soul-doctor", description: "Audit the soul for catalog hygiene (read-only)" },
  { name: "soul-refresh", description: "Refresh promoted soul documents from the knowledge system" },
  { name: "promote", description: "Promote a soul document up to a shared knowledge system" },
  { name: "promote-scan", description: "Sweep the soul for promotion candidates" },
];

// Load a command prompt body: strip its YAML frontmatter and resolve the
// ${DEMARKUS_SCRIPTS} token to the bundled scripts directory's absolute path.
function commandBody(name: string): string {
  const raw = readFileSync(join(COMMANDS_DIR, `${name}.md`), "utf8");
  return raw
    .replace(/^---\n[\s\S]*?\n---\n/, "")
    .replace(/\$\{DEMARKUS_SCRIPTS\}/g, SCRIPTS_DIR)
    .trim();
}

export default function demarkusMemoryExtension(pi: ExtensionAPI): void {
  let contextDelivered = false;
  let activity = new SessionActivity();

  pi.on("session_start", async (_event, ctx) => {
    contextDelivered = false;
    activity = new SessionActivity();

    ensureMcpServerEntry();
    const warning = await provisionServer();
    if (warning) ctx.ui.notify(warning, "warning");
  });

  pi.on("before_agent_start", async (event) => {
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

  pi.on("tool_call", async (event, ctx) => {
    activity.observe(event.toolName);

    const input = event.input ?? {};
    const decisions: GateDecision[] = [
      destinationGate(event.toolName, input, ctx.cwd),
      publishTagGate(event.toolName, input),
    ];

    // Block on the first blocking decision (destination misroute before tag gate).
    const blocking = decisions.find((d) => d.action === "block");
    if (blocking && blocking.action === "block") {
      return { block: true, reason: blocking.reason };
    }

    // Non-blocking warnings surface as injected reminders.
    for (const d of decisions) {
      if (d.action === "warn") {
        pi.sendMessage({ customType: CUSTOM, content: d.reason, display: false }, { triggerTurn: false });
      }
    }

    // Promote nudge on a fresh high-signal ADR publish (allowed call).
    const nudge = promoteNudge(event.toolName, input);
    if (nudge) pi.sendMessage({ customType: CUSTOM, content: nudge, display: false }, { triggerTurn: false });

    return undefined;
  });

  pi.on("session_shutdown", (_event, ctx) => {
    const nudge = activity.journalNudge();
    if (nudge) ctx.ui.notify(nudge, "info");
  });

  for (const { name, description } of COMMANDS) {
    pi.registerCommand(name, {
      description,
      handler: (args, ctx) => {
        let body: string;
        try {
          body = commandBody(name);
        } catch {
          ctx.ui.notify(`demarkus-memory: command '${name}' not found`, "error");
          return;
        }
        const content = args && args.trim() ? `${body}\n\n---\nUser arguments: ${args.trim()}` : body;
        pi.sendMessage({ customType: `${CUSTOM}-${name}`, content, display: false });
      },
    });
  }
}
