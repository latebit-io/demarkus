// Soft nudges, ported from hooks/recall-nudge.sh, promote-nudge.sh, and
// session-journal-nudge.sh. All non-blocking — they surface a reminder, never
// stop work.

import { localSoulPresent, parseMcpTool, promoteDestinationPresent, publishGateScope } from "./config.js";

// Recall-intent patterns — multi-word and specific so ordinary questions don't
// trip them (mirrors recall-nudge.sh's regex).
const RECALL_RE =
  /\bdid we\b|\bhave we\b|\bhad we\b|\bwhat did we\b|\bwhy did we\b|\bhow did we\b|\bwhat( is| was|s)? our\b|\bdo we (have|know)\b|\bwe (decided|agreed|chose|discussed|already)\b|\blast time\b|\bpreviously\b|\brecall\b|\bis there (a |an |any )?(adr|decision|note|record)\b/;

export const RECALL_NUDGE =
  "This reads like a recall question. Before answering from this conversation alone, check the soul first: " +
  "mark_lookup the project for the subject (and mark_fetch the project /index.md), then answer from what's " +
  "recorded — or say plainly if nothing is there.";

// recallNudge — returns the nudge text when PROMPT reads like recall and a soul
// is configured, else null. (No point nudging toward a store that isn't set up.)
export function recallNudge(prompt: string): string | null {
  if (!localSoulPresent()) return null;
  return RECALL_RE.test(prompt.toLowerCase()) ? RECALL_NUDGE : null;
}

// promoteNudge — returns the nudge when a freshly-published doc is a high-signal
// ADR worth promoting, else null. Fires only when: a promote destination exists,
// the tool targets this plugin's own soul, the url is an /adr/*.md, and the body
// is not already back-stamped as promoted.
export function promoteNudge(toolName: string, input: Record<string, unknown>): string | null {
  if (!promoteDestinationPresent()) return null;
  const parsed = parseMcpTool(toolName);
  if (!parsed || parsed.verb !== "publish") return null;
  if (publishGateScope(toolName) !== "local") return null;

  const url = typeof input.url === "string" ? input.url : "";
  if (!/\/adr\/.*\.md$/.test(url)) return null;

  const body = typeof input.body === "string" ? input.body : "";
  if (/promoted: mark:\/\//.test(body)) return null;

  return (
    `New ADR published to the soul (${url}). ADRs are high-signal, shared-team knowledge: if this decision is ` +
    `durable and useful beyond you, /promote ${url} to lift it into the knowledge system (curate → gate → ` +
    "publish). If it is personal or provisional, leave it here — most soul content correctly stays put."
  );
}

// --- session journal nudge (session-end) -------------------------------------
//
// Claude's Stop hook can force one more turn to make the agent journal; pi's
// session_shutdown cannot. So we track the session's shape and, at shutdown,
// surface a reminder when the session changed files but recorded nothing to the
// soul. It can't force the journal entry the way the Claude hook does, but it
// makes the omission visible.

export class SessionActivity {
  private soulWrite = false;
  private fileMutation = false;

  observe(toolName: string): void {
    const parsed = parseMcpTool(toolName);
    if (parsed && (parsed.verb === "publish" || parsed.verb === "append")) {
      this.soulWrite = true;
      return;
    }
    if (/(^|_)(Edit|Write|NotebookEdit)$/.test(toolName) || ["edit", "write"].includes(toolName)) {
      this.fileMutation = true;
    }
  }

  // journalNudge — the reminder text when work happened without a soul write,
  // else null. Only meaningful when a soul is configured.
  journalNudge(): string | null {
    if (!localSoulPresent()) return null;
    if (!this.fileMutation || this.soulWrite) return null;
    return (
      "This session changed files but recorded nothing to the soul. If something here is worth remembering — " +
      "a decision, a gotcha, a non-obvious why — capture it with /soul-journal (or mark_append to today's journal " +
      "under /<project>/journal/<YYYY-MM-DD>.md). If it's all routine, ignore this."
    );
  }
}
