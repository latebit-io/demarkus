// Session activity tracker. The nudge DECISIONS (recall / promote / session-end
// text + gating) now live in the demarkus-plugin binary; pi only needs to track,
// during the session, whether real work happened without a soul write — so it can
// hand those two signals to `demarkus-plugin nudge --event session-end` at
// shutdown. (Claude derives the same signals from its Stop transcript.)

// Parse the verb out of an MCP tool name (incl. the pi-mcp-adapter proxy isn't
// needed here — soul writes arrive as the proxy "mcp" tool too, so match on the
// underlying name when present).
function isSoulWrite(toolName: string, input: Record<string, unknown>): boolean {
  let name = toolName;
  if (name === "mcp" && typeof input.tool === "string") name = input.tool;
  return /mark_(publish|append)$/.test(name);
}

export class SessionActivity {
  private soulWrite = false;
  private fileMutation = false;

  observe(toolName: string, input: Record<string, unknown> = {}): void {
    if (isSoulWrite(toolName, input)) {
      this.soulWrite = true;
      return;
    }
    if (/(^|_)(Edit|Write|NotebookEdit)$/.test(toolName) || ["edit", "write"].includes(toolName)) {
      this.fileMutation = true;
    }
  }

  get changedFiles(): boolean {
    return this.fileMutation;
  }
  get hadSoulWrite(): boolean {
    return this.soulWrite;
  }
}
