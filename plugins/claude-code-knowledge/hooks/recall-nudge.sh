#!/usr/bin/env bash
# UserPromptSubmit hook for the demarkus-knowledge plugin.
#
# When the user's prompt reads like a RECALL question and at least one knowledge
# system is joined, inject a discreet reminder to consult the shared catalog
# before answering. The SessionStart guidance says the same, but it decays as a
# session grows; this re-asserts it at the moment a recall question is asked.
#
# Gated on a joined system: with no knowledge system registered this plugin has
# nothing to add, so it stays silent and leaves recall entirely to the soul
# (the demarkus-memory plugin, if installed). Non-blocking: emits
# additionalContext, never a decision:block. Pure grep/tr, no deps. Fails open.

set -uo pipefail

HOOK_DIR="$(cd "$(dirname "$0")" && pwd)"
SCRIPTS_DIR="${HOOK_DIR}/../scripts"
# shellcheck source=../scripts/lib.sh
. "${SCRIPTS_DIR}/lib.sh"

# Nothing to nudge toward unless a knowledge system is actually joined.
[[ -n "$(list_knowledge_systems)" ]] || exit 0

input="$(cat)"
lc="$(printf '%s' "${input}" | tr '[:upper:]' '[:lower:]')"

# Recall-intent patterns — multi-word and specific so ordinary questions don't
# trip them. Matched against the lowercased payload; only the `prompt` field
# carries prose, so the uuid/path/mode fields won't false-match these phrases.
recall_re='\bdid we\b|\bhave we\b|\bhad we\b|\bwhat did we\b|\bwhy did we\b|\bhow did we\b|\bwhat( is| was|s)? our\b|\bdo we (have|know)\b|\bwe (decided|agreed|chose|discussed|already)\b|\blast time\b|\bpreviously\b|\brecall\b|\bis there (a |an |any )?(adr|decision|note|record)\b|\bwhat does the (org|team|company)\b|\b(our|team|org|company|agreed|approved) (standard|convention)s?\b'

printf '%s' "${lc}" | grep -qE "${recall_re}" || exit 0

nudge="This reads like a recall question. If it concerns shared or organizational knowledge, check the joined demarkus knowledge system first: mark_lookup the system for the subject (and fetch the relevant world's index.md / the root hub), then answer from what's recorded — or say plainly if nothing is there."

printf '{"hookSpecificOutput":{"hookEventName":"UserPromptSubmit","additionalContext":%s}}\n' \
  "$(printf '%s' "${nudge}" | json_escape)"
