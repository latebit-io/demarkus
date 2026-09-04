// Server provisioning + MCP wiring for pi. On session start the extension runs
// the bundled provision.sh (downloads pinned binaries incl. demarkus-plugin,
// generates a token, spawns the managed demarkus-server), then registers the
// demarkus-memory MCP server by asking the binary to do it — `demarkus-plugin
// registry mcp add demarkus-memory <bin> mcp-serve` — so the config-write logic
// (locking, atomic write, array-shape guard) lives once in the binary instead of
// being duplicated here. pi-mcp-adapter then connects it from ~/.config/mcp/mcp.json.

import { execFile, execFileSync } from "node:child_process";
import { existsSync } from "node:fs";
import { homedir } from "node:os";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const HERE = dirname(fileURLToPath(import.meta.url));
const BOOTSTRAP_SH = join(HERE, "..", "scripts", "bootstrap.sh");
const BIN = join(homedir(), ".demarkus", "bin", "demarkus-plugin");
const MCP_SERVER_NAME = "demarkus-memory";

function run(cmd: string, args: string[]): Promise<string | null> {
  return new Promise((resolve) => {
    execFile(cmd, args, { timeout: 600_000 }, (err, _stdout, stderr) => {
      if (err) {
        const tail = (stderr || "").trim().split("\n").slice(-3).join(" ");
        resolve(tail || err.message);
        return;
      }
      resolve(null);
    });
  });
}

// provisionServer — ensure the demarkus-plugin binary is installed (bootstrap.sh),
// then run `demarkus-plugin provision` (download server/mcp/token, ensure config,
// spawn the managed server, seed). Resolves to a warning string on failure, "" on
// success. Bounded so a hung download can't wedge session start.
export async function provisionServer(): Promise<string> {
  const bootErr = await run("bash", [BOOTSTRAP_SH]);
  if (bootErr) return `demarkus-memory bootstrap failed: ${bootErr}. Run /soul-init to recover.`;
  const provErr = await run(BIN, ["provision"]);
  if (provErr) return `demarkus-memory provisioning failed: ${provErr}. Run /soul-init to recover.`;
  return "";
}

export type EnsureResult = { status: "ok" } | { status: "error"; message: string };

// ensureMcpServerEntry — register the local-memory MCP server (launched via
// `demarkus-plugin mcp-serve`) through the binary's registry command. Idempotent.
// Returns an error result if the binary isn't installed yet or the call fails.
export function ensureMcpServerEntry(): EnsureResult {
  if (!existsSync(BIN)) {
    return { status: "error", message: "demarkus-plugin not installed yet (provisioning may have failed)" };
  }
  try {
    execFileSync(BIN, ["registry", "mcp", "add", MCP_SERVER_NAME, BIN, "mcp-serve"], {
      encoding: "utf8",
      timeout: 10_000,
    });
    return { status: "ok" };
  } catch (e) {
    return { status: "error", message: String((e as Error)?.message ?? e) };
  }
}
