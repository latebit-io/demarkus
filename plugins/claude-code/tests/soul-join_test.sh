#!/usr/bin/env bash
# Shell-level tests for /soul-join: the lib.sh catalog/binding helpers, the
# publish-gate scope extension, and scripts/soul-join.sh validation/registration.
#
# Each test_* runs in its own throwaway HOME so the real ~/.demarkus is never
# touched. soul-join.sh is driven with DEMARKUS_SOUL_JOIN_SKIP_BINARIES=1 so it
# registers offline without the network binary download. Pure bash/awk — nothing
# to install, nothing to skip.

# The lib path is computed at runtime (${LIB}), so shellcheck can't follow the
# source. The file it sources (scripts/lib.sh) is checked on its own. File-level
# directive must precede the first command to apply throughout.
# shellcheck disable=SC1090
set -uo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" &>/dev/null && pwd)"
PLUGIN_DIR="$(cd -- "${SCRIPT_DIR}/.." &>/dev/null && pwd)"
LIB="${PLUGIN_DIR}/scripts/lib.sh"
JOIN="${PLUGIN_DIR}/scripts/soul-join.sh"

[[ -f "${LIB}" ]]  || { echo "lib.sh not found at ${LIB}"; exit 2; }
[[ -x "${JOIN}" ]] || { echo "soul-join.sh not executable at ${JOIN}"; exit 2; }

PASS=0
FAIL=0
FAILED_NAMES=()

run_test() {
  local name="$1" out rc
  out=$( "test_${name}" 2>&1 )
  rc=$?
  if (( rc == 0 )); then
    echo "PASS  ${name}"; PASS=$((PASS + 1))
  else
    echo "FAIL  ${name}"
    printf '%s\n' "${out}" | sed 's/^/      /'
    FAIL=$((FAIL + 1)); FAILED_NAMES+=("${name}")
  fi
}

# new_home — make a throwaway dir, export it as HOME, echo it. Each test gets a
# clean ~/.demarkus this way (lib.sh derives PLUGIN_HOME from HOME at source).
new_home() { local t; t="$(mktemp -d)"; export HOME="${t}"; echo "${t}"; }

# join — run soul-join.sh offline against the current HOME. Echoes its stdout.
join() { DEMARKUS_SOUL_JOIN_SKIP_BINARIES=1 bash "${JOIN}" "$@"; }

# val KEY OUTPUT — extract the value of a key=value line from join() output.
val() { printf '%s\n' "$2" | awk -F= -v k="$1" '$1==k{ sub(/^[^=]*=/,""); print; exit }'; }

# file_mode FILE — echo the octal perm bits, portable across macOS/Linux.
file_mode() { stat -f '%Lp' "$1" 2>/dev/null || stat -c '%a' "$1" 2>/dev/null; }

# ----- lib helper tests -------------------------------------------------------

test_register_and_fields_roundtrip() {
  new_home >/dev/null; . "${LIB}"
  register_remote_soul "soul" "mark://soul.demarkus.io" 1 "/tok/soul.token" || { echo "register failed"; return 1; }
  local f; f="$(remote_soul_fields soul)"
  [[ "${f}" == $'mark://soul.demarkus.io\t1\t/tok/soul.token' ]] || { echo "fields mismatch: ${f}"; return 1; }
}

test_register_upsert_replaces_row() {
  new_home >/dev/null; . "${LIB}"
  register_remote_soul "soul" "mark://old.example" 0 "-"
  register_remote_soul "soul" "mark://new.example" 1 "/t.token"
  local n; n="$(list_remote_souls | grep -c '^soul$')"
  [[ "${n}" -eq 1 ]] || { echo "expected 1 row for slug, got ${n}"; return 1; }
  local f; f="$(remote_soul_fields soul)"
  [[ "${f}" == $'mark://new.example\t1\t/t.token' ]] || { echo "upsert did not replace: ${f}"; return 1; }
}

test_insecure_and_empty_token_normalized() {
  new_home >/dev/null; . "${LIB}"
  register_remote_soul "a" "mark://a" "true" ""   # true → 1, ""→ "-"
  [[ "$(remote_soul_fields a)" == $'mark://a\t1\t-' ]] || { echo "norm A: $(remote_soul_fields a)"; return 1; }
  register_remote_soul "b" "mark://b" "nonsense" "-"  # garbage → 0
  [[ "$(remote_soul_fields b)" == $'mark://b\t0\t-' ]] || { echo "norm B: $(remote_soul_fields b)"; return 1; }
}

test_register_empty_slug_fails() {
  new_home >/dev/null; . "${LIB}"
  register_remote_soul "" "mark://x" 0 "-" 2>/dev/null && { echo "empty slug should fail"; return 1; }
  return 0
}

test_is_registered_remote_soul() {
  new_home >/dev/null; . "${LIB}"
  register_remote_soul "soul" "mark://s" 0 "-"
  is_registered_remote_soul soul   || { echo "registered slug not found"; return 1; }
  is_registered_remote_soul nope   && { echo "unregistered slug matched"; return 1; }
  return 0
}

test_project_binding_roundtrip_and_upsert() {
  new_home >/dev/null; . "${LIB}"
  bind_project_soul "/repo/demarkus" "soul" || { echo "bind failed"; return 1; }
  [[ "$(project_soul_binding /repo/demarkus)" == "soul" ]] || { echo "binding lookup wrong"; return 1; }
  bind_project_soul "/repo/demarkus" "other"  # rebind same dir
  [[ "$(project_soul_binding /repo/demarkus)" == "other" ]] || { echo "rebind did not replace"; return 1; }
  local n; n="$(awk -F'\t' '$1=="/repo/demarkus"' "${PROJECT_SOULS}" | wc -l | tr -d ' ')"
  [[ "${n}" -eq 1 ]] || { echo "expected 1 binding row, got ${n}"; return 1; }
  [[ -z "$(project_soul_binding /repo/unbound)" ]] || { echo "unbound dir should be empty"; return 1; }
}

