// Recall nudge, ported from hooks/recall-nudge.sh. Gated on at least one joined
// knowledge system; with none, the plugin stays silent and leaves recall to the
// soul (demarkus-memory, if installed).

import { listKnowledgeSystems } from "./config.js";

const RECALL_RE =
  /\bdid we\b|\bhave we\b|\bhad we\b|\bwhat did we\b|\bwhy did we\b|\bhow did we\b|\bwhat( is| was|s)? our\b|\bdo we (have|know)\b|\bwe (decided|agreed|chose|discussed|already)\b|\blast time\b|\bpreviously\b|\brecall\b|\bis there (a |an |any )?(adr|decision|note|record)\b|\bwhat does the (org|team|company)\b|\b(our|team|org|company|agreed|approved) (standard|convention)s?\b/;

export const RECALL_NUDGE =
  "This reads like a recall question. If it concerns shared or organizational knowledge, check the joined demarkus " +
  "knowledge system first: mark_lookup the system for the subject (and fetch the relevant world's index.md / the " +
  "root hub), then answer from what's recorded — or say plainly if nothing is there.";

export function recallNudge(prompt: string): string | null {
  if (listKnowledgeSystems().length === 0) return null;
  return RECALL_RE.test(prompt.toLowerCase()) ? RECALL_NUDGE : null;
}
