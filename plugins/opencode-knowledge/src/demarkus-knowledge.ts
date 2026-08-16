// OpenCode adapter for the broker-fronted demarkus knowledge plugin. Runtime
// policy stays in demarkus-plugin; this file maps OpenCode hooks and config.

import { execFile } from "node:child_process";
import { randomUUID } from "node:crypto";
import { existsSync, readdirSync, readFileSync } from "node:fs";
import { homedir } from "node:os";
import { join } from "node:path";

const HOME = homedir();
const BIN = join(HOME, ".demarkus", "bin", "demarkus-plugin");
const ASSETS = join(HOME, ".demarkus", "opencode-knowledge");
const COMMANDS_DIR = join(ASSETS, "commands");
const SCRIPTS_DIR = join(ASSETS, "scripts");
const GUIDANCE_FILE = join(ASSETS, "context", "session-guidance.md");
const MANIFEST = join(ASSETS, "package.json");
const KNOWLEDGE_SYSTEMS = join(HOME, ".demarkus", "knowledge-systems");
const MCP_CATALOG = join(HOME, ".config", "mcp", "mcp.json");
const PLUGIN_NAME = "demarkus-opencode-knowledge";
const RAW_BASE = "https://raw.githubusercontent.com/latebit-io/demarkus/main/plugins/opencode-knowledge";
const GATE_ADAPTERS = Symbol.for("io.demarkus.opencode.gate-adapters");

interface GateDecision {
  decision: "allow" | "warn" | "block" | "ask";
  reason?: string;
}

interface RegistryPaths {
  knowledgeSystems: string;
  mcpCatalog: string;
}

interface SessionState {
  guidanceDelivered: boolean;
}

type ToastClient = {
  tui?: { showToast?: (args: { body: { message: string; variant: string } }) => Promise<unknown> };
};

type GateGlobal = typeof globalThis & { [key: symbol]: unknown };

const reportedFailures = new Set<string>();

function reportFailure(operation: string, detail: string): void {
  const message = `${operation}: ${detail}`;
  if (reportedFailures.has(message)) return;
  reportedFailures.add(message);
  console.error(`[demarkus-knowledge] ${message}`);
}

function runBin<T>(args: string[], payload?: unknown, emptyIsNull = false): Promise<T | null> {
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
        if (!emptyIsNull) reportFailure(operation, "helper returned an empty response");
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
    if (payload !== undefined && child.stdin) child.stdin.end(JSON.stringify(payload));
  });
}

async function callGate(toolName: string, input: Record<string, unknown>, cwd: string): Promise<GateDecision> {
  const decision = await runBin<GateDecision>(["gate"], { tool: toolName, input, cwd });
  return decision && typeof decision.decision === "string" ? decision : { decision: "allow" };
}

async function callNudge(request: Record<string, unknown>): Promise<string> {
  const output = await runBin<{ nudge?: string }>(["nudge"], request);
  return output?.nudge ?? "";
}

async function callGuidance(): Promise<string | null> {
  const output = await runBin<{ context?: string }>([
    "guidance",
    "--surface",
    "knowledge",
    "--guidance-file",
    GUIDANCE_FILE,
  ]);
  return output === null ? null : (output.context ?? "");
}

function isKnowledgeWrite(toolName: string): boolean {
  return /mark_(publish|append)$/.test(toolName);
}

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

export function ownsGate(adapter: "memory" | "knowledge", adapters: Set<string>): boolean {
  if (adapters.has("memory")) return adapter === "memory";
  return adapter === "knowledge";
}

function nonCommentLines(path: string): string[] {
  return readFileSync(path, "utf8")
    .split("\n")
    .map((line) => line.trim())
    .filter((line) => line && !line.startsWith("#"));
}

function catalogServers(path: string): Record<string, unknown> {
  const parsed = JSON.parse(readFileSync(path, "utf8")) as unknown;
  if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) {
    throw new Error(`${path} is not a JSON object`);
  }
  const servers = (parsed as { mcpServers?: unknown }).mcpServers;
  if (!servers || typeof servers !== "object" || Array.isArray(servers)) {
    throw new Error(`${path}: mcpServers is not a JSON object`);
  }
  return servers as Record<string, unknown>;
}

