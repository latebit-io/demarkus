#!/usr/bin/env bash
# Shell-level tests for knowledge-system scoping of the publish gate (#7):
# register-knowledge.sh, the registry, server scoping in publish-gate.sh, and
# per-slug strictness. Each case runs with an isolated HOME so the registry and
# strictness files (~/.demarkus/...) never touch the real environment.

set -uo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" &>/dev/null && pwd)"
PLUGIN_DIR="$(cd -- "${SCRIPT_DIR}/.." &>/dev/null && pwd)"
GATE="${PLUGIN_DIR}/hooks/publish-gate.sh"
REGISTER="${PLUGIN_DIR}/scripts/register-knowledge.sh"

[[ -x "${GATE}" && -x "${REGISTER}" ]] || { echo "gate or register script not executable"; exit 2; }

PASS=0
FAIL=0
FAILED_NAMES=()

run_test() {
  local name="$1" out rc
  out=$( "test_${name}" 2>&1 )
  rc=$?
  if (( rc == 0 )); then echo "PASS  ${name}"; PASS=$((PASS + 1))
  else echo "FAIL  ${name}"; printf '%s\n' "${out}" | sed 's/^/      /'; FAIL=$((FAIL + 1)); FAILED_NAMES+=("${name}"); fi
}

# payload TOOL META — tagless if META=="none", else META is the metadata object.
payload() {
  local tool="$1" meta="$2"
  if [[ "${meta}" == "none" ]]; then
    printf '{"hook_event_name":"PreToolUse","tool_name":"%s","tool_input":{"url":"/x.md"}}' "${tool}"
  else
    printf '{"hook_event_name":"PreToolUse","tool_name":"%s","tool_input":{"url":"/x.md","metadata":%s}}' "${tool}" "${meta}"
  fi
}

# gate HOME [ENV_STRICTNESS] PAYLOAD — run the gate with an isolated HOME. When
# ENV_STRICTNESS is "-", no env override is set (so file-based strictness wins).
gate() {
  local home="$1" envs="$2" pl="$3"
  if [[ "${envs}" == "-" ]]; then
    printf '%s' "${pl}" | HOME="${home}" bash "${GATE}"
  else
    printf '%s' "${pl}" | HOME="${home}" DEMARKUS_KNOWLEDGE_STRICTNESS="${envs}" bash "${GATE}"
  fi
}

# ----- tests ------------------------------------------------------------

test_register_writes_registry() {
  local home; home="$(mktemp -d)"
  HOME="${home}" bash "${REGISTER}" acme 2>/dev/null
  local ok=1
  grep -qxF acme "${home}/.demarkus/knowledge-systems" 2>/dev/null || ok=0
  # idempotent: a second register adds no duplicate line.
  HOME="${home}" bash "${REGISTER}" acme 2>/dev/null
  [[ "$(grep -cxF acme "${home}/.demarkus/knowledge-systems")" == "1" ]] || ok=0
  rm -rf "${home}"
  (( ok == 1 )) || { echo "register-knowledge.sh should write one acme line"; return 1; }
}

test_local_soul_not_gated_here() {
  # The local demarkus-memory soul is the demarkus-memory plugin's concern, NOT
  # this one. Even tagless in block mode, the knowledge gate must defer on it so
  # the two plugins' gates don't both fire on a local-soul publish.
  local home; home="$(mktemp -d)"
  local out; out="$(gate "${home}" block "$(payload 'mcp__plugin_demarkus-memory_demarkus-memory__mark_publish' none)")"
  rm -rf "${home}"
  [[ -z "${out}" ]] \
    || { echo "local soul must NOT be gated by the knowledge plugin; got: ${out}"; return 1; }
}

test_foreign_server_not_gated() {
  # A demarkus server the plugin doesn't manage (e.g. a personal soul) must be
  # left alone, even tagless in block mode.
  local home; home="$(mktemp -d)"
  local out; out="$(gate "${home}" block "$(payload 'mcp__demarkus-soul__mark_publish' none)")"
  rm -rf "${home}"
  [[ -z "${out}" ]] || { echo "unmanaged server must not be gated; got: ${out}"; return 1; }
}

test_unregistered_ks_not_gated() {
  # Same shape as a knowledge system but not registered → not ours → ignore.
  local home; home="$(mktemp -d)"
  local out; out="$(gate "${home}" block "$(payload 'mcp__acme__mark_publish' none)")"
  rm -rf "${home}"
  [[ -z "${out}" ]] || { echo "unregistered server must not be gated; got: ${out}"; return 1; }
}

