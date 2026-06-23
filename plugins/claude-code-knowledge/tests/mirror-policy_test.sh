#!/usr/bin/env bash
# Tests for deterministic policy mirroring (mirror_policy in lib.sh, driven via
# mirror-policy.sh). Each case runs with an isolated HOME so the per-slug mirror
# files never touch the real environment.

set -uo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" &>/dev/null && pwd)"
PLUGIN_DIR="$(cd -- "${SCRIPT_DIR}/.." &>/dev/null && pwd)"
MIRROR="${PLUGIN_DIR}/scripts/mirror-policy.sh"

[[ -x "${MIRROR}" ]] || { echo "mirror-policy.sh not executable"; exit 2; }

PASS=0; FAIL=0; FAILED_NAMES=()
run_test() {
  local name="$1" out rc
  out=$( "test_${name}" 2>&1 ); rc=$?
  if (( rc == 0 )); then echo "PASS  ${name}"; PASS=$((PASS + 1))
  else echo "FAIL  ${name}"; printf '%s\n' "${out}" | sed 's/^/      /'; FAIL=$((FAIL + 1)); FAILED_NAMES+=("${name}"); fi
}

# mirror HOME SLUG BODY — pipe BODY into mirror-policy.sh under an isolated HOME.
mirror() { printf '%s' "$3" | HOME="$1" bash "${MIRROR}" "$2" >/dev/null 2>&1; }

# mf HOME NAME — echo trimmed contents of ~/.demarkus/<NAME>, or "<none>".
mf() { local f="$1/.demarkus/$2"; [[ -f "${f}" ]] && tr -d '\n' < "${f}" || echo "<none>"; }

FULL_POLICY='# Knowledge System Policy

policy_version: 3
owner: x@example.com

strictness: block
require_tags: category
require_fields: type

The three lines above are the enforced core (`strictness:` etc).'

test_mirrors_all_three() {
  local h; h="$(mktemp -d)"
  mirror "${h}" acme "${FULL_POLICY}"
  local s t f
  s="$(mf "${h}" plugin-knowledge.strictness.acme)"
  t="$(mf "${h}" plugin-knowledge.require-tags.acme)"
  f="$(mf "${h}" plugin-knowledge.require-fields.acme)"
  rm -rf "${h}"
  [[ "${s}" == "block" ]]    || { echo "strictness=${s}, want block"; return 1; }
  [[ "${t}" == "category" ]] || { echo "require_tags=${t}, want category"; return 1; }
  [[ "${f}" == "type" ]]     || { echo "require_fields=${f}, want type"; return 1; }
}

test_partial_policy_omits_absent_knob() {
  local h; h="$(mktemp -d)"
  mirror "${h}" acme 'strictness: warn
require_tags: category, team'
  local t f
  t="$(mf "${h}" plugin-knowledge.require-tags.acme)"
  f="$(mf "${h}" plugin-knowledge.require-fields.acme)"
  rm -rf "${h}"
  [[ "${t}" == "category, team" ]] || { echo "require_tags=${t}, want 'category, team'"; return 1; }
  [[ "${f}" == "<none>" ]]         || { echo "require_fields should be absent; got ${f}"; return 1; }
}

test_relaxing_policy_clears_stale_file() {
  local h; h="$(mktemp -d)"
  mkdir -p "${h}/.demarkus"
  printf 'type\n' > "${h}/.demarkus/plugin-knowledge.require-fields.acme"   # stale, pre-existing
  mirror "${h}" acme 'strictness: block
require_tags: category'                                                     # no require_fields → must clear
  local f; f="$(mf "${h}" plugin-knowledge.require-fields.acme)"
  rm -rf "${h}"
  [[ "${f}" == "<none>" ]] || { echo "dropped knob must clear its mirror; got ${f}"; return 1; }
}

test_prose_mention_not_mirrored() {
  local h; h="$(mktemp -d)"
  # 'strictness:' appears only mid-sentence / in backticks, never as a line's
  # first token — must not be mistaken for a declaration.
  mirror "${h}" acme 'This policy declares no enforced core.
The `strictness:` knob would go here but is not set.'
  local s; s="$(mf "${h}" plugin-knowledge.strictness.acme)"
  rm -rf "${h}"
  [[ "${s}" == "<none>" ]] || { echo "prose mention must not mirror; got ${s}"; return 1; }
}

test_per_slug_isolation() {
  local h; h="$(mktemp -d)"
  mirror "${h}" acme 'require_fields: type'
  mirror "${h}" beta 'strictness: warn'
  local af bf
  af="$(mf "${h}" plugin-knowledge.require-fields.acme)"
  bf="$(mf "${h}" plugin-knowledge.require-fields.beta)"
  rm -rf "${h}"
  [[ "${af}" == "type" ]]  || { echo "acme require_fields=${af}, want type"; return 1; }
  [[ "${bf}" == "<none>" ]] || { echo "beta must not inherit acme's require_fields; got ${bf}"; return 1; }
}

echo "=== mirror-policy tests ==="
TESTS=$(declare -F | awk '$3 ~ /^test_/ { print $3 }')
for t in ${TESTS}; do run_test "${t#test_}"; done
echo "${PASS} passed, ${FAIL} failed"
(( FAIL == 0 ))
