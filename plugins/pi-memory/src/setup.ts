// Server provisioning + MCP wiring for pi. On session start the extension runs
// the bundled provision.sh (downloads pinned binaries, generates a token, spawns
// the managed demarkus-server) and ensures pi-mcp-adapter has a config entry for
// the demarkus-memory MCP server pointing at the bundled mcp-wrapper.sh.
//
// pi-mcp-adapter reads MCP server configs from ~/.config/mcp/mcp.json (the
// generic global), so that is where we register — idempotently merging, never
// clobbering the user's other servers.

import { execFile } from "node:child_process";
import { existsSync, mkdirSync, readFileSync, renameSync, rmdirSync, writeFileSync } from "node:fs";
import { homedir } from "node:os";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const HERE = dirname(fileURLToPath(import.meta.url));
const SCRIPTS_DIR = join(HERE, "..", "scripts");
const PROVISION_SH = join(SCRIPTS_DIR, "provision.sh");
const MCP_WRAPPER_SH = join(SCRIPTS_DIR, "mcp-wrapper.sh");
const MCP_CONFIG = join(homedir(), ".config", "mcp", "mcp.json");
const MCP_SERVER_NAME = "demarkus-memory";

// provisionServer — run provision.sh; resolves to a warning string on failure
// (so the caller can surface it), or "" on success. Bounded so a hung download
// can't wedge session start indefinitely.
export function provisionServer(): Promise<string> {
  return new Promise((resolve) => {
    execFile("bash", [PROVISION_SH], { timeout: 120_000 }, (err, _stdout, stderr) => {
      if (err) {
        const tail = (stderr || "").trim().split("\n").slice(-3).join(" ");
        resolve(`demarkus-memory provisioning failed: ${tail || err.message}. Run /soul-init to recover.`);
        return;
      }
      resolve("");
    });
  });
}

export type EnsureResult =
  | { status: "unchanged" }
  | { status: "written" }
  | { status: "error"; message: string };

// Synchronous sleep (no busy-spin) for the brief lock retry loop.
function sleepSync(ms: number): void {
  Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, ms);
}

// withConfigLock — serialize read-modify-write of the shared global mcp.json via
// an atomic mkdir mutex, so two concurrent session starts (or another MCP config
// writer) can't clobber each other and drop server entries. Bounded (~1s) then
// reports an error rather than blocking forever on a stale lock.
function withConfigLock(fn: () => EnsureResult): EnsureResult {
  const lock = `${MCP_CONFIG}.lock`;
  try {
    mkdirSync(dirname(MCP_CONFIG), { recursive: true });
  } catch (e) {
    return { status: "error", message: `cannot create ${dirname(MCP_CONFIG)}: ${String((e as Error)?.message ?? e)}` };
  }
  for (let i = 0; i < 50; i++) {
    try {
      mkdirSync(lock); // atomic: succeeds only if we created it
    } catch (e) {
      if ((e as NodeJS.ErrnoException)?.code === "EEXIST") {
        sleepSync(20);
        continue;
      }
      return { status: "error", message: `lock error on ${lock}: ${String((e as Error)?.message ?? e)}` };
    }
    try {
      return fn();
    } finally {
      try {
        rmdirSync(lock);
      } catch {
        /* best-effort release */
      }
    }
  }
  return { status: "error", message: `could not acquire ${lock} (held by another process?)` };
}

// ensureMcpServerEntry — make sure ~/.config/mcp/mcp.json registers the
// demarkus-memory server via the bundled wrapper. Idempotent; writes only when
// the entry is missing or its command path changed. Returns a structured result
// so the caller can distinguish "already correct" from a real failure (rather
// than collapsing both into a bare false). The write is locked + atomic
// (temp file + rename) so a concurrent writer can't tear or drop entries.
export function ensureMcpServerEntry(): EnsureResult {
  return withConfigLock(() => {
    let config: { mcpServers?: Record<string, unknown> } = {};
    if (existsSync(MCP_CONFIG)) {
      try {
        config = JSON.parse(readFileSync(MCP_CONFIG, "utf8")) || {};
      } catch {
        // Unparseable config: don't clobber the user's file — surface it.
        return { status: "error", message: `${MCP_CONFIG} is not valid JSON; fix it by hand` };
      }
    }
    if (typeof config !== "object" || config === null) config = {};
    if (!config.mcpServers || typeof config.mcpServers !== "object") config.mcpServers = {};

    const existing = config.mcpServers[MCP_SERVER_NAME] as { command?: string } | undefined;
    if (existing && existing.command === MCP_WRAPPER_SH) return { status: "unchanged" };

    config.mcpServers[MCP_SERVER_NAME] = { command: MCP_WRAPPER_SH };
    try {
      const tmp = `${MCP_CONFIG}.${process.pid}.tmp`;
      writeFileSync(tmp, `${JSON.stringify(config, null, 2)}\n`);
      renameSync(tmp, MCP_CONFIG);
      return { status: "written" };
    } catch (e) {
      return { status: "error", message: `could not write ${MCP_CONFIG}: ${String((e as Error)?.message ?? e)}` };
    }
  });
}
