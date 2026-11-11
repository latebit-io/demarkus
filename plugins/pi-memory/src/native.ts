// Pi's built-in MCP support (pi 0.99 and later) connects servers an extension
// registers. The plugin binary records demarkus servers in pi-mcp-adapter's
// file; without the adapter nothing reads it, so the extension registers them.

import { readFileSync } from "node:fs";
import { homedir } from "node:os";
import { basename, join } from "node:path";

const MCP_CONFIG = join(homedir(), ".config", "mcp", "mcp.json");
const STATE_DIR = join(homedir(), ".demarkus");

// The registry file naming a surface's joined stores. A souls row carries its
// host; a knowledge system's broker URL is in knowledge-urls. The formats are
// the binary's: config.MemoryRow, knowledge.URL, broker.Validate (the /mcp).
const JOINED = { memory: "souls", knowledge: "knowledge-systems" } as const;
const KNOWLEDGE_URLS = "knowledge-urls";
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

// Registry records: fields are tab-separated, # lines are comments.
function rows(path: string): string[][] {
  const records = read(path).split("\n").map((line) => line.trim());
  return records.filter((record) => record && !record.startsWith("#")).map((record) => record.split("\t"));
}

// mcpUrl is the MCP endpoint of a broker, as a join derives it.
function mcpUrl(broker: string): string {
  return `${broker.replace(/\/+$/, "")}/mcp`;
}

