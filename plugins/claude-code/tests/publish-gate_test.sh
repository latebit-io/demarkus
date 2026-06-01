#!/usr/bin/env bash
# Shell-level tests for hooks/publish-gate.sh.
#
# Each test_* function returns 0 on pass and non-zero on fail. The runner at
# the bottom executes every test_* in lexical order, prints PASS/FAIL, and
# exits non-zero if any failed.
#
# The gate is a pure stdin→stdout filter: it reads a Claude Code hook payload
# on stdin and prints the decision JSON (or nothing, to defer) on stdout. We
# drive it with synthetic payloads and assert on stdout. Strictness is forced
# per-case via DEMARKUS_MEMORY_STRICTNESS so the tests never touch
# ~/.demarkus/plugin-memory.strictness.
#
# The gate parses payloads with pure awk — no jq, no python, no external deps —
# so there is nothing to install and nothing to skip.

set -uo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" &>/dev/null && pwd)"
PLUGIN_DIR="$(cd -- "${SCRIPT_DIR}/.." &>/dev/null && pwd)"
GATE="${PLUGIN_DIR}/hooks/publish-gate.sh"

[[ -x "${GATE}" ]] || { echo "publish-gate.sh not executable at ${GATE}"; exit 2; }

PUB_TOOL="mcp__plugin_demarkus-memory_demarkus-memory__mark_publish"

PASS=0
FAIL=0
FAILED_NAMES=()

# run_gate STRICTNESS PAYLOAD — pipes PAYLOAD to the gate with the given
# strictness and echoes the gate's stdout.
run_gate() {
  local strictness="$1" payload="$2"
  printf '%s' "${payload}" | DEMARKUS_MEMORY_STRICTNESS="${strictness}" bash "${GATE}"
}


run_test() {
  local name="$1"
  local out rc
  out=$( "test_${name}" 2>&1 )
  rc=$?
  if (( rc == 0 )); then
    echo "PASS  ${name}"
    PASS=$((PASS + 1))
  else
    echo "FAIL  ${name}"
    printf '%s\n' "${out}" | sed 's/^/      /'
    FAIL=$((FAIL + 1))
    FAILED_NAMES+=("${name}")
  fi
}

# ----- helpers ----------------------------------------------------------

# payload EVENT TOOL META_JSON — builds a hook payload. META_JSON is the inner
# value of tool_input.metadata (e.g. '{"tags":"a,b"}'), or the literal word
# "none" to omit metadata entirely.
payload() {
  local event="$1" tool="$2" meta="$3"
  if [[ "${meta}" == "none" ]]; then
    printf '{"hook_event_name":"%s","tool_name":"%s","tool_input":{"url":"/proj/journal/2026-06-01.md"}}' \
      "${event}" "${tool}"
  else
    printf '{"hook_event_name":"%s","tool_name":"%s","tool_input":{"url":"/proj/journal/2026-06-01.md","metadata":%s}}' \
      "${event}" "${tool}" "${meta}"
  fi
}

# ----- test functions ---------------------------------------------------

test_tagged_publish_warn_pre_defers() {
  local out
  out=$(run_gate warn "$(payload PreToolUse "${PUB_TOOL}" '{"tags":"journal,proj"}')")
  [[ -z "${out}" ]] || { echo "expected empty (defer); got: ${out}"; return 1; }
}

test_tagged_publish_warn_post_defers() {
  local out
  out=$(run_gate warn "$(payload PostToolUse "${PUB_TOOL}" '{"tags":"journal,proj"}')")
  [[ -z "${out}" ]] || { echo "expected empty (defer); got: ${out}"; return 1; }
}

test_tagless_publish_warn_pre_defers() {
  # In warn mode the PreToolUse half always defers — the nudge is post-call.
  local out
  out=$(run_gate warn "$(payload PreToolUse "${PUB_TOOL}" 'none')")
  [[ -z "${out}" ]] || { echo "expected empty (defer); got: ${out}"; return 1; }
}

test_tagless_publish_warn_post_injects_context() {
  local out
  out=$(run_gate warn "$(payload PostToolUse "${PUB_TOOL}" 'none')")
  grep -q '"additionalContext"' <<<"${out}" \
    || { echo "missing additionalContext: ${out}"; return 1; }
  grep -q 'metadata.tags' <<<"${out}" \
    || { echo "reason should mention metadata.tags: ${out}"; return 1; }
  # warn must NOT carry a permissionDecision — it is non-blocking.
  grep -q 'permissionDecision' <<<"${out}" \
    && { echo "warn must not emit permissionDecision: ${out}"; return 1; }
  return 0
}

