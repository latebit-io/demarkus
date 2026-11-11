import assert from "node:assert/strict";
import { mkdtempSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";
import { NativeServers, builtinMcp, ownedServers, type McpApi, type ServerConfig } from "../src/native.ts";

// home is a temp dir holding the adapter's file and the registry files.
function home(servers: Record<string, unknown>, souls = "", systems = "", urls = ""): { config: string; state: string } {
  const dir = mkdtempSync(join(tmpdir(), "pi-native-"));
  const config = join(dir, "mcp.json");
  writeFileSync(config, JSON.stringify({ mcpServers: servers }));
  writeFileSync(join(dir, "souls"), souls);
  writeFileSync(join(dir, "knowledge-systems"), systems);
  writeFileSync(join(dir, "knowledge-urls"), urls);
  return { config, state: dir };
}

// The registry rows of the stores FILE's URL entries were joined as.
const CLOUD = "cloud\thttps://memory.example\t0\t-\n";
const KNOWLEDGE_URL = "knowledge\thttps://knowledge.example\n";

// fakePi records registrations; refuse names a server whose registration
// throws, stuck one whose unregistration throws.
function fakePi(commands: string[] = ["mcp"], refuse = "", stuck = { name: "" }): McpApi & { servers: Map<string, ServerConfig> } {
  const servers = new Map<string, ServerConfig>();
  return {
    servers,
    registerMcpServer(name, config) {
      if (name === refuse) throw new Error("name taken");
      servers.set(name, config);
    },
    unregisterMcpServer(name) {
      if (name === stuck.name) throw new Error("busy");
      servers.delete(name);
    },
    getCommands: () => commands.map((name) => ({ name })),
  };
}

const BIN = "/home/u/.demarkus/bin/demarkus-plugin";
const FILE = {
  "demarkus-memory": { command: BIN, args: ["mcp-serve"] },
  team: { command: BIN, args: ["mcp-serve", "--memory", "team"] },
  cloud: { url: "https://memory.example/mcp", auth: "oauth" },
  knowledge: { url: "https://knowledge.example/mcp", auth: "oauth" },
  github: { url: "https://api.example/mcp", auth: "oauth" },
  files: { command: "npx", args: ["-y", "server-filesystem"] },
};

test("each surface owns its own servers, and no one else's", () => {
  const { config, state } = home(FILE, CLOUD, "# joined\nknowledge\n", KNOWLEDGE_URL);
  assert.deepEqual(Object.fromEntries(ownedServers("memory", config, state).servers), {
    "demarkus-memory": { command: BIN, args: ["mcp-serve"], exposure: "direct" },
    team: { command: BIN, args: ["mcp-serve", "--memory", "team"], exposure: "direct" },
    cloud: { url: "https://memory.example/mcp", exposure: "direct" },
  });
  // The adapter's auth marker is dropped: pi reads auth as a provider token.
  assert.deepEqual(Object.fromEntries(ownedServers("knowledge", config, state).servers), {
    knowledge: { url: "https://knowledge.example/mcp", exposure: "direct" },
  });
});

test("a URL entry is owned only where the registry records its endpoint", () => {
  const moved = { cloud: { url: "https://elsewhere.example/mcp" }, knowledge: { url: "https://elsewhere.example/mcp" } };
  const { config, state } = home(moved, CLOUD, "knowledge\n", KNOWLEDGE_URL);
  const memory = ownedServers("memory", config, state);
  assert.equal(memory.servers.size, 0);
  assert.match(memory.refused.get("cloud") ?? "", /is not the store joined as cloud$/);
  const knowledge = ownedServers("knowledge", config, state);
  assert.equal(knowledge.servers.size, 0);
  assert.deepEqual([...knowledge.refused.keys()], ["knowledge"]);

  // Host case and a trailing slash are the same endpoint.
  const same = home({ cloud: { url: "https://MEMORY.example/mcp/" } }, "cloud\thttps://memory.example/\t0\t-\n");
  assert.deepEqual([...ownedServers("memory", same.config, same.state).servers.keys()], ["cloud"]);

  // A store joined over QUIC is served by the binary, never by a URL entry.
  const quic = home({ cloud: FILE.cloud }, "cloud\tmark://memory.example\t0\t/t\n");
  assert.deepEqual([...ownedServers("memory", quic.config, quic.state).refused.keys()], ["cloud"]);

  // A knowledge join older than knowledge-urls records no endpoint to contradict.
  const old = home({ knowledge: FILE.knowledge }, "", "knowledge\n");
  assert.deepEqual([...ownedServers("knowledge", old.config, old.state).servers.keys()], ["knowledge"]);
});

test("settings pi shares with the adapter's file are registered as written", () => {
  const env = { DEMARKUS_TAINT_STRICTNESS: "ask" };
  const headers = { Authorization: "Bearer ${TOKEN}" };
  const file = {
    "demarkus-memory": { command: BIN, args: ["mcp-serve"], env, cwd: "/w", enabled: false, lifecycle: "lazy" },
    cloud: { url: "https://memory.example/mcp", auth: "oauth", headers, enabled: false, bearerToken: "x" },
  };
  const { config, state } = home(file, CLOUD);
  assert.deepEqual(Object.fromEntries(ownedServers("memory", config, state).servers), {
    "demarkus-memory": { command: BIN, args: ["mcp-serve"], env, cwd: "/w", enabled: false, exposure: "direct" },
    cloud: { url: "https://memory.example/mcp", headers, enabled: false, exposure: "direct" },
  });
});

test("a server pi's own mcp.json points elsewhere is refused, naming the file", () => {
  const { config, state } = home(FILE, CLOUD, "knowledge\n", KNOWLEDGE_URL);
  const piFile = (servers: Record<string, unknown>) => {
    const path = join(mkdtempSync(join(tmpdir(), "pi-own-")), "mcp.json");
    writeFileSync(path, JSON.stringify({ mcpServers: servers }));
    return path;
  };
  // Pi folds - and _ in a name, and its entry wins over a registration.
  const other = piFile({ demarkus_memory: { command: "/elsewhere/demarkus-mcp" }, cloud: { url: "https://elsewhere.example/mcp" } });
  const memory = ownedServers("memory", config, state, [other]);
  assert.deepEqual([...memory.servers.keys()], ["team"]);
  assert.deepEqual([...memory.refused.keys()].sort(), ["cloud", "demarkus-memory"]);
  assert.ok(memory.refused.get("cloud")?.startsWith(`${other} defines it at another endpoint`));

  // The same command under another HOME or directory serves another store.
  for (const moved of [{ env: { HOME: "/other" } }, { cwd: "/other" }]) {
    const relocated = piFile({ "demarkus-memory": { ...FILE["demarkus-memory"], ...moved } });
    assert.deepEqual([...ownedServers("memory", config, state, [relocated]).refused.keys()], ["demarkus-memory"]);
  }

  // The same endpoint, an override of enabled alone, and a broken file refuse nothing.
  const same = piFile({ cloud: { url: "https://memory.example/mcp/" }, team: { enabled: false }, "demarkus-memory": FILE["demarkus-memory"] });
  const broken = join(mkdtempSync(join(tmpdir(), "pi-own-")), "mcp.json");
  writeFileSync(broken, "{not json");
  const clean = ownedServers("memory", config, state, [same, broken, join(state, "absent.json")]);
  assert.equal(clean.refused.size, 0);
  assert.deepEqual([...clean.servers.keys()].sort(), ["cloud", "demarkus-memory", "team"]);
});

test("a missing file owns nothing; a broken one throws", () => {
  const dir = mkdtempSync(join(tmpdir(), "pi-native-"));
  assert.equal(ownedServers("memory", join(dir, "mcp.json"), dir).servers.size, 0);
  writeFileSync(join(dir, "mcp.json"), "{not json");
  assert.throws(() => ownedServers("memory", join(dir, "mcp.json"), dir));
});

test("the built-in MCP is used only without pi-mcp-adapter", () => {
  assert.equal(builtinMcp(fakePi(["mcp", "soul"])), true);
  assert.equal(builtinMcp(fakePi(["mcp", "mcp-adapter"])), false);
  assert.equal(builtinMcp(fakePi(["mcp", "mcp-auth"])), false);
  assert.equal(builtinMcp(fakePi(["soul"])), false); // --no-mcp, or disabled in settings
  assert.equal(builtinMcp({}), false); // pi before 0.99
});

test("start registers, sync follows joins and removals", () => {
  const { config, state } = home({ "demarkus-memory": FILE["demarkus-memory"] });
  const pi = fakePi();
  const native = new NativeServers("memory", config, state);
  assert.deepEqual(native.start(pi), []);
  assert.deepEqual([...pi.servers.keys()], ["demarkus-memory"]);
  const joined = { "demarkus-memory": FILE["demarkus-memory"], cloud: FILE.cloud };

  writeFileSync(config, JSON.stringify({ mcpServers: joined }));
  writeFileSync(join(state, "souls"), CLOUD);
  assert.deepEqual(native.sync(pi), []);
  assert.deepEqual([...pi.servers.keys()].sort(), ["cloud", "demarkus-memory"]);

  // The entry moves off the joined host: disconnected, and reported once.
  const moved = { "demarkus-memory": FILE["demarkus-memory"], cloud: { url: "https://elsewhere.example/mcp" } };
  writeFileSync(config, JSON.stringify({ mcpServers: moved }));
  const refused = native.sync(pi);
  assert.equal(refused.length, 1);
  assert.match(refused[0], /^cloud: its URL in .* is not the store joined as cloud$/);
  assert.deepEqual(native.sync(pi), []);
  assert.deepEqual([...pi.servers.keys()], ["demarkus-memory"]);

  writeFileSync(config, JSON.stringify({ mcpServers: joined }));
  assert.deepEqual(native.sync(pi), []);
  assert.deepEqual([...pi.servers.keys()].sort(), ["cloud", "demarkus-memory"]);

  writeFileSync(config, JSON.stringify({ mcpServers: { cloud: FILE.cloud } }));
  assert.deepEqual(native.sync(pi), []);
  assert.deepEqual([...pi.servers.keys()], ["cloud"]);
});

test("a server pi fails to drop stays tracked and is dropped on a later sync", () => {
  const { config, state } = home({ "demarkus-memory": FILE["demarkus-memory"], team: FILE.team });
  const stuck = { name: "team" };
  const pi = fakePi(["mcp"], "", stuck);
  const native = new NativeServers("memory", config, state);
  assert.deepEqual(native.start(pi), []);

  writeFileSync(config, JSON.stringify({ mcpServers: { "demarkus-memory": FILE["demarkus-memory"] } }));
  assert.deepEqual(native.sync(pi), ["team: busy"]);
  assert.deepEqual(native.sync(pi), []);
  assert.deepEqual([...pi.servers.keys()].sort(), ["demarkus-memory", "team"]);

  stuck.name = "";
  assert.deepEqual(native.sync(pi), []);
  assert.deepEqual([...pi.servers.keys()], ["demarkus-memory"]);
});

test("with the adapter loaded nothing is registered", () => {
  const { config, state } = home(FILE, CLOUD, "knowledge\n", KNOWLEDGE_URL);
  const pi = fakePi(["mcp", "mcp-adapter"]);
  const native = new NativeServers("memory", config, state);
  assert.deepEqual(native.start(pi), []);
  assert.deepEqual(native.sync(pi), []);
  assert.equal(pi.servers.size, 0);
});

test("a failure is reported once, and again when its cause changes", () => {
  const { config, state } = home({ "demarkus-memory": FILE["demarkus-memory"], team: FILE.team });
  const pi = fakePi(["mcp"], "team");
  const native = new NativeServers("memory", config, state);
  assert.deepEqual(native.start(pi), ["team: name taken"]);
  assert.deepEqual(native.sync(pi), []);
  assert.deepEqual([...pi.servers.keys()], ["demarkus-memory"]);

  writeFileSync(config, "{not json");
  const broken = native.sync(pi);
  assert.equal(broken.length, 1);
  assert.match(broken[0], /mcp\.json/);
  assert.deepEqual(native.sync(pi), []);
  // An unreadable file drops nothing that is connected.
  assert.deepEqual([...pi.servers.keys()], ["demarkus-memory"]);
});