// endpoints is each joined slug's MCP URL as the registry records it: "" for a
// store not reached over HTTP, undefined where none is recorded (a knowledge
// join older than knowledge-urls), which no entry can contradict.
function endpoints(surface: Surface, stateDir: string): Map<string, string | undefined> {
  const joined = rows(join(stateDir, JOINED[surface]));
  if (surface === "memory") {
    return new Map(joined.map(([slug, host = ""]) => [slug, /^https?:\/\//i.test(host) ? mcpUrl(host) : ""]));
  }
  const urls = new Map(rows(join(stateDir, KNOWLEDGE_URLS)).map(([slug, url]) => [slug, url ? mcpUrl(url) : undefined]));
  return new Map(joined.map(([slug]) => [slug, urls.get(slug)]));
}

// sameUrl compares two endpoints as URLs, so host case and a trailing slash
// do not matter; one that does not parse matches nothing.
function sameUrl(a: string, b: string): boolean {
  const normal = (url: string) => new URL(url).href.replace(/\/+$/, "");
  try {
    return normal(a) === normal(b);
  } catch {
    return false;
  }
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

// Owned is a surface's servers in the adapter's file. refused is each joined
// store left unregistered, with why: the gate judges a write by server name,
// so a name that reaches another endpoint would misdirect writes.
export interface Owned {
  servers: Map<string, ServerConfig>;
  refused: Map<string, string>;
}

// Settings pi and the adapter's file spell alike, registered as written. The
// adapter's auth marker is not one: pi reads auth as a provider token.
const CARRIED = { command: ["args", "env", "cwd", "enabled"], url: ["headers", "enabled"] } as const;

// asPi is an adapter entry as pi takes it, declared to the model like on
// every other harness.
function asPi(entry: Record<string, unknown>, kind: keyof typeof CARRIED): ServerConfig {
  const config: ServerConfig = { [kind]: entry[kind], exposure: "direct" };
  for (const key of CARRIED[kind]) if (entry[key] !== undefined) config[key] = entry[key];
  return config;
}

// mcpServers is the servers an mcp.json defines; a missing file defines none.
function mcpServers(path: string): Record<string, unknown> {
  const parsed: unknown = JSON.parse(read(path).trim() || "{}");
  return isRecord(parsed) && isRecord(parsed.mcpServers) ? parsed.mcpServers : {};
}

// piConfigs is pi's own mcp.json files: user-level, and the project's once it
// is trusted. A server one defines overrides a registration of the same name.
export function piConfigs(cwd: string, trusted: boolean): string[] {
  const agent = process.env.PI_CODING_AGENT_DIR?.replace(/^~(?=$|\/)/, homedir()) || join(homedir(), ".pi", "agent");
  return trusted ? [join(agent, "mcp.json"), join(cwd, ".pi", "mcp.json")] : [join(agent, "mcp.json")];
}

// Pi counts names that differ only in - and _ as one server.
const fold = (name: string) => name.replaceAll("-", "_");

// launch is how a stdio server starts. env and cwd count: they decide which
// state directory the demarkus binary serves.
function launch(server: Record<string, unknown>): string {
  const env = isRecord(server.env) ? Object.entries(server.env).sort() : [];
  return JSON.stringify([server.command, server.args ?? [], env, server.cwd ?? ""]);
}

function sameServer(entry: Record<string, unknown>, config: ServerConfig): boolean {
  if (typeof config.url === "string") return typeof entry.url === "string" && sameUrl(entry.url, config.url);
  return launch(entry) === launch(config);
}

// overriding is the pi file whose entry pi connects in place of name, when
// that entry reaches somewhere else; "" otherwise.
function overriding(name: string, config: ServerConfig, piFiles: string[]): string {
  for (const path of piFiles) {
    let defined: Record<string, unknown>;
    try {
      defined = mcpServers(path);
    } catch {
      continue; // Pi reports its own unreadable file and takes no server from it.
    }
    for (const [other, entry] of Object.entries(defined)) {
      // An entry with neither is a project override of enabled and exposure only.
      if (fold(other) !== fold(name) || !isRecord(entry) || (entry.command === undefined && entry.url === undefined)) continue;
      if (!sameServer(entry, config)) return path;
    }
  }
  return "";
}

// ownedServers is the demarkus servers of surface in the adapter's file that
// reach the store the registry records, checked against piFiles (piConfigs).
export function ownedServers(surface: Surface, configPath = MCP_CONFIG, stateDir = STATE_DIR, piFiles: string[] = []): Owned {
  const owned: Owned = { servers: new Map(), refused: new Map() };
  const remote = endpoints(surface, stateDir);
  for (const [name, entry] of Object.entries(mcpServers(configPath))) {
    if (!isRecord(entry)) continue;
    let config: ServerConfig;
    if (typeof entry.command === "string") {
      // Local and QUIC stores run a demarkus binary; they are the memory's.
      if (surface !== "memory" || !basename(entry.command).startsWith("demarkus-")) continue;
      config = asPi(entry, "command");
    } else if (typeof entry.url === "string" && remote.has(name)) {
      const recorded = remote.get(name);
      if (recorded !== undefined && !sameUrl(entry.url, recorded)) {
        owned.refused.set(name, `its URL in ${configPath} is not the store joined as ${name}`);
        continue;
      }
      config = asPi(entry, "url");
    } else {
      continue;
    }
    const path = overriding(name, config, piFiles);
    if (path) owned.refused.set(name, `${path} defines it at another endpoint; pi connects that one, not the store joined as ${name}`);
    else owned.servers.set(name, config);
  }
  return owned;
}

// NativeServers keeps pi's built-in MCP in step with the adapter's file for
// one surface. Registrations last one extension load, so start resets it.
export class NativeServers {
  private live = new Map<string, string>();
  private reported = new Set<string>();
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
  start(pi: McpApi, piFiles: string[] = []): string[] {
    this.live.clear();
    this.reported.clear();
    this.active = builtinMcp(pi);
    return this.sync(pi, piFiles);
  }

  // sync registers new and changed servers and drops the ones that left the
  // file (a join or a removal mid-session). Returns the failures that are new
  // since the last sync, so one that persists is reported once.
  sync(pi: McpApi, piFiles: string[] = []): string[] {
    if (!this.active) return [];
    const failures: string[] = [];
    try {
      const { servers, refused } = ownedServers(this.surface, this.configPath, this.stateDir, piFiles);
      for (const [name, why] of refused) failures.push(`${name}: ${why}`);
      this.apply(pi, servers, failures);
    } catch (error) {
      // An unreadable file drops nothing that is connected.
      failures.push(`${this.configPath}: ${message(error)}`);
    }
    const fresh = failures.filter((failure) => !this.reported.has(failure));
    this.reported = new Set(failures);
    return fresh;
  }

  // apply makes pi's registrations match servers. A name leaves live only once
  // pi dropped it, so a failed unregistration is retried on the next sync.
  private apply(pi: McpApi, servers: Map<string, ServerConfig>, failures: string[]): void {
    for (const [name, config] of servers) {
      const key = JSON.stringify(config);
      if (this.live.get(name) === key) continue;
      try {
        pi.registerMcpServer?.(name, config);
        this.live.set(name, key);
      } catch (error) {
        failures.push(`${name}: ${message(error)}`);
      }
    }
    for (const name of [...this.live.keys()]) {
      if (servers.has(name)) continue;
      try {
        pi.unregisterMcpServer?.(name);
        this.live.delete(name);
      } catch (error) {
        failures.push(`${name}: ${message(error)}`);
      }
    }
  }
}

function message(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}
