// Pi's built-in MCP support (pi 0.99 and later) connects servers an extension
// registers. The plugin binary records demarkus servers in pi-mcp-adapter's
// file; without the adapter nothing reads it, so the extension registers them.

import { readFileSync } from "node:fs";
import { homedir } from "node:os";
import { basename, join } from "node:path";

const MCP_CONFIG = join(homedir(), ".config", "mcp", "mcp.json");
const STATE_DIR = join(homedir(), ".demarkus");

// The registry file naming the remote stores a surface joined over HTTP.
const JOINED = { memory: "souls", knowledge: "knowledge-systems" } as const;
export type Surface = keyof typeof JOINED;

// /mcp is the built-in MCP support's command: absent under --no-mcp or with it
// disabled in settings, where pi reports every registration as an error.
const BUILTIN_COMMAND = "mcp";
// Commands only pi-mcp-adapter registers, at load. It replaces the built-in
// MCP support and connects its own file, so registering would duplicate.
const ADAPTER_COMMANDS = new Set(["mcp-adapter", "mcp-auth"]);

export type ServerConfig = Record<string, unknown>;

// The part of pi's ExtensionAPI this module calls; absent before pi 0.99.
export interface McpApi {
  registerMcpServer?(name: string, config: ServerConfig): void;
  unregisterMcpServer?(name: string): void;
  getCommands?(): { name: string }[];
}

// builtinMcp reports pi's own MCP support connecting what an extension registers.
export function builtinMcp(pi: McpApi): boolean {
  if (typeof pi.registerMcpServer !== "function" || typeof pi.unregisterMcpServer !== "function") return false;
  const commands = (pi.getCommands?.() ?? []).map((command) => command.name);
  return commands.includes(BUILTIN_COMMAND) && !commands.some((name) => ADAPTER_COMMANDS.has(name));
}

// read is a file's text, "" when it does not exist; any other failure throws.
function read(path: string): string {
  try {
    return readFileSync(path, "utf8");
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ENOENT") return "";
    throw error;
  }
}

// joined is the slugs of a registry file: the first field of each record.
function joined(path: string): Set<string> {
  const slugs = new Set<string>();
  for (const line of read(path).split("\n")) {
    const record = line.trim();
    if (record && !record.startsWith("#")) slugs.add(record.split(/\s/)[0]);
  }
  return slugs;
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

// ownedServers is the demarkus servers of surface in the adapter's file, as
// pi takes them: declared to the model like every other harness, and without
// the adapter's auth marker, which pi reads as a provider token.
export function ownedServers(surface: Surface, configPath = MCP_CONFIG, stateDir = STATE_DIR): Map<string, ServerConfig> {
  const owned = new Map<string, ServerConfig>();
  const text = read(configPath);
  if (!text.trim()) return owned;
  const parsed: unknown = JSON.parse(text);
  const servers = isRecord(parsed) && isRecord(parsed.mcpServers) ? parsed.mcpServers : {};
  const remote = joined(join(stateDir, JOINED[surface]));
  for (const [name, entry] of Object.entries(servers)) {
    if (!isRecord(entry)) continue;
    if (typeof entry.command === "string") {
      // Local and QUIC stores run a demarkus binary; they are the memory's.
      if (surface !== "memory" || !basename(entry.command).startsWith("demarkus-")) continue;
      const config: ServerConfig = { command: entry.command, exposure: "direct" };
      if (Array.isArray(entry.args)) config.args = entry.args;
      owned.set(name, config);
    } else if (typeof entry.url === "string" && remote.has(name)) {
      owned.set(name, { url: entry.url, exposure: "direct" });
    }
  }
  return owned;
}

// NativeServers keeps pi's built-in MCP in step with the adapter's file for
// one surface. Registrations last one extension load, so start resets it.
export class NativeServers {
  private live = new Map<string, string>();
  private failed = new Map<string, string>();
  private active = false;
  private readonly surface: Surface;
  private readonly configPath: string;
  private readonly stateDir: string;

  constructor(surface: Surface, configPath = MCP_CONFIG, stateDir = STATE_DIR) {
    this.surface = surface;
    this.configPath = configPath;
    this.stateDir = stateDir;
  }

  // start is a session start: decide whether pi connects servers, then sync.
  start(pi: McpApi): string[] {
    this.live.clear();
    this.failed.clear();
    this.active = builtinMcp(pi);
    return this.sync(pi);
  }

  // sync registers new and changed servers and drops the ones that left the
  // file (a join or a removal mid-session). Returns each failure once.
  sync(pi: McpApi): string[] {
    if (!this.active) return [];
    let owned: Map<string, ServerConfig>;
    try {
      owned = ownedServers(this.surface, this.configPath, this.stateDir);
    } catch (error) {
      return this.once("config", `${this.configPath}: ${message(error)}`);
    }
    this.failed.delete("config");
    const errors: string[] = [];
    for (const [name, config] of owned) {
      const key = JSON.stringify(config);
      if (this.live.get(name) === key) continue;
      try {
        pi.registerMcpServer?.(name, config);
        this.live.set(name, key);
        this.failed.delete(name);
      } catch (error) {
        errors.push(...this.once(name, `${name}: ${message(error)}`, key));
      }
    }
    for (const name of [...this.live.keys()]) {
      if (owned.has(name)) continue;
      this.live.delete(name);
      try {
        pi.unregisterMcpServer?.(name);
      } catch (error) {
        errors.push(`${name}: ${message(error)}`);
      }
    }
    return errors;
  }

  // once reports a failure the first time its cause is seen, not every turn.
  private once(name: string, text: string, cause = text): string[] {
    if (this.failed.get(name) === cause) return [];
    this.failed.set(name, cause);
    return [text];
  }
}

function message(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}