test_publish_gate_scope_local_and_remote() {
  new_home >/dev/null; . "${LIB}"
  register_remote_soul "soul" "mark://s" 0 "-"
  [[ "$(publish_gate_scope mcp__demarkus-memory__mark_publish)" == "local" ]] \
    || { echo "manual local soul not scoped"; return 1; }
  [[ "$(publish_gate_scope mcp__plugin_demarkus-memory_demarkus-memory__mark_publish)" == "local" ]] \
    || { echo "plugin-prefixed local soul not scoped"; return 1; }
  [[ "$(publish_gate_scope mcp__soul__mark_publish)" == "local" ]] \
    || { echo "registered remote soul not scoped"; return 1; }
  [[ -z "$(publish_gate_scope mcp__knowledge__mark_publish)" ]] \
    || { echo "unregistered server must not be scoped"; return 1; }
}

# ----- soul-join.sh tests -----------------------------------------------------

test_join_bare_host_ok() {
  new_home >/dev/null
  local out; out="$(join soul.demarkus.io)"
  [[ "$(printf '%s\n' "${out}" | head -1)" == "OK" ]] || { echo "no OK: ${out}"; return 1; }
  [[ "$(val slug "${out}")" == "soul" ]]                       || { echo "slug: ${out}"; return 1; }
  [[ "$(val host "${out}")" == "mark://soul.demarkus.io" ]]    || { echo "host: ${out}"; return 1; }
  [[ "$(val insecure "${out}")" == "0" ]]                      || { echo "insecure: ${out}"; return 1; }
  [[ "$(val token-file "${out}")" == "-" ]]                    || { echo "token-file: ${out}"; return 1; }
}

test_join_mark_url_with_port_preserved() {
  new_home >/dev/null
  local out; out="$(join mark://soul.demarkus.io:6309 --insecure)"
  [[ "$(val host "${out}")" == "mark://soul.demarkus.io:6309" ]] || { echo "host: ${out}"; return 1; }
  [[ "$(val slug "${out}")" == "soul" ]]                          || { echo "slug: ${out}"; return 1; }
  [[ "$(val insecure "${out}")" == "1" ]]                         || { echo "insecure: ${out}"; return 1; }
}

test_join_token_written_0600() {
  new_home >/dev/null
  local out; out="$(join mark://soul.demarkus.io --token SECRET123)"
  local tf; tf="$(val token-file "${out}")"
  [[ "${tf}" == "${HOME}/.demarkus/soul-soul.token" ]] || { echo "token-file path: ${tf}"; return 1; }
  [[ -f "${tf}" ]]                                       || { echo "token file not written"; return 1; }
  [[ "$(cat "${tf}")" == "SECRET123" ]]                  || { echo "token contents wrong"; return 1; }
  [[ "$(file_mode "${tf}")" == "600" ]]                  || { echo "token mode not 600: $(file_mode "${tf}")"; return 1; }
}

test_join_installs_wrapper_to_stable_path() {
  new_home >/dev/null
  local out; out="$(join soul.demarkus.io)"
  local w; w="$(val wrapper "${out}")"
  [[ "${w}" == "${HOME}/.demarkus/bin/soul-remote-wrapper.sh" ]] || { echo "wrapper path: ${w}"; return 1; }
  [[ -x "${w}" ]]                                                 || { echo "wrapper not installed/executable"; return 1; }
}

test_join_binds_project() {
  local t; t="$(new_home)"
  local out; out="$(join soul.demarkus.io --bind /repo/demarkus)"
  [[ "$(printf '%s\n' "${out}" | head -1)" == "OK" ]] || { echo "no OK: ${out}"; return 1; }
  # Verify via the registry file directly (independent of lib state).
  grep -qF $'/repo/demarkus\tsoul' "${HOME}/.demarkus/project-souls" \
    || { echo "binding not recorded: $(cat "${HOME}/.demarkus/project-souls" 2>/dev/null)"; return 1; }
}

test_join_slug_sanitized() {
  new_home >/dev/null
  local out; out="$(join mark://My_Soul.example.com)"
  [[ "$(val slug "${out}")" == "my-soul" ]] || { echo "slug not sanitized: $(val slug "${out}")"; return 1; }
}

test_join_https_rejected_points_to_knowledge() {
  new_home >/dev/null
  local out; out="$(join https://knowledge.demarkus.io 2>&1 || true)"
  grep -q '^FAIL:' <<<"${out}"        || { echo "https should FAIL: ${out}"; return 1; }
  grep -q 'knowledge-join' <<<"${out}" || { echo "should point to /knowledge-join: ${out}"; return 1; }
}

test_join_reserved_slug_rejected() {
  new_home >/dev/null
  # first DNS label "demarkus-memory" → reserved local-soul name → FAIL.
  local out; out="$(join mark://demarkus-memory.example 2>&1 || true)"
  grep -q '^FAIL:' <<<"${out}"   || { echo "reserved slug should FAIL: ${out}"; return 1; }
  grep -q 'reserved' <<<"${out}" || { echo "message should say reserved: ${out}"; return 1; }
}

test_join_no_host_fails() {
  new_home >/dev/null
  local out; out="$(join 2>&1 || true)"
  grep -q '^FAIL:' <<<"${out}" || { echo "missing host should FAIL: ${out}"; return 1; }
}

# ----- runner -----------------------------------------------------------------

TESTS=$(declare -F | awk '$3 ~ /^test_/ { print $3 }')

echo "=== soul-join shell tests ==="
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
