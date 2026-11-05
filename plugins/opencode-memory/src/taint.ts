// Untrusted reads a session made, for the gate's taint guard: once any is
// recorded, a demarkus write asks first. A subset of taint.Of in the plugin
// binary (web, browser, download tools); the binary cleans and caps.
const WEB_TOOL = /^(?:webfetch|websearch|web_fetch|web_search|fetch)$/i;
const BROWSER_TOOL = /chrome|browser|playwright|puppeteer|selenium/i;
const SHELL_TOOL = /^(?:bash|shell)$/i;
const DOWNLOAD = /(?:^|[\s;|&(])(?:curl|wget|aria2c)\s|\bgh\s+api\b/;
const MAX_SOURCES = 3;

// isGatedWrite matches every demarkus tool that changes a store.
export function isGatedWrite(toolName: string): boolean {
  return /mark_(?:publish|append|archive|curate|promote|graph_publish)$/.test(toolName);
}

// taintOf names the untrusted read one tool call is, or "".
export function taintOf(tool: string, args: Record<string, unknown> | undefined): string {
  if (/mark_[a-z_]+$/.test(tool)) return "";
  const url = typeof args?.url === "string" ? args.url : "";
  if (WEB_TOOL.test(tool)) return clip(url ? `${tool} ${url}` : tool);
  if (BROWSER_TOOL.test(tool)) return clip(tool);
  const command = typeof args?.command === "string" ? args.command : "";
  if (SHELL_TOOL.test(tool) && DOWNLOAD.test(command)) return "shell download";
  return "";
}

// recordTaint adds a call's untrusted read to sources, first seen first.
export function recordTaint(sources: string[], tool: string, args: Record<string, unknown> | undefined): void {
  const s = taintOf(tool, args);
  if (s && !sources.includes(s) && sources.length < MAX_SOURCES) sources.push(s);
}

function clip(s: string): string {
  return s.replace(/\s+/g, " ").slice(0, 120);
}
