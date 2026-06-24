// Shared ~/.demarkus paths and state files for the demarkus-memory pi extension.
//
// These point at the SAME state the claude-code demarkus-memory plugin and the
// bundled bash scripts (scripts/lib.sh) read and write — so the pi extension
// interoperates with them: one soul, one token, one set of registries. The TS
// here only READS this state (plus the config file); all mutation of the
// registries/server lifecycle stays in the bundled bash (scripts/*.sh), which
// is the single source of truth for provisioning.

import { homedir } from "node:os";
import { join } from "node:path";

export const PLUGIN_HOME = join(homedir(), ".demarkus");
export const PLUGIN_BIN_DIR = join(PLUGIN_HOME, "bin");
export const PLUGIN_CONFIG = join(PLUGIN_HOME, "plugin-memory.conf");
export const PLUGIN_TOKEN_FILE = join(PLUGIN_HOME, "plugin-memory.token");

// Tag-gate strictness: warn (default) | block | ask. Orthogonal file so a
// setup.sh rewrite of the conf never clobbers a chosen strictness.
export const PLUGIN_STRICTNESS_FILE = join(PLUGIN_HOME, "plugin-memory.strictness");
// Destination-gate strictness: warn | block | ask. Default block.
export const PLUGIN_DEST_STRICTNESS_FILE = join(PLUGIN_HOME, "plugin-memory.dest-strictness");

// Registry of joined knowledge-system MCP slugs (written by demarkus-knowledge).
export const KNOWLEDGE_REGISTRY = join(PLUGIN_HOME, "knowledge-systems");
// Catalog of joined remote souls (written by /soul-join), tab-separated rows
// "<slug>\t<host>\t<insecure>\t<token-file>".
export const SOULS_REGISTRY = join(PLUGIN_HOME, "souls");
// Per-project binding "<project-dir>\t<slug>".
export const PROJECT_SOULS = join(PLUGIN_HOME, "project-souls");
// Plain remote promote targets "<mcp-slug> <write-path> [label]".
export const PROMOTE_TARGETS = join(PLUGIN_HOME, "promote-targets");

// Local managed soul's reserved catalog id / binding slug.
export const LOCAL_SOUL_ID = "demarkus-memory";
