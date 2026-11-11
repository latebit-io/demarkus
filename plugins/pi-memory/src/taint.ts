// Untrusted reads a session made, for the gate's taint guard: once any is
// recorded, a demarkus write warns, or asks at ask. Mirrors taint.Of in the plugin binary
// (web, browser, shell download, foreign mark read); the binary cleans and caps.
const WEB_TOOL = /fetch|scrape|crawl|search/i;
// Local built-ins WEB_TOOL would catch.
const LOCAL_TOOL = /^tool_search$/i;
const BROWSER_TOOL = /chrome|browser|playwright|puppeteer|selenium/i;
const SHELL_TOOL = /^(?:bash|shell)$/i;
const MARK_TOOL = /(?:^|_)mark_[a-z_]+$/;
// The default soul's tools, or bare ones (the binary's local soul too); joined
// and aliased souls are known only to the binary.
const SOUL_TOOL = /^(?:(?:mcp__)?demarkus[-_]memory_{1,2})?mark_[a-z_]+$/i;
const DEFAULT_PORT = 6309;
const MAX_SOURCES = 3;

const DOWNLOADERS = new Set(["curl", "wget", "aria2c"]);
// Run the command after their options; WRAPPER_ARG skips a numeric option value.
const WRAPPERS = new Set(["sudo", "doas", "env", "exec", "nohup", "time", "nice", "timeout", "xargs", "stdbuf"]);
const WRAPPER_ARG = /^\d+(?:\.\d+)?[smhd]?$/;
const KEYWORDS = new Set(["if", "then", "elif", "else", "do", "while", "until", "!"]);
const SHELLS = new Set(["sh", "bash", "zsh", "dash", "ksh"]);
const ENV_ASSIGN = /^[A-Za-z_][A-Za-z0-9_]*=/;
const HEREDOC = /<<-?[ \t]*(['"]?)([A-Za-z_][A-Za-z0-9_]*)\1([^\n]*)\n(?:[^\n]*\n)*?[ \t]*\2[ \t]*(?:\n|$)/g;
const MAX_NESTING = 3;

// isGatedWrite matches every demarkus tool that changes a store.
export function isGatedWrite(toolName: string): boolean {
  return /mark_(?:publish|append|archive|curate|promote|graph_publish)$/.test(toolName);
}

// taintOf names the untrusted read one tool call is, or "".
export function taintOf(tool: string, args: Record<string, unknown> | undefined): string {
  const url = typeof args?.url === "string" ? args.url : "";
  if (MARK_TOOL.test(tool)) return foreignRead(tool, url);
  if (WEB_TOOL.test(tool) && !LOCAL_TOOL.test(tool)) return clip(url ? `${tool} ${publicURL(url)}` : tool);
  if (BROWSER_TOOL.test(tool)) return clip(tool);
  const command = typeof args?.command === "string" ? args.command : "";
  return SHELL_TOOL.test(tool) ? shellRead(command) : "";
}

// recordTaint adds a call's untrusted read to sources, first seen first.
export function recordTaint(sources: string[], tool: string, args: Record<string, unknown> | undefined): void {
  const s = taintOf(tool, args);
  if (s && !sources.includes(s) && sources.length < MAX_SOURCES) sources.push(s);
}

// taintStamp is a publish's metadata with the label the binary's ask path
// stamps (protocol.TaintKey), or null for another verb or no taint.
export function taintStamp(tool: string, input: Record<string, unknown>, sources: string[]): Record<string, unknown> | null {
  if (!/mark_publish$/.test(tool) || sources.length === 0) return null;
  const meta = isRecord(input.metadata) ? input.metadata : {};
  return { ...meta, "source-trust": "tainted", "source-trust-from": sources.join("; ") };
}

type Unwrap = (tool: string, args: Record<string, unknown>) => { toolName: string; input: Record<string, unknown> };

// branchTaint replays a pi session branch's untrusted reads: the agent's
// toolCall blocks and the nested calls pi records on a result, never its text.
export function branchTaint(entries: readonly unknown[], unwrap: Unwrap): string[] {
  const sources: string[] = [];
  const replay = (name: unknown, args: unknown) => {
    if (typeof name !== "string") return;
    const { toolName, input } = unwrap(name, isRecord(args) ? args : {});
    recordTaint(sources, toolName, input);
  };
  for (const entry of entries) {
    if (!isRecord(entry) || entry.type !== "message" || !isRecord(entry.message)) continue;
    const { role, content, nestedCalls } = entry.message;
    if (role === "assistant" && Array.isArray(content)) {
      for (const block of content) if (isRecord(block) && block.type === "toolCall") replay(block.name, block.arguments);
    } else if (role === "toolResult" && isRecord(nestedCalls) && Array.isArray(nestedCalls.calls)) {
      for (const call of nestedCalls.calls) if (isRecord(call)) replay(call.name, call.arguments);
    }
  }
  return sources;
}

// foreignRead names the server a soul read reached by full mark:// URL: the
// soul takes bare paths, so a host is someone else's.
function foreignRead(tool: string, url: string): string {
  if (isGatedWrite(tool) || !SOUL_TOOL.test(tool) || !url.startsWith("mark://")) return "";
  const m = /^mark:\/\/(?:[^@/?#]*@)?(\[[^\]/?#]*\]|[^:/?#]+)(?::(\d+))?(?=[/?#]|$)/.exec(url);
  const port = Number(m?.[2] ?? DEFAULT_PORT);
  if (!m || port < 1 || port > 65535) return clip(url);
  return clip(`mark://${m[1].toLowerCase()}${port === DEFAULT_PORT ? "" : `:${port}`}`);
}

// shellRead labels a downloader or gh api run at a command position (past env
// assignments, keywords, wrappers), in a substitution or a sh -c / eval script,
// by program only (arguments carry tokens). Quoted text or arguments: no run.
function shellRead(command: string, depth = 0): string {
  if (depth > MAX_NESTING) return "shell script too nested to check";
  const scripts: string[] = [];
  for (const words of commandsOf(command.replace(HEREDOC, "$3\n"), scripts)) {
    let i = 0;
    for (;;) {
      while (i < words.length && (ENV_ASSIGN.test(words[i]) || KEYWORDS.has(words[i]))) i++;
      if (!WRAPPERS.has(program(words[i]))) break;
      i++;
      while (i < words.length && (words[i].startsWith("-") || WRAPPER_ARG.test(words[i]))) i++;
    }
    const found = downloaderAt(words, i, scripts);
    if (found) return `shell download: ${found}`;
  }
  return scripts.reduce((found, script) => found || shellRead(script, depth + 1), "");
}

// downloaderAt names the downloader the command at s is, or ""; a script it
// runs goes to scripts.
function downloaderAt(words: string[], s: number, scripts: string[]): string {
  const name = program(words[s]);
  if (DOWNLOADERS.has(name)) return name;
  if (name === "gh" && words[s + 1] === "api") return "gh api";
  if (name === "eval") scripts.push(words.slice(s + 1).join(" "));
  if (SHELLS.has(name)) {
    const flag = words.findIndex((w, j) => j > s && /^-[a-zA-Z]*c[a-zA-Z]*$/.test(w));
    if (flag > 0 && flag + 1 < words.length) scripts.push(words[flag + 1]);
  }
  return "";
}

// program is a command word's base name: a path runs its last segment.
function program(word = ""): string {
  return word.slice(word.lastIndexOf("/") + 1).toLowerCase();
}

// commandsOf splits a shell line into simple commands of unquoted words;
// $(...) and `...` bodies, which run even inside double quotes, go to scripts.
function commandsOf(line: string, scripts: string[]): string[][] {
  const commands: string[][] = [];
  let words: string[] = [];
  let text = "";
  let open = false;
  let quote = "";
  const endWord = () => {
    if (open) words.push(text);
    text = "";
    open = false;
  };
  const endCommand = () => {
    endWord();
    if (words.length) commands.push(words);
    words = [];
  };
  for (let i = 0; i < line.length; i++) {
    const c = line[i];
    if (quote === "'") {
      if (c === "'") quote = "";
      else text += c;
      continue;
    }
    if (c === "`" || (c === "$" && line[i + 1] === "(")) {
      const end = c === "`" ? line.indexOf("`", i + 1) : closing(line, i + 1);
      const stop = end < 0 ? line.length : end;
      scripts.push(line.slice(c === "`" ? i + 1 : i + 2, stop));
      i = stop;
      open = true;
      continue;
    }
    if (quote === '"') {
      if (c === '"') quote = "";
      else text += c === "\\" && i + 1 < line.length ? line[++i] : c;
      continue;
    }
    if (c === "'" || c === '"') {
      quote = c;
      open = true;
    } else if (c === "\\" && i + 1 < line.length) {
      text += line[++i];
      open = true;
    } else if (c === " " || c === "\t") {
      endWord();
    } else if (";&|\n(){}".includes(c)) {
      endCommand();
    } else {
      text += c;
      open = true;
    }
  }
  endCommand();
  return commands;
}

// closing is the index of the ) matching the ( at open, or -1.
function closing(line: string, open: number): number {
  let depth = 0;
  for (let i = open; i < line.length; i++) {
    if (line[i] === "(") depth++;
    else if (line[i] === ")" && --depth === 0) return i;
  }
  return -1;
}

function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === "object" && v !== null && !Array.isArray(v);
}

// publicURL keeps a URL's scheme and host: tokens ride in userinfo, paths,
// queries and fragments, and taint labels are stored (Go's taint.publicURL).
function publicURL(raw: string): string {
  try {
    const u = new URL(raw);
    if (!u.host) return "a URL";
    return `${u.protocol}//${u.host}`;
  } catch {
    return "a URL";
  }
}

function clip(s: string): string {
  return s.replace(/\s+/g, " ").slice(0, 120);
}
