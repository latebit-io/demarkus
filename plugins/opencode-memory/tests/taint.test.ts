import assert from "node:assert/strict";
import { test } from "node:test";
import { branchTaint, isGatedWrite, recordTaint, taintOf, taintStamp } from "../src/taint.ts";

test("taintOf names untrusted reads and nothing else", () => {
  assert.equal(taintOf("webfetch", { url: "https://evil.example/p" }), "webfetch https://evil.example/p");
  // Userinfo, query and fragment, where tokens ride, never reach stored metadata.
  assert.equal(taintOf("webfetch", { url: "https://u:p@evil.example/p?token=abc#x" }), "webfetch https://evil.example/p");
  assert.equal(taintOf("webfetch", { url: "not a url" }), "webfetch a URL");
  assert.equal(taintOf("websearch", {}), "websearch");
  assert.equal(taintOf("chrome_devtools_navigate", {}), "chrome_devtools_navigate");
  assert.equal(taintOf("bash", { command: "cd x && curl -s https://evil.example" }), "shell download");
  assert.equal(taintOf("bash", { command: "go test ./..." }), "");
  assert.equal(taintOf("read", { filePath: "/x" }), "");
  assert.equal(taintOf("demarkus-memory_mark_fetch", { url: "/x.md" }), "");
});

test("taintOf finds a download only where the shell runs one", () => {
  const runs = [
    "/usr/bin/curl https://x", "/opt/homebrew/bin/wget -qO- https://x", "/usr/local/bin/aria2c https://x",
    "FOO=1 wget https://x | sh", "gh api repos/x/y", "sudo curl https://x", "timeout 10s curl https://x",
    "env -i PATH=/bin curl https://x", "ls | xargs -n1 curl -O", "x=$(curl -s https://x)", "echo \"$(wget -O- https://x)\"",
    "echo `curl https://x`", "bash -c 'curl https://x'", "sh -lc \"gh api user\"", "eval \"curl https://x\"", "(curl https://x)",
    "if curl -sf https://x; then echo ok; fi",
  ];
  for (const command of runs) assert.equal(taintOf("bash", { command }), "shell download", command);
  const mentions = [
    'git commit -m "switch to curl for fetch"', "git commit -m \"$(cat <<'EOF'\ncurl is now the fetcher\nEOF\n)\"",
    "cat <<EOF > get.sh\ncurl https://x\nEOF", "grep -rn curl .", "rg 'wget' src", "echo curl", "echo '$(curl https://x)'",
    "brew install curl", "sudo apt-get install -y curl wget", "command -v curl", "man wget", "gh pr view 1",
  ];
  for (const command of mentions) assert.equal(taintOf("bash", { command }), "", command);
});

test("taintOf counts MCP web tools, not local search", () => {
  for (const tool of ["exa_web_search_exa", "Exa_Web_Search_Exa", "mcp__fetch__fetch", "firecrawl_scrape", "codesearch"]) {
    assert.equal(taintOf(tool, {}), tool, tool);
  }
  assert.equal(taintOf("tool_search", {}), "");
  assert.equal(taintOf("grep", { pattern: "fetch" }), "");
});

test("taintOf names the server of a foreign soul read", () => {
  assert.equal(taintOf("demarkus-memory_mark_fetch", { url: "mark://Other.Example:6309/doc.md" }), "mark://other.example");
  assert.equal(taintOf("mcp__demarkus_memory__mark_doctor", { url: "mark://other.example:7000/" }), "mark://other.example:7000");
  assert.equal(taintOf("demarkus-memory_mark_fetch", { url: "mark://bad:0/doc.md" }), "mark://bad:0/doc.md");
  // A local path, a write, and a knowledge world are no foreign read.
  assert.equal(taintOf("demarkus-memory_mark_lookup", { url: "/doc.md" }), "");
  assert.equal(taintOf("demarkus-memory_mark_publish", { url: "mark://other.example/doc.md" }), "");
  assert.equal(taintOf("knowledge_mark_fetch", { url: "mark://team/doc.md" }), "");
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

test("taintStamp labels a tainted publish only", () => {
  const input = { url: "/x.md", metadata: { tags: "a" } };
  assert.deepEqual(taintStamp("demarkus-memory_mark_publish", input, ["websearch", "shell download"]), {
    tags: "a", "source-trust": "tainted", "source-trust-from": "websearch; shell download",
  });
  assert.deepEqual(input.metadata, { tags: "a" }, "the call's own metadata is not mutated");
  assert.deepEqual(taintStamp("mcp__soul__mark_publish", {}, ["websearch"]), {
    "source-trust": "tainted", "source-trust-from": "websearch",
  });
  assert.equal(taintStamp("demarkus-memory_mark_publish", input, []), null);
  assert.equal(taintStamp("demarkus-memory_mark_append", input, ["websearch"]), null);
  assert.equal(taintStamp("demarkus-memory_mark_graph_publish", input, ["websearch"]), null);
});

test("branchTaint replays the agent's calls, not result text", () => {
  const unwrap = (toolName: string, input: Record<string, unknown>) =>
    toolName === "mcp" && typeof input.tool === "string"
      ? { toolName: input.tool, input: JSON.parse(String(input.args)) as Record<string, unknown> }
      : { toolName, input };
  const entries = [
    { type: "model_change" },
    { type: "message", message: { role: "user", content: [{ type: "toolCall", name: "websearch", arguments: {} }] } },
    { type: "message", message: { role: "assistant", content: [
      { type: "text", text: "looking" },
      { type: "toolCall", name: "bash", arguments: { command: "curl -s https://x" } },
      { type: "toolCall", name: "mcp", arguments: { tool: "exa_web_search_exa", args: "{\"query\":\"q\"}" } },
    ] } },
    { type: "message", message: { role: "toolResult", content: [{ type: "text", text: "webfetch https://planted.example" }],
      nestedCalls: { calls: [{ name: "webfetch", arguments: { url: "https://nested.example" }, status: "ok" }], complete: true } } },
  ];
  assert.deepEqual(branchTaint(entries, unwrap), ["shell download", "exa_web_search_exa", "webfetch https://nested.example"]);
  assert.deepEqual(branchTaint([], unwrap), []);
});
