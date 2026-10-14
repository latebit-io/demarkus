// Session activity tracker. The nudge DECISIONS (recall / promote / session-end
// text + gating) now live in the demarkus-plugin binary; pi only needs to track,
// during the session, whether real work happened without a journal entry — so it
// can hand those two signals to `demarkus-plugin nudge --event session-end` at
// shutdown. (Claude derives the same signals from its Stop transcript.)

// Memory writes can arrive wrapped in the pi-mcp-adapter "mcp" proxy tool, so
// unwrap input.tool before parsing the verb out of the tool name.
function isMemoryWrite(toolName: string, input: Record<string, unknown>): boolean {
  let name = toolName;
  if (name === "mcp" && typeof input.tool === "string") name = input.tool;
  return /mark_(publish|append)$/.test(name);
}

// Mirrors config.IsJournalWrite in the plugin binary: route today, or a url
// under a journal/ directory. Args arrive already unwrapped (normalizeToolCall).
function isJournalWrite(args: Record<string, unknown>): boolean {
  const url = typeof args.url === "string" ? args.url : "";
  return args.route === "today" || url.includes("/journal/");
}

export class SessionActivity {
  private journalWrite = false;
  private fileMutation = false;

  observe(toolName: string, input: Record<string, unknown> = {}): void {
    if (isMemoryWrite(toolName, input)) {
      if (isJournalWrite(input)) this.journalWrite = true;
      return;
    }
    if (/(^|_)(Edit|Write|NotebookEdit)$/.test(toolName) || ["edit", "write"].includes(toolName)) {
      this.fileMutation = true;
      this.journalWrite = false; // an edit after the entry leaves it stale
    }
  }

  get changedFiles(): boolean {
    return this.fileMutation;
  }
  get hadJournalWrite(): boolean {
    return this.journalWrite;
  }
}
