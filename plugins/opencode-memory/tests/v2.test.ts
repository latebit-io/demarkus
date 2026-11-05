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
    command: { list: async () => ({ data: [] }), transform: async (fn: any) => fn({ add: () => {} }) },
    skill: { transform: async (fn: any) => fn({ list: () => [], add: () => {} }) },
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
  const gateTaint: unknown[] = [];
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
        dispose: () => {},
      } as any;
    },
    guidance: async () => ({ context: "Recall first", status: "demarkus: soul s · project p (new)" }),
    nudge: async (input) => {
      if (input.event === "session-end") sessionEndCwds.push(input.cwd);
      return input.event === "recall" ? "Recall nudge" : input.event === "promote" ? "Promote nudge" : "Journal nudge";
    },
    gate: async (tool, input, directory, taint) => {
      gateCalls.push({ tool, input, directory });
      gateTaint.push(taint);
      return input.block ? { decision: "block", reason: "write denied" } : { decision: "warn", reason: "Tag this publish" };
    },
  });
  assert.deepEqual(servers.get("demarkus-memory"), { type: "local", command: ["bin", "mcp-serve"] });
  assert.deepEqual(notices, ["[demarkus-memory] bootstrap failed"]);
  const prompt = { sessionID: "s1", prompt: { text: "original" } };
  await hooks.get("prompt")!(prompt);
  assert.equal(prompt.prompt.text, "original");
  const context = { sessionID: "s1", system: [] as Array<{ type: string; text: string }> };
  const info = console.info;
  const statuses: string[] = [];
  console.info = (message: string) => { statuses.push(message); };
  try {
    await hooks.get("context")!(context);
  } finally {
    console.info = info;
  }
  assert.deepEqual(statuses, ["demarkus: soul s · project p (new)"]);
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
  assert.deepEqual(gateTaint.at(-1), []);
  // A web read taints the session: every later write carries it to the gate.
  await hooks.get("execute.after")!({ tool: "webfetch", sessionID: "s1", id: "web", input: { url: "https://evil.example" }, status: "completed", result: {} });
  await hooks.get("execute.before")!({ tool: "demarkus-memory_mark_archive", sessionID: "s1", id: "arch", input: { url: "/note.md" } });
  assert.deepEqual(gateTaint.at(-1), ["webfetch https://evil.example"]);

  await hooks.get("execute.after")!({ tool: "patch", sessionID: "s2", id: "edit", input: {}, status: "completed", result: {} });
  const idle = { type: "session.idle", data: { sessionID: "s2" } };
  if (nextEvent) { const resolve = nextEvent; nextEvent = undefined; resolve(idle); } else events.push(idle);
  await new Promise((resolve) => setImmediate(resolve));
  const nextPrompt = { sessionID: "s2", prompt: { text: "Continue" } };
  await hooks.get("prompt")!(nextPrompt);
  assert.equal(nextPrompt.prompt.text, "Continue");
  const nextContext = { sessionID: "s2", system: [] as Array<{ type: string; text: string }> };
  // A new session gets its own guidance call and status line.
  statuses.length = 0;
  console.info = (message: string) => { statuses.push(message); };
  try {
    await hooks.get("context")!(nextContext);
  } finally {
    console.info = info;
  }
  assert.deepEqual(statuses, ["demarkus: soul s · project p (new)"]);
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