test_tagless_publish_block_pre_denies() {
  local out
  out=$(run_gate block "$(payload PreToolUse "${PUB_TOOL}" 'none')")
  grep -q '"permissionDecision":"deny"' <<<"${out}" \
    || { echo "expected deny: ${out}"; return 1; }
}

test_tagless_publish_block_post_is_noop() {
  # In block mode the tagless publish never runs, so the post half must defer.
  local out
  out=$(run_gate block "$(payload PostToolUse "${PUB_TOOL}" 'none')")
  [[ -z "${out}" ]] || { echo "expected empty (post no-op in block mode); got: ${out}"; return 1; }
}

test_tagless_publish_ask_pre_escalates() {
  local out
  out=$(run_gate ask "$(payload PreToolUse "${PUB_TOOL}" 'none')")
  grep -q '"permissionDecision":"ask"' <<<"${out}" \
    || { echo "expected ask: ${out}"; return 1; }
}

test_empty_tags_string_treated_as_tagless() {
  local out
  out=$(run_gate block "$(payload PreToolUse "${PUB_TOOL}" '{"tags":"   "}')")
  grep -q '"permissionDecision":"deny"' <<<"${out}" \
    || { echo "whitespace-only tags should be tagless → deny: ${out}"; return 1; }
}

test_escaped_whitespace_tags_treated_as_tagless() {
  # The escape-decode bypass: raw JSON \n / \t / \r decode to whitespace and
  # must be judged tagless, not the literal backslash sequences. Each must deny.
  local out esc
  for esc in '\n' '\t' '\r' '\n\t' '\r\n'; do
    out=$(run_gate block "$(payload PreToolUse "${PUB_TOOL}" "{\"tags\":\"${esc}\"}")")
    grep -q '"permissionDecision":"deny"' <<<"${out}" \
      || { echo "escaped-whitespace tags '${esc}' should be tagless → deny: ${out}"; return 1; }
  done
}

test_unicode_escape_whitespace_tags_treated_as_tagless() {
  # \u escapes for space (0020), tab (0009), and ideographic space (3000)
  # decode to whitespace → tagless → deny. Build the "\uXXXX" from a literal
  # backslash var so the sequence survives verbatim into the JSON payload.
  local out esc bs='\'
  for esc in "${bs}u0020" "${bs}u0009" "${bs}u3000"; do
    out=$(run_gate block "$(payload PreToolUse "${PUB_TOOL}" "{\"tags\":\"${esc}\"}")")
    grep -q '"permissionDecision":"deny"' <<<"${out}" \
      || { echo "unicode-whitespace tags '${esc}' should be tagless → deny: ${out}"; return 1; }
  done
}

test_unicode_escape_real_char_tags_pass() {
  # a = 'a' — a real non-whitespace char → not tagless → defer.
  local out bs='\'
  out=$(run_gate block "$(payload PreToolUse "${PUB_TOOL}" "{\"tags\":\"${bs}u0061\"}")")
  [[ -z "${out}" ]] || { echo "unicode non-whitespace tag should defer; got: ${out}"; return 1; }
}

test_escaped_tags_with_real_content_pass() {
  # A tag value carrying a real escape but real content (newline between two
  # subjects) is NOT tagless — must defer.
  local out
  out=$(run_gate block "$(payload PreToolUse "${PUB_TOOL}" '{"tags":"alpha\nbeta"}')")
  [[ -z "${out}" ]] || { echo "tags with escape + real content should defer; got: ${out}"; return 1; }
}

test_nonstring_tags_treated_as_tagless() {
  # tags must be a JSON string. A bare false/null/0/number scalar must NOT
  # satisfy the gate — block must deny each.
  local out v
  for v in 'false' 'null' '0' '42'; do
    out=$(run_gate block "$(payload PreToolUse "${PUB_TOOL}" "{\"tags\":${v}}")")
    grep -q '"permissionDecision":"deny"' <<<"${out}" \
      || { echo "non-string tags '${v}' should be tagless → deny: ${out}"; return 1; }
  done
}

test_exponent_importance_in_range_ok() {
  # 1e-1 == 0.1, a valid in-range number — must not be flagged (defer).
  local out
  out=$(run_gate warn "$(payload PostToolUse "${PUB_TOOL}" '{"tags":"x","importance":"1e-1"}')")
  [[ -z "${out}" ]] || { echo "exponent importance 1e-1 (=0.1) should defer; got: ${out}"; return 1; }
}

test_exponent_importance_out_of_range_flagged() {
  # 1e1 == 10, out of [0,1] — must be flagged.
  local out
  out=$(run_gate warn "$(payload PostToolUse "${PUB_TOOL}" '{"tags":"x","importance":"1e1"}')")
  grep -q 'importance' <<<"${out}" \
    || { echo "exponent importance 1e1 (=10) should be flagged: ${out}"; return 1; }
}

test_importance_out_of_range_flagged() {
  local out
  out=$(run_gate warn "$(payload PostToolUse "${PUB_TOOL}" '{"tags":"journal","importance":"1.5"}')")
  grep -q 'importance' <<<"${out}" \
    || { echo "out-of-range importance should be flagged even with tags present: ${out}"; return 1; }
}

test_importance_string_in_range_ok() {
  # Tags present + importance a valid stringified float → fully valid → defer.
  local out
  out=$(run_gate warn "$(payload PostToolUse "${PUB_TOOL}" '{"tags":"journal","importance":"0.4"}')")
  [[ -z "${out}" ]] || { echo "valid importance should defer; got: ${out}"; return 1; }
}

test_append_tool_is_ignored() {
  # A non-publish tool must never be gated, even with no metadata.
  local out append_tool="mcp__plugin_demarkus-memory_demarkus-memory__mark_append"
  out=$(run_gate block "$(payload PreToolUse "${append_tool}" 'none')")
  [[ -z "${out}" ]] || { echo "append must be ignored; got: ${out}"; return 1; }
}

test_decoy_body_does_not_false_match() {
  # The killer case for any non-real-parser: a `body` value that literally
  # contains "tags":"DECOY", escaped quotes, and stray braces must NOT be read
  # as the real metadata. Real metadata.tags is present → block must defer.
  local p out
  p='{"hook_event_name":"PreToolUse","tool_name":"'"${PUB_TOOL}"'","tool_input":{"url":"/p/a.md","body":"prose with \"tags\":\"DECOY\" and { braces } inside","metadata":{"tags":"real,subjects"}}}'
  out=$(run_gate block "${p}")
  [[ -z "${out}" ]] || { echo "decoy body false-matched; tagged publish should defer: ${out}"; return 1; }
}

test_decoy_body_tagless_still_caught() {
  # Same decoy text, but NO real metadata — block must still deny (the decoy
  # inside body must not be mistaken for real tags).
  local p out
  p='{"hook_event_name":"PreToolUse","tool_name":"'"${PUB_TOOL}"'","tool_input":{"url":"/p/a.md","body":"prose with \"tags\":\"DECOY\" inside"}}'
  out=$(run_gate block "${p}")
  grep -q '"permissionDecision":"deny"' <<<"${out}" \
    || { echo "decoy tags in body must not satisfy the gate; expected deny: ${out}"; return 1; }
}

test_unknown_strictness_falls_back_to_warn() {
  # A bogus strictness must not block — it degrades to warn (no pre-deny).
  local out
  out=$(run_gate bogus "$(payload PreToolUse "${PUB_TOOL}" 'none')")
  [[ -z "${out}" ]] || { echo "unknown strictness must not deny (defer to warn); got: ${out}"; return 1; }
  # ...and the post half should warn.
  out=$(run_gate bogus "$(payload PostToolUse "${PUB_TOOL}" 'none')")
  grep -q '"additionalContext"' <<<"${out}" \
    || { echo "unknown strictness post should warn: ${out}"; return 1; }
}

# ----- runner -----------------------------------------------------------

TESTS=$(declare -F | awk '$3 ~ /^test_/ { print $3 }')

echo "=== publish-gate shell tests ==="
for t in ${TESTS}; do
  run_test "${t#test_}"
done

echo
echo "${PASS} passed, ${FAIL} failed"
if (( FAIL > 0 )); then
  echo "failing tests:"
  for n in "${FAILED_NAMES[@]}"; do echo "  - ${n}"; done
  exit 1
fi
