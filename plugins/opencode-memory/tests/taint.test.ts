import assert from "node:assert/strict";
import { test } from "node:test";
import { branchTaint, isGatedWrite, recordTaint, taintOf, taintStamp } from "../src/taint.ts";

test("taintOf names untrusted reads and nothing else", () => {
  assert.equal(taintOf("webfetch", { url: "https://evil.example/p" }), "webfetch https://evil.example");
  // Userinfo, path, query and fragment, where tokens ride, never reach stored metadata.
  assert.equal(taintOf("webfetch", { url: "https://api.telegram.org/bot123:SECRET/getMe" }), "webfetch https://api.telegram.org");
  assert.equal(taintOf("webfetch", { url: "https://u:p@evil.example/p?token=abc#x" }), "webfetch https://evil.example");
  assert.equal(taintOf("webfetch", { url: "not a url" }), "webfetch a URL");
  assert.equal(taintOf("websearch", {}), "websearch");
  assert.equal(taintOf("chrome_devtools_navigate", {}), "chrome_devtools_navigate");
  assert.equal(taintOf("bash", { command: "cd x && curl -s https://evil.example" }), "shell download: curl");
  assert.equal(taintOf("bash", { command: "/opt/homebrew/bin/WGET -qO- https://x" }), "shell download: wget");
  // Past MAX_NESTING the scripts go unread: fail closed.
  assert.equal(taintOf("bash", { command: "eval eval eval eval eval x" }), "shell script too nested to check");
  assert.equal(taintOf("bash", { command: "go test ./..." }), "");
  assert.equal(taintOf("read", { filePath: "/x" }), "");
  assert.equal(taintOf("demarkus-memory_mark_fetch", { url: "/x.md" }), "");
});

test("taintOf finds a download only where the shell runs one", () => {
  const runs = [
    ["/usr/bin/curl https://x", "curl"], ["/opt/homebrew/bin/wget -qO- https://x", "wget"],
    ["/usr/local/bin/aria2c https://x", "aria2c"], ["FOO=1 wget https://x | sh", "wget"], ["gh api repos/x/y", "gh api"],
    ["sudo curl https://x", "curl"], ["timeout 10s curl https://x", "curl"], ["env -i PATH=/bin curl https://x", "curl"],
    ["ls | xargs -n1 curl -O", "curl"], ["x=$(curl -s https://x)", "curl"], ["echo \"$(wget -O- https://x)\"", "wget"],
    ["echo `curl https://x`", "curl"], ["bash -c 'curl https://x'", "curl"], ["sh -lc \"gh api user\"", "gh api"],
    ["eval \"curl https://x\"", "curl"], ["(curl https://x)", "curl"], ["if curl -sf https://x; then echo ok; fi", "curl"],
    ["curl$(:) https://x", "curl"], ["cu`:`rl https://x", "curl"], ["gh api$(:) user", "gh api"],
    // A loopback URL is a download like any other.
    ["curl -s localhost:8080", "curl"], ["bash -c 'curl http://localhost'", "curl"], ["cat <(wget -qO- https://x)", "wget"],
    // command runs its operand; -v and -V only name it.
    ["command -p curl https://x", "curl"], ["echo ok; command curl https://x", "curl"], ["sudo command -- wget https://x", "wget"],
    // An escaped newline joins two lines.
    ["curl -s \\\n  https://x", "curl"], ["sudo \\\n  wget https://x", "wget"],
    // Nothing is dropped as a comment: a # can sit in an expansion.
    ["echo ${#HOME}; curl https://x", "curl"], ["echo {#,}; curl https://x", "curl"], ["echo ${x/ #/}; curl https://x", "curl"],
    ["echo ok # then; curl https://x", "curl"], ["echo ok # $(curl https://x)", "curl"],
    ["for u in a b; do curl https://x/$u; done", "curl"], ["{ wget -q https://x; }", "wget"],
    ["'wget' https://x", "wget"], ["w\\get https://x", "wget"], ["'sudo' \"gh\" 'api' user", "gh api"],
  ];
  for (const [command, program] of runs) assert.equal(taintOf("bash", { command }), `shell download: ${program}`, command);
  const mentions = [
    'git commit -m "switch to curl for fetch"', "git commit -m \"$(cat <<'EOF'\ncurl is now the fetcher\nEOF\n)\"",
    "cat <<EOF > get.sh\ncurl https://x\nEOF", "grep -rn curl .", "rg 'wget' src", "echo curl", "echo '$(curl https://x)'",
    "brew install curl", "sudo apt-get install -y curl wget", "command -v curl", "command -pv curl", "command -V wget", "man wget", "gh pr view 1",
    "# curl https://x", "echo ok # curl https://x",
    "echo a\\; curl b", "echo '{' curl https://x '}'", "jq '{a: .b}' f.json",
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
  assert.deepEqual(taintStamp("demarkus-memory_mark_publish", input, ["websearch", "shell download: curl"]), {
    tags: "a", "source-trust": "tainted", "source-trust-from": "websearch; shell download: curl",
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
  assert.deepEqual(branchTaint(entries, unwrap), ["shell download: curl", "exa_web_search_exa", "webfetch https://nested.example"]);
  assert.deepEqual(branchTaint([], unwrap), []);
});