test("V2 delivers every gated write's warn and hands an ask to the host prompt once it shows the order", async () => {
  const hooks = new Map<string, (event: any) => any>();
  const ctx: any = {
    location: { directory: "/test/project" },
    command: { list: async () => ({ data: [] }), transform: async (fn: any) => fn({ add: () => {} }) },
    skill: { transform: async (fn: any) => fn({ list: () => [], add: () => {} }) },
    mcp: { transform: async (fn: any) => fn({ get: () => undefined, set: () => {} }) },
    session: { hook: async (name: string, fn: any) => { hooks.set(name, fn); } },
    tool: { hook: async (name: string, fn: any) => { hooks.set(name, fn); } },
    permission: { hook: async (name: string, fn: any) => { hooks.set(`permission.${name}`, fn); } },
    event: { subscribe: async function* ({ signal }: { signal: AbortSignal }) {
      if (!signal.aborted) await new Promise<void>((resolve) => signal.addEventListener("abort", () => resolve(), { once: true }));
    } },
  };
  const stop = await setupV2(ctx, {
    legacy: async () => ({ config: async () => {}, event: async () => {}, dispose: () => {} }) as any,
    guidance: async () => null,
    nudge: async (input) => input.event === "promote" ? "Promote nudge" : "",
    gate: async (_tool, input) => typeof input.decision === "string"
      ? { decision: input.decision as "warn" | "ask", reason: "policy reason" }
      : { decision: "allow" },
  });
  try {
    const before = hooks.get("execute.before")!;
    const after = hooks.get("execute.after")!;
    const evaluate = hooks.get("permission.evaluate")!;
    const check = (id: string, tool: string) => ({ sessionID: "s1", action: tool, resources: ["*"], source: { type: "tool", messageID: "m", id }, effect: "allow", message: undefined as string | undefined });

    // A warn on a write that is not publish/append still reaches the result, without a nudge.
    const archive = { tool: "demarkus-memory_mark_archive", sessionID: "s1", id: "a1", input: { url: "/x.md", decision: "warn" } };
    await before(archive);
    const archived = { ...archive, status: "completed", result: { content: "Archived" } };
    await after(archived);
    assert.equal(archived.result.content, "Archived\n\n⚠️ policy reason");

    // Until a demarkus call's check follows its execute.before, an ask blocks.
    const publish = { tool: "demarkus-memory_mark_publish", sessionID: "s1", input: { url: "/x.md", decision: "ask", metadata: { tags: "a" } } };
    await assert.rejects(before({ ...publish, id: "p1" }), /policy reason Confirm with the user/);
    await after({ tool: "webfetch", sessionID: "s1", id: "w1", input: { url: "https://evil.example" }, status: "completed", result: {} });
    const read = { tool: "demarkus-memory_mark_fetch", sessionID: "s1", id: "r1", input: { url: "/x.md" } };
    await before(read);
    const readCheck = check("r1", read.tool);
    await evaluate(readCheck);
    assert.equal(readCheck.effect, "allow");
    await after({ ...read, status: "completed", result: { content: "doc" } });

    // Then the ask defers: the call is stamped and its check becomes the prompt.
    const call = { ...publish, id: "p2", input: { ...publish.input } };
    await before(call);
    assert.deepEqual(call.input.metadata, { tags: "a", "source-trust": "tainted", "source-trust-from": "webfetch https://evil.example" });
    const writeCheck = check("p2", call.tool);
    await evaluate(writeCheck);
    assert.deepEqual([writeCheck.effect, writeCheck.message], ["ask", "policy reason"]);
    const saved = { ...call, status: "completed", result: { content: "Saved" } };
    await after(saved);
    assert.equal(saved.result.content, "Saved\n\nPromote nudge");

    // An ask whose check never came reaches the result, and later asks block
    // until the order shows again.
    const errors: string[] = [];
    const original = console.error;
    console.error = (message: string) => { errors.push(message); };
    try {
      const skipped = { ...publish, id: "p3", input: { ...publish.input } };
      await before(skipped);
      const ran = { ...skipped, status: "completed", result: { content: "Saved" } };
      await after(ran);
      assert.match(ran.result.content, /⚠️ demarkus-memory_mark_publish ran without the approval prompt its ask needed; review this write with the user\. The ask: policy reason/);
      await assert.rejects(before({ ...publish, id: "p4", input: { ...publish.input } }), /policy reason Confirm with the user/);
    } finally {
      console.error = original;
    }
    assert.match(errors.join("\n"), /mark_publish ran without the approval prompt its ask needed/);
  } finally {
    stop();
  }
});
