// Server provisioning + MCP wiring for pi. On session start the extension runs
// the bundled provision.sh (downloads pinned binaries, generates a token, spawns
// the managed demarkus-server) and ensures pi-mcp-adapter has a config entry for
// the demarkus-memory MCP server pointing at the bundled mcp-wrapper.sh.
//
// pi-mcp-adapter reads MCP server configs from ~/.config/mcp/mcp.json (the
// generic global), so that is where we register — idempotently merging, never
// clobbering the user's other servers.

import { execFile } from "node:child_process";
import { existsSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
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

// ensureMcpServerEntry — make sure ~/.config/mcp/mcp.json registers the
// demarkus-memory server via the bundled wrapper. Idempotent; only writes when
// the entry is missing or its command path changed. Returns true if it wrote.
export function ensureMcpServerEntry(): boolean {
  let config: { mcpServers?: Record<string, unknown> } = {};
  if (existsSync(MCP_CONFIG)) {
    try {
      config = JSON.parse(readFileSync(MCP_CONFIG, "utf8")) || {};
    } catch {
      // Unparseable config: don't clobber the user's file — bail and let them fix it.
      return false;
    }
  }
  if (typeof config !== "object" || config === null) config = {};
  if (!config.mcpServers || typeof config.mcpServers !== "object") config.mcpServers = {};

  const existing = config.mcpServers[MCP_SERVER_NAME] as { command?: string } | undefined;
  if (existing && existing.command === MCP_WRAPPER_SH) return false;

  config.mcpServers[MCP_SERVER_NAME] = { command: MCP_WRAPPER_SH };
  try {
    mkdirSync(dirname(MCP_CONFIG), { recursive: true });
    writeFileSync(MCP_CONFIG, `${JSON.stringify(config, null, 2)}\n`);
    return true;
  } catch {
    return false;
  }
}
