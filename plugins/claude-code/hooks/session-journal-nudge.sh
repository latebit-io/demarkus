#!/usr/bin/env bash
# Stop hook for the demarkus-memory plugin.
#
# Journaling a significant session is a quality behavior the gate can't backstop
# (the publish gate only fires when you DO write). This nudges once, at session
# end, when the session changed files but recorded nothing to any demarkus
# memory store — so substantive work isn't silently lost to the next session.
#
# Stop hooks can only surface a message by blocking (decision:block forces one
# more turn); there is no non-blocking context channel. So the nudge blocks
# exactly once and hands the agent an explicit "if it's routine, just stop" out,
# honoring the don't-journal-trivia restraint. Two independent loop guards:
#   1. stop_hook_active == true  → already mid stop-cycle, never re-block.
#   2. a per-session sentinel file → fire at most once per session_id, even if
#      the block triggers another Stop after the agent journals.
#
# Zero runtime deps (pure awk/bash). Fails open: any missing/unreadable input
# means no nudge — never wedge the session over a heuristic.

set -uo pipefail

HOOK_DIR="$(cd "$(dirname "$0")" && pwd)"
SCRIPTS_DIR="${HOOK_DIR}/../scripts"
# shellcheck source=../scripts/lib.sh
. "${SCRIPTS_DIR}/lib.sh"

input="$(cat)"

parsed="$(printf '%s' "${input}" | stop_hook_fields)" || exit 0
sid="$(sed -n '1p' <<<"${parsed}")"
tpath="$(sed -n '2p' <<<"${parsed}")"
active="$(sed -n '3p' <<<"${parsed}")"

# Loop guard 1: never block a stop that is itself the product of a prior block.
[[ "${active}" == "true" ]] && exit 0

# Without a session id we can't dedup safely — don't risk a loop; stay quiet.
[[ -n "${sid}" ]] || exit 0

# Loop guard 2: at most once per session. Sanitize the id for use in a filename.
sentinel="${TMPDIR:-/tmp}/demarkus-memory-nudge-${sid//[^A-Za-z0-9_-]/_}"
[[ -e "${sentinel}" ]] && exit 0

# Need a readable transcript to judge what happened.
[[ -n "${tpath}" && -r "${tpath}" ]] || exit 0

# Did the session write to a demarkus memory store? Match the structured MCP
# attribution field and the tool_use call name — both compact-serialized in the
# transcript JSONL. Prose mentions of "mark_publish" won't match these shapes.
soul_write=0
if grep -qE '"attributionMcpTool":"mark_(publish|append)"|"name":"mcp__[^"]*mark_(publish|append)"' "${tpath}"; then
  soul_write=1
fi

# Did the session do real work? File mutations are the low-false-positive signal
# (a pure-chat/research session won't trip this).
real_work=0
if grep -qE '"name":"(Edit|Write|NotebookEdit)"' "${tpath}"; then
  real_work=1
fi

if (( real_work == 1 && soul_write == 0 )); then
  # Create the sentinel BEFORE blocking so the follow-up Stop won't re-nudge.
  : > "${sentinel}" 2>/dev/null || true
  reason="This session changed files but recorded nothing to the soul. If something here is worth remembering — a decision, a gotcha, a non-obvious why — capture it now with /soul-journal (or mark_append to today's journal under /<project>/journal/<YYYY-MM-DD>.md). If it's all routine, just say so and stop; you won't be nudged again this session."
  printf '{"decision":"block","reason":%s}\n' "$(printf '%s' "${reason}" | json_escape)"
  exit 0
fi

exit 0
