#!/usr/bin/env node
// mcp-config.mjs — add/remove/list demarkus MCP servers in the pi-mcp-adapter
// config (~/.config/mcp/mcp.json, the generic global config pi-mcp-adapter
// reads). The pi equivalent of `claude mcp add/remove/list`.
//
//   - `add` registers a stdio server (a command + args) — used for managed
//     remote souls, where a launch wrapper injects the token from a 0600 file.
//   - `add-http` registers an HTTP/broker server by URL; pi-mcp-adapter
//     auto-detects OAuth and runs the auth flow on first use (no token stored
//     here) — used for organizational knowledge systems.
//
// Mutations take an atomic mkdir lock and write via temp-file + rename, so two
// concurrent invocations (or another MCP config writer) can't clobber each other
// and drop server entries.
//
// Usage:
//   node mcp-config.mjs add <name> <command> [args...]
//   node mcp-config.mjs add-http <name> <url>
//   node mcp-config.mjs remove <name>
//   node mcp-config.mjs list

import { existsSync, mkdirSync, readFileSync, renameSync, rmdirSync, writeFileSync } from "node:fs";
import { homedir } from "node:os";
import { dirname, join } from "node:path";

const CONFIG = join(homedir(), ".config", "mcp", "mcp.json");
const LOCK = `${CONFIG}.lock`;

function load() {
  if (!existsSync(CONFIG)) return { mcpServers: {} };
  try {
    const obj = JSON.parse(readFileSync(CONFIG, "utf8")) || {};
    if (typeof obj !== "object") return { mcpServers: {} };
    if (!obj.mcpServers || typeof obj.mcpServers !== "object") obj.mcpServers = {};
    return obj;
  } catch (e) {
    console.error(`FAIL: ${CONFIG} is not valid JSON (${e.message}); fix it by hand before retrying`);
    process.exit(1);
  }
}

function save(config) {
  const tmp = `${CONFIG}.${process.pid}.tmp`;
  writeFileSync(tmp, `${JSON.stringify(config, null, 2)}\n`);
  renameSync(tmp, CONFIG);
}

function sleepSync(ms) {
  Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, ms);
}

// Run fn while holding an atomic mkdir lock around the whole read-modify-write.
function withLock(fn) {
  mkdirSync(dirname(CONFIG), { recursive: true });
  for (let i = 0; i < 50; i++) {
    try {
      mkdirSync(LOCK);
    } catch (e) {
      if (e?.code === "EEXIST") {
        sleepSync(20);
        continue;
      }
      console.error(`FAIL: lock error on ${LOCK} (${e.message})`);
      process.exit(1);
    }
    try {
      return fn();
    } finally {
      try {
        rmdirSync(LOCK);
      } catch {
        /* best-effort release */
      }
    }
  }
  console.error(`FAIL: could not acquire ${LOCK} (held by another process?)`);
  process.exit(1);
}

const [, , cmd, name, command, ...args] = process.argv;

switch (cmd) {
  case "add": {
    if (!name || !command) {
      console.error("usage: mcp-config.mjs add <name> <command> [args...]");
      process.exit(1);
    }
    withLock(() => {
      const config = load();
      config.mcpServers[name] = args.length ? { command, args } : { command };
      save(config);
    });
    console.log(`OK: registered MCP server '${name}' in ${CONFIG}`);
    break;
  }
  case "add-http": {
    const url = command; // third positional is the URL for add-http
    if (!name || !url) {
      console.error("usage: mcp-config.mjs add-http <name> <url>");
      process.exit(1);
    }
    withLock(() => {
      const config = load();
      config.mcpServers[name] = { url, auth: "oauth" };
      save(config);
    });
    console.log(`OK: registered HTTP MCP server '${name}' (${url}) in ${CONFIG}`);
    break;
  }
  case "remove": {
    if (!name) {
      console.error("usage: mcp-config.mjs remove <name>");
      process.exit(1);
    }
    const removed = withLock(() => {
      const config = load();
      if (!config.mcpServers[name]) return false;
      delete config.mcpServers[name];
      save(config);
      return true;
    });
    console.log(removed ? `OK: removed MCP server '${name}' from ${CONFIG}` : `OK: no MCP server named '${name}' (nothing to remove)`);
    break;
  }
  case "list": {
    const config = load();
    const names = Object.keys(config.mcpServers);
    console.log(names.length ? names.join("\n") : "(no MCP servers configured)");
    break;
  }
  default:
    console.error("usage: mcp-config.mjs add|add-http|remove|list ...");
    process.exit(1);
}
