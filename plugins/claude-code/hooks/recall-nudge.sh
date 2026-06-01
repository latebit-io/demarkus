#!/usr/bin/env bash
# UserPromptSubmit hook for the demarkus-memory plugin.
#
# When the user's prompt reads like a RECALL question ("did we...", "what did we
# decide", "last time", "do we have a note on..."), inject a discreet reminder to
# consult the soul before answering. Recall is the one quality behavior with no
# other backstop — the publish gate only fires on writes, and the SessionStart
# "recall first" guidance decays as a session grows. This re-asserts it at the
# exact moment a recall question is asked.
#
# Non-blocking: emits additionalContext, never decision:block — a nudge, not a
# gate. The pattern is deliberately tight (multi-word, recall-specific) to keep
# false positives low; a stray nudge costs only one discreet line. Pure grep/tr,
# no deps. Fails open (no nudge) on anything unexpected. If it proves noisy in
# practice, tighten the pattern or add a disable knob (deferred until there's
# signal it's needed).

set -uo pipefail

HOOK_DIR="$(cd "$(dirname "$0")" && pwd)"
SCRIPTS_DIR="${HOOK_DIR}/../scripts"
# shellcheck source=../scripts/lib.sh
. "${SCRIPTS_DIR}/lib.sh"

# No point nudging toward a memory store that isn't configured yet.
[[ -f "${PLUGIN_CONFIG}" ]] || exit 0

input="$(cat)"
lc="$(printf '%s' "${input}" | tr '[:upper:]' '[:lower:]')"

# Recall-intent patterns — multi-word and specific so ordinary questions don't
# trip them. Matched against the lowercased payload; only the `prompt` field
# carries prose, so the uuid/path/mode fields won't false-match these phrases.
recall_re='\bdid we\b|\bhave we\b|\bhad we\b|\bwhat did we\b|\bwhy did we\b|\bhow did we\b|\bwhat( is| was|s)? our\b|\bdo we (have|know)\b|\bwe (decided|agreed|chose|discussed|already)\b|\blast time\b|\bpreviously\b|\brecall\b|\bis there (a |an |any )?(adr|decision|note|record)\b'

printf '%s' "${lc}" | grep -qE "${recall_re}" || exit 0

nudge="This reads like a recall question. Before answering from this conversation alone, check the soul first: mark_lookup the project for the subject (and mark_fetch the project /index.md), then answer from what's recorded — or say plainly if nothing is there."

printf '{"hookSpecificOutput":{"hookEventName":"UserPromptSubmit","additionalContext":%s}}\n' \
  "$(printf '%s' "${nudge}" | json_escape)"