function isReservedKnowledgeSlug(slug: string): boolean {
  const normalized = slug.toLowerCase().replaceAll("-", "_");
  return normalized === "demarkus_memory" || normalized.endsWith("_demarkus_memory");
}

export function wireKnowledgeMcp(
  mcp: Record<string, unknown>,
  paths: RegistryPaths = { knowledgeSystems: KNOWLEDGE_SYSTEMS, mcpCatalog: MCP_CATALOG },
  report: (message: string) => void = (message) => console.error(`[demarkus-knowledge] ${message}`),
): void {
  let slugs: string[];
  try {
    slugs = nonCommentLines(paths.knowledgeSystems);
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code !== "ENOENT") report(`knowledge registry unreadable: ${error}`);
    return;
  }

  let servers: Record<string, unknown> = {};
  try {
    servers = catalogServers(paths.mcpCatalog);
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code !== "ENOENT") report(`MCP endpoint catalog unreadable: ${error}`);
  }

  for (const slug of slugs) {
    if (isReservedKnowledgeSlug(slug)) {
      const entry = mcp[slug];
      if (entry && typeof entry === "object" && !Array.isArray(entry)) {
        mcp[slug] = { ...entry, enabled: false };
      } else {
        delete mcp[slug];
      }
      report(`knowledge slug '${slug}' is reserved by local-soul tool normalization and was disabled; unregister it`);
      continue;
    }
    if (mcp[slug] !== undefined) continue;
    const entry = servers[slug];
    const url = entry && typeof entry === "object" && !Array.isArray(entry) ? (entry as { url?: unknown }).url : undefined;
    if (typeof url !== "string" || !url) {
      report(`joined system '${slug}' has no MCP endpoint; rerun /knowledge-join in OpenCode`);
      continue;
    }
    mcp[slug] = { type: "remote", url, enabled: true };
  }
}

function run(command: string, args: string[], label: string, timeout: number): Promise<string> {
  return new Promise((resolve) => {
    execFile(command, args, { timeout }, (err, _stdout, stderr) => {
      if (!err) {
        resolve("");
        return;
      }
      const tail = (stderr || "").trim().split("\n").slice(-3).join(" ");
      resolve(`demarkus-knowledge ${label} failed: ${tail || err.message}. Re-run the installer to recover.`);
    });
  });
}

async function bootstrap(): Promise<string> {
  return run("bash", [join(SCRIPTS_DIR, "bootstrap.sh")], "bootstrap", 2_500_000);
}

async function checkForUpdate(): Promise<string> {
  let installed = "";
  try {
    installed = (JSON.parse(readFileSync(MANIFEST, "utf8")) as { version?: string }).version ?? "";
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code !== "ENOENT") {
      reportFailure("update check", `unreadable manifest ${MANIFEST}: ${error}`);
    }
    return "";
  }
  if (!installed) return "";
  const output = await runBin<{ message?: string }>([
    "update-check",
    "--plugin",
    PLUGIN_NAME,
    "--installed",
    installed,
    "--manifest-url",
    `${RAW_BASE}/package.json`,
    "--update-command",
    `curl -fsSL ${RAW_BASE}/install.sh | bash`,
  ], undefined, true);
  return output?.message ?? "";
}

export function loadCommand(file: string, commandsDir = COMMANDS_DIR): { description: string; template: string } {
  const raw = readFileSync(join(commandsDir, file), "utf8");
  const frontmatter = raw.match(/^---\n([\s\S]*?)\n---\n/);
  const description = frontmatter?.[1].match(/^description:\s*(.+)$/m)?.[1]?.trim() ?? "";
  const body = (frontmatter ? raw.slice(frontmatter[0].length) : raw)
    .replace(/\$\{DEMARKUS_SCRIPTS\}/g, SCRIPTS_DIR)
    .trim();
  return { description, template: `${body}\n\nUser arguments (may be empty): $ARGUMENTS\n` };
}