test_registered_ks_gated() {
  local home; home="$(mktemp -d)"
  HOME="${home}" bash "${REGISTER}" acme 2>/dev/null
  local out; out="$(gate "${home}" block "$(payload 'mcp__acme__mark_publish' none)")"
  rm -rf "${home}"
  grep -q '"permissionDecision":"deny"' <<<"${out}" \
    || { echo "registered knowledge system publish should be gated: ${out}"; return 1; }
}

test_registered_ks_per_slug_strictness() {
  # No env override, no global strictness file — only a per-slug file set to
  # block. The gate must read the per-slug level for this system.
  local home; home="$(mktemp -d)"
  HOME="${home}" bash "${REGISTER}" acme 2>/dev/null
  printf 'block\n' > "${home}/.demarkus/plugin-knowledge.strictness.acme"
  local out; out="$(gate "${home}" - "$(payload 'mcp__acme__mark_publish' none)")"
  rm -rf "${home}"
  grep -q '"permissionDecision":"deny"' <<<"${out}" \
    || { echo "per-slug strictness=block should deny tagless KS publish: ${out}"; return 1; }
}

test_registered_ks_per_slug_isolation() {
  # A per-slug block for 'acme' must NOT affect a different registered system
  # 'beta' (which falls back to the warn default → PostToolUse, not a pre-deny).
  local home; home="$(mktemp -d)"
  HOME="${home}" bash "${REGISTER}" acme 2>/dev/null
  HOME="${home}" bash "${REGISTER}" beta 2>/dev/null
  printf 'block\n' > "${home}/.demarkus/plugin-knowledge.strictness.acme"
  local out; out="$(gate "${home}" - "$(payload 'mcp__beta__mark_publish' none)")"
  rm -rf "${home}"
  # beta defers in PreToolUse under warn (the nudge is on PostToolUse).
  [[ -z "${out}" ]] || { echo "beta must not inherit acme's block; got: ${out}"; return 1; }
}

test_registered_ks_tagged_defers() {
  local home; home="$(mktemp -d)"
  HOME="${home}" bash "${REGISTER}" acme 2>/dev/null
  local out; out="$(gate "${home}" block "$(payload 'mcp__acme__mark_publish' '{"tags":"team,decision"}')")"
  rm -rf "${home}"
  [[ -z "${out}" ]] || { echo "properly tagged KS publish should defer; got: ${out}"; return 1; }
}

# require_tags: an axis is satisfied by an "axis:value" token in the tags.

test_require_tags_satisfied_defers() {
  local home; home="$(mktemp -d)"
  HOME="${home}" bash "${REGISTER}" acme 2>/dev/null
  printf 'team, type\n' > "${home}/.demarkus/plugin-knowledge.require-tags.acme"
  local out; out="$(gate "${home}" block "$(payload 'mcp__acme__mark_publish' '{"tags":"team:payments, type:decision"}')")"
  rm -rf "${home}"
  [[ -z "${out}" ]] || { echo "all required axes present should defer; got: ${out}"; return 1; }
}

test_require_tags_missing_axis_denied() {
  local home; home="$(mktemp -d)"
  HOME="${home}" bash "${REGISTER}" acme 2>/dev/null
  printf 'team, type\n' > "${home}/.demarkus/plugin-knowledge.require-tags.acme"
  # Has tags and a team axis, but no type axis → violation.
  local out; out="$(gate "${home}" block "$(payload 'mcp__acme__mark_publish' '{"tags":"team:payments, design"}')")"
  rm -rf "${home}"
  grep -q '"permissionDecision":"deny"' <<<"${out}" \
    || { echo "missing required axis should deny: ${out}"; return 1; }
  grep -q 'type' <<<"${out}" \
    || { echo "reason should name the missing axis 'type': ${out}"; return 1; }
}

test_require_tags_warn_nudges_post() {
  local home; home="$(mktemp -d)"
  HOME="${home}" bash "${REGISTER}" acme 2>/dev/null
  printf 'team\n' > "${home}/.demarkus/plugin-knowledge.require-tags.acme"
  # warn (no env): PostToolUse should nudge about the missing axis.
  local pl; pl="$(printf '{"hook_event_name":"PostToolUse","tool_name":"mcp__acme__mark_publish","tool_input":{"url":"/d.md","metadata":{"tags":"random"}}}')"
  local out; out="$(gate "${home}" - "${pl}")"
  rm -rf "${home}"
  grep -q '"additionalContext"' <<<"${out}" \
    || { echo "warn should nudge on missing axis: ${out}"; return 1; }
  grep -q 'team' <<<"${out}" \
    || { echo "nudge should name the missing axis 'team': ${out}"; return 1; }
}

