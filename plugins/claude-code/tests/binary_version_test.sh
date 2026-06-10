#!/usr/bin/env bash
# Shell-level tests for _binary_version() in scripts/lib.sh.
#
# _binary_version queries a binary's version via its --version flag and is the
# basis for ensure_binaries' live drift check. It must echo the reported version
# for a working binary and empty for anything that can't report one — missing,
# non-executable, or too old to know the flag (a pre-0.17.15 binary errors on it).
# Empty is what makes ensure_binaries treat an old/broken binary as a mismatch
# and re-download. Each case uses an isolated temp dir and stub scripts; no real
# binary or network. Pure bash, no deps.

set -uo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" &>/dev/null && pwd)"
PLUGIN_DIR="$(cd -- "${SCRIPT_DIR}/.." &>/dev/null && pwd)"
# shellcheck source=../scripts/lib.sh
. "${PLUGIN_DIR}/scripts/lib.sh"

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

# _stub FILE BODY — write BODY as an executable stub script.
_stub() {
  printf '#!/usr/bin/env bash\n%s\n' "$2" > "$1"
  chmod +x "$1"
}

test_reports_version_and_forwards_flag() {
  # A working binary echoes its version only when handed the version flag —
  # proves _binary_version both captures stdout and forwards "$@".
  local tmp; tmp="$(mktemp -d)"
  _stub "${tmp}/bin" '[[ "$1" == "--version" ]] && echo "1.2.3" || { echo usage >&2; exit 1; }'
  local got; got="$(_binary_version "${tmp}/bin" --version)"
  rm -rf "${tmp}"
  [[ "${got}" == "1.2.3" ]] || { echo "expected 1.2.3, got '${got}'"; return 1; }
}

test_empty_for_old_binary() {
  # A pre-version binary errors on the unknown flag (stderr + non-zero). The
  # captured value must be empty, never the error text.
  local tmp; tmp="$(mktemp -d)"
  _stub "${tmp}/bin" 'echo "flag provided but not defined: -version" >&2; exit 2'
  local got; got="$(_binary_version "${tmp}/bin" --version)"
  rm -rf "${tmp}"
  [[ -z "${got}" ]] || { echo "expected empty for an old binary, got '${got}'"; return 1; }
}

test_empty_for_missing_binary() {
  local tmp; tmp="$(mktemp -d)"
  local got; got="$(_binary_version "${tmp}/nope" --version)"
  rm -rf "${tmp}"
  [[ -z "${got}" ]] || { echo "expected empty for a missing binary, got '${got}'"; return 1; }
}

test_empty_for_non_executable() {
  local tmp; tmp="$(mktemp -d)"
  printf 'not executable\n' > "${tmp}/bin"  # no chmod +x
  local got; got="$(_binary_version "${tmp}/bin" --version)"
  rm -rf "${tmp}"
  [[ -z "${got}" ]] || { echo "expected empty for a non-executable file, got '${got}'"; return 1; }
}

test_desired_versions_shape() {
  # _installed_versions must be byte-comparable to _desired_versions, so the
  # field shape has to match exactly (no trailing newline, same key order).
  local desired; desired="$(_desired_versions)"
  case "${desired}" in
    server=*,client=*,tools=*) ;;
    *) echo "unexpected _desired_versions shape: '${desired}'"; return 1 ;;
  esac
  # No trailing newline (printf, not echo) — a stray newline would never match
  # _installed_versions and would force a redundant re-download every startup.
  [[ "$(printf '%s' "${desired}" | wc -l | tr -d ' ')" == "0" ]] || { echo "_desired_versions has a trailing newline"; return 1; }
}

TESTS=$(declare -F | awk '$3 ~ /^test_/ { print $3 }')

echo "=== _binary_version shell tests ==="
for t in ${TESTS}; do run_test "${t#test_}"; done

echo
echo "${PASS} passed, ${FAIL} failed"
if (( FAIL > 0 )); then
  echo "failing tests:"
  for n in "${FAILED_NAMES[@]}"; do echo "  - ${n}"; done
  exit 1
fi
