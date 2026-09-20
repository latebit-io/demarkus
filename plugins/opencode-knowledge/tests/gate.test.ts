import assert from "node:assert/strict";
import { chmodSync, mkdtempSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";
import { callGate } from "../src/demarkus-knowledge.ts";

function helper(script: string): string {
  const bin = join(mkdtempSync(join(tmpdir(), "demarkus-gate-")), "demarkus-plugin");
  writeFileSync(bin, `#!/bin/sh\ncat >/dev/null\n${script}\n`);
  chmodSync(bin, 0o755);
  return bin;
}

test("gate allows only when the helper is absent", async () => {
  const d = await callGate("mark_publish", {}, "/tmp", join(tmpdir(), "demarkus-absent", "demarkus-plugin"));
  assert.equal(d.decision, "allow");
});

test("gate passes the helper's verdict through", async () => {
  const d = await callGate("mark_publish", {}, "/tmp", helper(`echo '{"decision":"block","reason":"no tags"}'`));
  assert.deepEqual(d, { decision: "block", reason: "no tags" });
});

for (const [name, script] of [
  ["crash", "exit 3"],
  ["empty output", "exit 0"],
  ["bad JSON", "echo not-json"],
  ["no decision field", `echo '{"reason":"x"}'`],
] as const) {
  test(`gate blocks on helper ${name}`, async () => {
    const d = await callGate("mark_publish", {}, "/tmp", helper(script));
    assert.equal(d.decision, "block");
    assert.match(d.reason ?? "", /gate failed/);
  });
}
