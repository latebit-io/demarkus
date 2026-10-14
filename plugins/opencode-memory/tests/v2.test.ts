import assert from "node:assert/strict";
import test from "node:test";
import { setupV2 } from "../src/demarkus-memory.ts";

test("V2 registers MCP, guards writes and delivers guidance and nudges", async () => {
  const hooks = new Map<string, (event: any) => Promise<void>>();
  const servers = new Map<string, unknown>();
  let nextEvent: ((value: any) => void) | undefined;
  const events: any[] = [];
  const ctx: any = {
    location: { directory: "/test/project" },
    mcp: { transform: async (fn: any) => fn({ get: (name: string) => servers.get(name), set: (name: string, config: unknown) => servers.set(name, config) }) },
    session: {
      hook: async (name: string, fn: any) => { hooks.set(name, fn); },
    },
    tool: { hook: async (name: string, fn: any) => { hooks.set(name, fn); } },
    event: { subscribe: async function* ({ signal }: { signal: AbortSignal }) {
      while (!signal.aborted) {
        const event = events.shift() ?? await new Promise<any>((resolve) => { nextEvent = resolve; });
        if (event) yield event;
      }
    } },
  };
  const gateCalls: unknown[] = [];
  const sessionEndCwds: unknown[] = [];
  const lifecycle: string[] = [];
  const notices: string[] = [];
  const stop = await setupV2(ctx, {
    legacy: async ({ client }) => {
      const original = console.error;
      console.error = (message: string) => { notices.push(message); };
      try {
        await client.tui?.showToast?.({ body: { message: "bootstrap failed", variant: "warning" } });
      } finally {
        console.error = original;
      }
      return {
        config: async (config: any) => { config.mcp = { "demarkus-memory": { type: "local", command: ["bin", "mcp-serve"], enabled: true } }; },
        event: async ({ event }: { event: { type: string } }) => { lifecycle.push(event.type); },
      } as any;
    },
    guidance: async () => "Recall first",
    nudge: async (input) => {
      if (input.event === "session-end") sessionEndCwds.push(input.cwd);
      return input.event === "recall" ? "Recall nudge" : input.event === "promote" ? "Promote nudge" : "Journal nudge";
    },
    gate: async (tool, input, directory) => {
      gateCalls.push({ tool, input, directory });
      return input.block ? { decision: "block", reason: "write denied" } : { decision: "warn", reason: "Tag this publish" };
    },
  });
  assert.deepEqual(servers.get("demarkus-memory"), { type: "local", command: ["bin", "mcp-serve"] });
  assert.deepEqual(notices, ["[demarkus-memory] bootstrap failed"]);
  const prompt = { sessionID: "s1", prompt: { text: "original" } };
  await hooks.get("prompt")!(prompt);
  assert.equal(prompt.prompt.text, "original");
  const context = { sessionID: "s1", system: [] as Array<{ type: string; text: string }> };
  await hooks.get("context")!(context);
  assert.deepEqual(context.system, [{ type: "text", text: "Recall first" }, { type: "text", text: "Recall nudge" }]);
  const continuation = { sessionID: "s1", system: [] as Array<{ type: string; text: string }> };
  await hooks.get("context")!(continuation);
  assert.deepEqual(continuation.system, [{ type: "text", text: "Recall first" }]);
  assert.equal(gateCalls.length, 0);
  await assert.rejects(hooks.get("execute.before")!({ tool: "demarkus-memory_mark_publish", sessionID: "s1", id: "blocked", input: { block: true } }), /write denied/);
  const call = { tool: "demarkus-memory_mark_publish", sessionID: "s1", id: "allowed", input: { url: "/note.md" } };
  await hooks.get("execute.before")!(call);
  const result = { ...call, status: "completed", result: { content: "Saved", metadata: { ok: true } } };
  await hooks.get("execute.after")!(result);
  assert.deepEqual(gateCalls.at(-1), { tool: call.tool, input: call.input, directory: "/test/project" });
  assert.deepEqual(result.result, { content: "Saved\n\n⚠️ Tag this publish\n\nPromote nudge", metadata: { ok: true } });

  await hooks.get("execute.after")!({ tool: "patch", sessionID: "s2", id: "edit", input: {}, status: "completed", result: {} });
  const idle = { type: "session.idle", data: { sessionID: "s2" } };
  if (nextEvent) { const resolve = nextEvent; nextEvent = undefined; resolve(idle); } else events.push(idle);
  await new Promise((resolve) => setImmediate(resolve));
  const nextPrompt = { sessionID: "s2", prompt: { text: "Continue" } };
  await hooks.get("prompt")!(nextPrompt);
  assert.equal(nextPrompt.prompt.text, "Continue");
  const nextContext = { sessionID: "s2", system: [] as Array<{ type: string; text: string }> };
  await hooks.get("context")!(nextContext);
  assert.deepEqual(nextContext.system, [
    { type: "text", text: "Recall first" },
    { type: "text", text: "Journal nudge" },
    { type: "text", text: "Recall nudge" },
  ]);
  // A journal write after the last edit suppresses the idle nudge; a plan
  // write does not, and an edit after the entry makes it stale again.
  const cases = [["s4", "/p/journal/2026-10-14.md", false, false], ["s5", "/p/plans/x.md", true, false], ["s6", "/p/journal/2026-10-14.md", true, true]] as const;
  for (const [sessionID, url, nudged, editAfter] of cases) {
    await hooks.get("execute.after")!({ tool: "patch", sessionID, id: "edit", input: {}, status: "completed", result: {} });
    await hooks.get("execute.after")!({ tool: "demarkus-memory_mark_publish", sessionID, id: "w", input: { url }, status: "completed", result: { content: "Saved" } });
    if (editAfter) await hooks.get("execute.after")!({ tool: "patch", sessionID, id: "edit2", input: {}, status: "completed", result: {} });
    const idleN = { type: "session.idle", data: { sessionID } };
    if (nextEvent) { const resolve = nextEvent; nextEvent = undefined; resolve(idleN); } else events.push(idleN);
    await new Promise((resolve) => setImmediate(resolve));
    const ctxN = { sessionID, system: [] as Array<{ type: string; text: string }> };
    await hooks.get("context")!(ctxN);
    assert.equal(ctxN.system.some((p) => p.text === "Journal nudge"), nudged, `${sessionID} ${url}`);
  }
  assert.ok(sessionEndCwds.length > 0 && sessionEndCwds.every((cwd) => cwd === "/test/project"), `session-end cwd: ${sessionEndCwds}`);
  const created = { type: "session.created", data: { sessionID: "s3" } };
  if (nextEvent) { const resolve = nextEvent; nextEvent = undefined; resolve(created); } else events.push(created);
  await new Promise((resolve) => setImmediate(resolve));
  assert.deepEqual(lifecycle, ["session.created"]);
  stop();
});
