// Canonical copy; scripts/check-identical-copies.sh checks the knowledge copy.

// The fields of an OpenCode V2 permission.evaluate event this reads and sets.
export interface Evaluation {
  readonly sessionID: string;
  readonly source?: { readonly type: string; readonly id: string };
  effect: "allow" | "deny" | "ask";
  message?: string;
}

// HostAsks hands a gate ask to OpenCode V2's permission prompt: evaluate turns
// the MCP tool's own check, which follows execute.before, into an ask. Until a
// demarkus call shows that order, defer refuses and the caller blocks.
export class HostAsks {
  private ordered = false;
  // Calls past execute.before awaiting their permission check: the ask's
  // reason, or "" for a call that only shows the order.
  private readonly pending = new Map<string, string>();

  // expect notes a demarkus call the gate let through.
  expect(tool: string, sessionID: string, callID: string): void {
    const key = `${sessionID}:${callID}`;
    if (/(?:^|_)mark_[a-z_]+$/.test(tool) && !this.pending.has(key)) this.pending.set(key, "");
  }

  // defer queues an ask for the call's permission check; false = block.
  defer(sessionID: string, callID: string, reason: string): boolean {
    if (!this.ordered) return false;
    this.pending.set(`${sessionID}:${callID}`, reason);
    return true;
  }

  evaluate(event: Evaluation): void {
    if (event.source?.type !== "tool") return;
    const key = `${event.sessionID}:${event.source.id}`;
    const reason = this.pending.get(key);
    if (reason === undefined) return;
    this.pending.delete(key);
    this.ordered = true;
    if (!reason) return;
    event.effect = "ask";
    event.message = reason;
  }

  // settle forgets a finished call; a reason back = its ask never prompted,
  // so the order no longer holds and later asks block until it shows again.
  settle(sessionID: string, callID: string): string {
    const key = `${sessionID}:${callID}`;
    const reason = this.pending.get(key) ?? "";
    this.pending.delete(key);
    if (reason) this.ordered = false;
    return reason;
  }

  dropSession(sessionID: string): void {
    for (const key of this.pending.keys()) if (key.startsWith(`${sessionID}:`)) this.pending.delete(key);
  }
}

// unpromptedWarning words a write that ran though its ask never reached the
// host prompt: the user and agent review it after the fact.
export function unpromptedWarning(tool: string, reason: string): string {
  return `${tool} ran without the approval prompt its ask needed; review this write with the user. The ask: ${reason}`;
}