export const DemarkusKnowledgePlugin = async ({ client, directory }: { client: ToastClient; directory: string }) => {
  const adapters = gateAdapters(directory);
  adapters.add("knowledge");
  const knowledgeOwnsGate = () => ownsGate("knowledge", adapters);
  const sessions = new Map<string, SessionState>();
  const pendingWarns = new Map<string, string>();
  const state = (id: string): SessionState => {
    let session = sessions.get(id);
    if (!session) {
      session = { guidanceDelivered: false };
      sessions.set(id, session);
    }
    return session;
  };

  const toast = async (message: string, variant: "info" | "warning" = "info") => {
    try {
      await client.tui?.showToast?.({ body: { message, variant } });
    } catch (error) {
      console.error(`[demarkus-knowledge] toast failed: ${error}; message: ${message}`);
    }
  };

  let bootstrapping: Promise<void> | null = null;
  let updateChecked = false;
  const ensureBootstrapped = () => {
    bootstrapping ??= bootstrap()
      .then(async (warning) => {
        if (warning) {
          await toast(warning, "warning");
          return;
        }
        if (!updateChecked) {
          updateChecked = true;
          const message = await checkForUpdate();
          if (message) await toast(message);
        }
      })
      .finally(() => {
        bootstrapping = null;
      });
  };
  ensureBootstrapped();

  return {
    config: async (config: Record<string, any>) => {
      config.mcp = config.mcp ?? {};
      wireKnowledgeMcp(config.mcp);

      config.command = config.command ?? {};
      let files: string[] = [];
      try {
        files = readdirSync(COMMANDS_DIR).filter((file) => file.endsWith(".md"));
      } catch (error) {
        console.error(`[demarkus-knowledge] commands unavailable (${ASSETS} not installed?): ${error}`);
      }
      for (const file of files) {
        const name = file.replace(/\.md$/, "");
        try {
          config.command[name] = config.command[name] ?? loadCommand(file);
        } catch (error) {
          console.error(`[demarkus-knowledge] failed to load command '${name}': ${error}`);
        }
      }
    },

    "chat.message": async (
      _input: unknown,
      output: { message: { id?: string; sessionID?: string }; parts: Array<Record<string, unknown>> },
    ) => {
      const session = state(output.message.sessionID ?? "default");
      const prompt = output.parts
        .filter((part) => part.synthetic !== true && part.type === "text" && typeof part.text === "string")
        .map((part) => part.text)
        .join("\n");
      const [context, recall] = await Promise.all([
        session.guidanceDelivered ? Promise.resolve("") : callGuidance(),
        callNudge({ event: "recall", surface: "knowledge", prompt }),
      ]);

      const texts: string[] = [];
      if (context !== null && !session.guidanceDelivered) {
        if (context) texts.push(context);
        session.guidanceDelivered = true;
      }
      if (recall) texts.push(recall);

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

    "tool.execute.before": async (
      input: { tool: string; sessionID?: string; callID?: string },
      output: { args: Record<string, unknown> },
    ) => {
      if (!isKnowledgeWrite(input.tool) || !knowledgeOwnsGate()) return;
      const decision = await callGate(input.tool, output.args ?? {}, directory);
      if (decision.decision === "block") {
        throw new Error(decision.reason ?? "demarkus-knowledge gate blocked this write");
      }
      if (decision.decision === "ask") {
        throw new Error(`${decision.reason ?? "blocked"} Confirm with the user before retrying this write.`);
      }
      if (decision.decision === "warn" && decision.reason && input.sessionID && input.callID) {
        pendingWarns.set(`${input.sessionID}:${input.callID}`, decision.reason);
      }
    },

    "tool.execute.after": async (
      input: { tool: string; sessionID?: string; callID?: string },
      output: { output: string },
    ) => {
      if (!isKnowledgeWrite(input.tool) || !knowledgeOwnsGate()) return;
      const key = `${input.sessionID}:${input.callID}`;
      const warning = pendingWarns.get(key);
      pendingWarns.delete(key);
      if (warning) output.output += `\n\nWarning: ${warning}`;
    },

    event: async ({ event }: { event: { type: string; properties?: { info?: { id?: string } } } }) => {
      if (event.type === "session.created") {
        ensureBootstrapped();
        return;
      }
      if (event.type !== "session.deleted") return;
      const id = event.properties?.info?.id;
      if (!id) return;
      sessions.delete(id);
      for (const key of pendingWarns.keys()) {
        if (key.startsWith(`${id}:`)) pendingWarns.delete(key);
      }
    },
  };
};

export default DemarkusKnowledgePlugin;
