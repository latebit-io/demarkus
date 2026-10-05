import assert from "node:assert/strict";
import { mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { homedir } from "node:os";
import { join } from "node:path";
import { pathToFileURL } from "node:url";
import test from "node:test";
import type { Plugin } from "@opencode/plugin";

// Import the standalone installed file, outside this package's node_modules.
const installed = await import(pathToFileURL(process.env.DEMARKUS_TEST_PLUGIN!).href);
const plugin = installed.default as typeof import("../src/demarkus-knowledge.ts").default;
const home = homedir();
const bin = join(home, ".demarkus", "bin");
const callsFile = join(home, "gate-calls.jsonl");
mkdirSync(bin, { recursive: true });
mkdirSync(join(home, ".config", "mcp"), { recursive: true });
writeFileSync(join(home, ".demarkus", "knowledge-systems"), "acme\nexplicit\nacme-demarkus-memory\n");
writeFileSync(join(home, ".config", "mcp", "mcp.json"), JSON.stringify({ mcpServers: {
  acme: { url: "https://acme.test/mcp" },
  explicit: { url: "https://catalog.test/mcp" },
} }));
writeFileSync(join(bin, "demarkus-plugin"), `#!/usr/bin/env node
const fs = require("node:fs");
const command = process.argv[2];
if (command === "version") { console.log("99.99.99"); process.exit(0); }
if (command === "guidance") { console.log(JSON.stringify({context: "Knowledge guidance"})); process.exit(0); }
if (command === "update-check") { console.log("{}"); process.exit(0); }
let text = "";
process.stdin.on("data", chunk => { text += chunk; });
process.stdin.on("end", () => {
  const request = JSON.parse(text);
  if (command === "gate") {
    fs.appendFileSync(${JSON.stringify(callsFile)}, JSON.stringify(request) + "\\n");
    console.log(JSON.stringify({decision: request.input.decision || "allow", reason: "policy reason"}));
  } else if (command === "nudge") console.log(JSON.stringify({nudge: "Recall " + request.prompt}));
  else process.exit(1);
});
`, { mode: 0o755 });
writeFileSync(callsFile, "");

function gateCalls(): Array<{ tool: string; input: unknown; cwd: string }> {
  return readFileSync(callsFile, "utf8").trim().split("\n").filter(Boolean).map((line) => JSON.parse(line));
}

function harness(directory: string) {
  const hooks = new Map<string, (event: any) => Promise<void>>();
  const servers = new Map<string, any>([
    ["explicit", { type: "remote", url: "https://explicit.test/mcp", disabled: true, headers: { custom: "value" } }],
    ["acme-demarkus-memory", { type: "remote", url: "https://reserved.test/mcp" }],
  ]);
  const commands = new Map<string, any>();
  const prompts: any[] = [];
  let replayMcp: () => void = () => {};
  let signal: AbortSignal;
  let receive: ((event: any) => void) | undefined;
  const ctx = {
    location: { directory },
    mcp: { transform: async (fn: any) => {
      replayMcp = () => fn({
        get: (name: string) => servers.get(name),
        set: (name: string, config: unknown) => servers.set(name, config),
        update: (name: string, update: any) => { if (servers.has(name)) update(servers.get(name)); },
      });
      replayMcp();
    } },
    command: {
      list: async () => ({ data: [{ name: "knowledge" }] }),
      transform: async (fn: any) => fn({ add: (command: any) => commands.set(command.name, command) }),
    },
    session: {
      hook: async (name: string, fn: any) => { hooks.set(name, fn); },
      prompt: async (input: any) => { prompts.push(input); },
    },
    tool: { hook: async (name: string, fn: any) => { hooks.set(name, fn); } },
    event: { subscribe: async function* (options: { signal: AbortSignal }) {
      signal = options.signal;
      while (!signal.aborted) {
        const event = await new Promise<any>((resolve) => {
          const abort = () => resolve(null);
          receive = (value) => {
            signal.removeEventListener("abort", abort);
            resolve(value);
          };
          signal.addEventListener("abort", abort, { once: true });
        });
        if (event) yield event;
      }
    } },
  };
  return {
    ctx: ctx as unknown as Plugin.Context, hooks, servers, commands, prompts,
    replayMcp: () => replayMcp(),
    aborted: () => signal.aborted,
    emit: async (event: any) => {
      assert.ok(receive, "subscription is listening");
      receive(event);
      await new Promise((resolve) => setImmediate(resolve));
    },
  };
}

test("installed default supports the V2 and V1 object entrypoint contracts", () => {
  assert.equal(plugin.id, "demarkus-knowledge");
  assert.equal(typeof plugin.setup, "function");
  assert.equal(plugin.server, installed.DemarkusKnowledgePlugin);
});

test("installed V1 adapter keeps config, synthetic guidance and write gates", async (t) => {
  const legacy = await plugin.server({ client: {}, directory: "/v1" });
  t.after(() => legacy.dispose());
  const explicit = { type: "remote", url: "https://explicit.test/mcp", enabled: false };
  const command = { template: "custom" };
  const config: any = { mcp: { explicit }, command: { knowledge: command } };
  await legacy.config(config);
  assert.deepEqual(config.mcp.acme, { type: "remote", url: "https://acme.test/mcp", enabled: true });
  assert.equal(config.mcp.explicit, explicit);
  assert.equal(config.command.knowledge, command);
  assert.ok(config.command["knowledge-join"].template);
  const output: any = { message: { id: "msg_1", sessionID: "s1" }, parts: [{ type: "text", text: "question" }] };
  await legacy["chat.message"]({}, output);
  assert.deepEqual(output.parts.slice(1).map((part: any) => part.text), ["Knowledge guidance", "Recall question"]);
  assert.ok(output.parts.slice(1).every((part: any) => part.synthetic && part.sessionID === "s1" && part.messageID === "msg_1"));
  await assert.rejects(legacy["tool.execute.before"]({ tool: "acme_mark_publish" }, { args: { decision: "block" } }), /policy reason/);
  const call = { tool: "acme_mark_append", sessionID: "s1", callID: "c1" };
  await legacy["tool.execute.before"](call, { args: { decision: "warn" } });
  const result = { output: "Saved" };
  await legacy["tool.execute.after"](call, result);
  assert.equal(result.output, "Saved\n\nWarning: policy reason");
});

test("installed V2 setup wires MCP and executes commands with attachments and literal arguments", async (t) => {
  const h = harness("/v2-config");
  const stop = await plugin.setup(h.ctx);
  t.after(stop);
  assert.deepEqual(h.servers.get("acme"), { type: "remote", url: "https://acme.test/mcp" });
  assert.deepEqual(h.servers.get("explicit"), { type: "remote", url: "https://explicit.test/mcp", disabled: true, headers: { custom: "value" } });
  assert.equal(h.servers.get("acme-demarkus-memory").disabled, true);
  h.servers.set("acme", { type: "remote", url: "https://changed.test/mcp", disabled: true });
  h.replayMcp();
  assert.equal(h.servers.get("acme").url, "https://changed.test/mcp");
  assert.equal(h.commands.has("knowledge"), false, "explicit commands take precedence");
  const mention = { start: 0, end: 4, text: "file" };
  await h.commands.get("knowledge-join").execute({
    sessionID: "s1", delivery: "queue",
    prompt: { text: "https://broker.test/$&", files: [{ uri: "file:///a", mention }], agents: [{ name: "build", mention }], skills: [{ id: "review", mention }] },
  });
  const prompt = h.prompts[0];
  assert.equal(prompt.sessionID, "s1");
  assert.equal(prompt.delivery, "queue");
  assert.match(prompt.text, /User arguments \(may be empty\): https:\/\/broker.test\/\$&\n$/);
  assert.deepEqual(prompt.files, [{ uri: "file:///a" }]);
  assert.deepEqual(prompt.agents, [{ name: "build" }]);
  assert.deepEqual(prompt.skills, [{ id: "review" }]);
  await h.commands.get("knowledge-doctor").execute({ sessionID: "s1", delivery: "steer", prompt: { text: "" } });
  assert.doesNotMatch(h.prompts[1].text, /\$ARGUMENTS/);
});

test("V2 guidance stays model-only, retries unavailable guidance and expires session nudges", async (t) => {
  const h = harness("/v2-guidance");
  let attempts = 0;
  const stop = await installed.setupV2(h.ctx, { guidance: async () => ++attempts === 1 ? null : "Guidance" });
  t.after(stop);
  const prompt = { sessionID: "s1", prompt: { text: "original" } };
  await h.hooks.get("prompt")!(prompt);
  assert.equal(prompt.prompt.text, "original");
  const context = async (sessionID = "s1") => {
    const event = { sessionID, system: [] as any[] };
    await h.hooks.get("context")!(event);
    return event.system.map((part) => part.text);
  };
  assert.deepEqual(await context("s2"), []);
  assert.deepEqual(await context(), ["Guidance", "Recall original"]);
  assert.deepEqual(await context(), ["Guidance"]);
  assert.equal(attempts, 2);
  for (const type of ["session.idle", "session.deleted"]) {
    await h.hooks.get("prompt")!(prompt);
    await h.emit({ type, data: { sessionID: "s1" } });
    assert.deepEqual(await context(), ["Guidance"]);
  }
  await stop();
  assert.equal(h.aborted(), true);
});

test("V2 gates block and ask, preserve structured results, and discard failed/deleted warnings", async (t) => {
  const h = harness("/v2-gates");
  t.after(await plugin.setup(h.ctx));
  const before = h.hooks.get("execute.before")!;
  const after = h.hooks.get("execute.after")!;
  const call = { tool: "acme_mark_publish", sessionID: "s1", id: "c1", input: { decision: "warn" } };
  for (const decision of ["block", "ask"]) {
    await assert.rejects(before({ ...call, input: { decision } }), decision === "ask" ? /Confirm with the user/ : /policy reason/);
  }
  for (const content of ["Saved", [{ type: "text", text: "Saved" }, { type: "file", uri: "file:///a" }], undefined]) {
    await before(call);
    const event: any = { ...call, status: "completed", result: { content, metadata: { retained: true } } };
    await after(event);
    assert.deepEqual(event.result.metadata, { retained: true });
    assert.deepEqual(event.result.content, typeof content === "string"
      ? "Saved\n\nWarning: policy reason"
      : [...(content ?? []), { type: "text", text: "Warning: policy reason" }]);
  }
  assert.deepEqual(gateCalls().at(-1), { tool: call.tool, input: call.input, cwd: "/v2-gates" });
  for (const failure of ["error", "deleted"]) {
    await before(call);
    if (failure === "error") await after({ ...call, status: "error", error: { message: "failed" } });
    else await h.emit({ type: "session.deleted", data: { sessionID: "s1" } });
    const result = { ...call, status: "completed", result: { content: "Saved" } };
    await after(result);
    assert.equal(result.result.content, "Saved");
  }
  const count = gateCalls().length;
  await before({ ...call, tool: "acme_mark_fetch" });
  assert.equal(gateCalls().length, count);
});

test("V2 async callbacks cannot restore nudges or warnings after session deletion or unload", async (t) => {
  const h = harness("/v2-inflight");
  let resolveNudge: (text: string) => void = () => {};
  const stop = await installed.setupV2(h.ctx, {
    guidance: async () => "",
    nudge: () => new Promise<string>((resolve) => { resolveNudge = resolve; }),
  });
  t.after(stop);
  for (const unload of [false, true]) {
    const prompting = h.hooks.get("prompt")!({ sessionID: "s1", prompt: { text: "question" } });
    const call = { tool: "acme_mark_append", sessionID: "s1", id: "c1", input: { decision: "warn" } };
    const gating = h.hooks.get("execute.before")!(call);
    if (unload) await stop();
    else await h.emit({ type: "session.deleted", data: { sessionID: "s1" } });
    resolveNudge("Stale nudge");
    await Promise.all([prompting, gating]);
    const context = { sessionID: "s1", system: [] };
    await h.hooks.get("context")!(context);
    assert.deepEqual(context.system, []);
    const result = { ...call, status: "completed", result: { content: "Saved" } };
    await h.hooks.get("execute.after")!(result);
    assert.equal(result.result.content, "Saved");
  }
});

test("V2 defers to memory gate owner in either load order and releases ownership on unload", async () => {
  const registry = (globalThis as any)[Symbol.for("io.demarkus.opencode.gate-adapters")] as Map<string, Set<string>>;
  for (const memoryFirst of [true, false]) {
    const directory = `/v2-coinstall-${memoryFirst}`;
    if (memoryFirst) registry.set(directory, new Set(["memory"]));
    const h = harness(directory);
    const stop = await plugin.setup(h.ctx);
    registry.get(directory)!.add("memory");
    const count = gateCalls().length;
    await h.hooks.get("execute.before")!({ tool: "acme_mark_publish", sessionID: "s1", id: "c1", input: { decision: "block" } });
    assert.equal(gateCalls().length, count);
    await stop();
    assert.deepEqual(registry.get(directory), new Set(["memory"]));
    registry.delete(directory);
  }
  const h = harness("/v2-unload");
  const stop = await plugin.setup(h.ctx);
  await stop();
  assert.equal(registry.has("/v2-unload"), false);
  const replacement = await plugin.setup(harness("/v2-unload").ctx);
  await stop();
  assert.deepEqual(registry.get("/v2-unload"), new Set(["knowledge"]), "cleanup is idempotent across reloads");
  await replacement();
});

test("V2 setup failure releases its gate registration", async () => {
  const h = harness("/v2-failure");
  h.ctx.command.list = async () => { throw new Error("registry failed"); };
  await assert.rejects(plugin.setup(h.ctx), /registry failed/);
  const registry = (globalThis as any)[Symbol.for("io.demarkus.opencode.gate-adapters")] as Map<string, Set<string>>;
  assert.equal(registry.has("/v2-failure"), false);
});
