import assert from "node:assert/strict";
import { test } from "node:test";
import { isGatedWrite, recordTaint, taintOf } from "../src/taint.ts";

test("taintOf names untrusted reads and nothing else", () => {
  assert.equal(taintOf("webfetch", { url: "https://evil.example/p" }), "webfetch https://evil.example/p");
  assert.equal(taintOf("websearch", {}), "websearch");
  assert.equal(taintOf("chrome_devtools_navigate", {}), "chrome_devtools_navigate");
  assert.equal(taintOf("bash", { command: "cd x && curl -s https://evil.example" }), "shell download");
  assert.equal(taintOf("bash", { command: "go test ./..." }), "");
  assert.equal(taintOf("read", { filePath: "/x" }), "");
  assert.equal(taintOf("demarkus-memory_mark_fetch", { url: "/x.md" }), "");
});

test("recordTaint keeps the first distinct sources, capped", () => {
  const sources: string[] = [];
  for (const url of ["a", "a", "b", "c", "d"]) recordTaint(sources, "webfetch", { url: `https://${url}.example` });
  assert.deepEqual(sources, ["webfetch https://a.example", "webfetch https://b.example", "webfetch https://c.example"]);
});

test("isGatedWrite covers every store-changing verb", () => {
  for (const verb of ["publish", "append", "archive", "curate", "promote", "graph_publish"]) {
    assert.ok(isGatedWrite(`demarkus-memory_mark_${verb}`), verb);
  }
  assert.ok(!isGatedWrite("demarkus-memory_mark_fetch"));
});