test_require_tags_axis_matched_literally() {
  # An axis containing a regex metachar ('a.c') must be matched LITERALLY — a
  # token 'abc:x' must NOT satisfy it (a grep -E interpolation would overmatch).
  local home; home="$(mktemp -d)"
  HOME="${home}" bash "${REGISTER}" acme 2>/dev/null
  printf 'a.c\n' > "${home}/.demarkus/plugin-knowledge.require-tags.acme"
  local deny tagged
  deny="$(gate "${home}" block "$(payload 'mcp__acme__mark_publish' '{"tags":"abc:x"}')")"
  tagged="$(gate "${home}" block "$(payload 'mcp__acme__mark_publish' '{"tags":"a.c:yes"}')")"
  rm -rf "${home}"
  grep -q '"permissionDecision":"deny"' <<<"${deny}" \
    || { echo "axis 'a.c' must not be satisfied by 'abc:x' (literal match): ${deny}"; return 1; }
  [[ -z "${tagged}" ]] \
    || { echo "axis 'a.c' satisfied by literal 'a.c:yes' should defer: ${tagged}"; return 1; }
}

test_require_tags_no_glob_expansion() {
  # A tag value with a glob metacharacter must NOT expand against the filesystem.
  # Run from a cwd that contains a decoy file named like a satisfying token; a
  # tag of '*' must still be treated literally → category axis missing → deny.
  local home; home="$(mktemp -d)"
  HOME="${home}" bash "${REGISTER}" acme 2>/dev/null
  printf 'category\n' > "${home}/.demarkus/plugin-knowledge.require-tags.acme"
  local work; work="$(mktemp -d)"
  : > "${work}/category:leak"   # would match 'category:'* if '*' globbed here
  local pl; pl="$(payload 'mcp__acme__mark_publish' '{"tags":"*"}')"
  local out
  out="$(cd "${work}" && printf '%s' "${pl}" | HOME="${home}" DEMARKUS_KNOWLEDGE_STRICTNESS=block bash "${GATE}")"
  rm -rf "${home}" "${work}"
  grep -q '"permissionDecision":"deny"' <<<"${out}" \
    || { echo "glob tag '*' must not expand to satisfy the axis; expected deny: ${out}"; return 1; }
}

test_require_tags_isolation() {
  # acme requires team; beta requires nothing — a bare-tagged beta publish defers.
  local home; home="$(mktemp -d)"
  HOME="${home}" bash "${REGISTER}" acme 2>/dev/null
  HOME="${home}" bash "${REGISTER}" beta 2>/dev/null
  printf 'team\n' > "${home}/.demarkus/plugin-knowledge.require-tags.acme"
  local out; out="$(gate "${home}" block "$(payload 'mcp__beta__mark_publish' '{"tags":"anything"}')")"
  rm -rf "${home}"
  [[ -z "${out}" ]] || { echo "beta has no required axes; should defer; got: ${out}"; return 1; }
}

test_require_tags_not_applied_to_local() {
  # A global require-tags file must not gate the local soul unless intended;
  # here only a per-slug file exists, so the local soul is unaffected.
  local home; home="$(mktemp -d)"; mkdir -p "${home}/.demarkus"
  printf 'team\n' > "${home}/.demarkus/plugin-knowledge.require-tags.acme"
  local out; out="$(gate "${home}" block "$(payload 'mcp__plugin_demarkus-memory_demarkus-memory__mark_publish' '{"tags":"notes"}')")"
  rm -rf "${home}"
  [[ -z "${out}" ]] || { echo "local soul must not inherit a per-slug require-tags; got: ${out}"; return 1; }
}

# ----- runner -----------------------------------------------------------

TESTS=$(declare -F | awk '$3 ~ /^test_/ { print $3 }')

echo "=== knowledge-gate shell tests ==="
for t in ${TESTS}; do run_test "${t#test_}"; done

echo
echo "${PASS} passed, ${FAIL} failed"
if (( FAIL > 0 )); then
  echo "failing tests:"
  for n in "${FAILED_NAMES[@]}"; do echo "  - ${n}"; done
  exit 1
fi
