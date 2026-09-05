#!/usr/bin/env bash
# Shim contract test: a fake demarkus-plugin records how each hook calls it.
# Run from anywhere: bash plugins/cursor-knowledge/hooks/test.sh
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
export HOME; HOME="$(mktemp -d)"
mkdir -p "${HOME}/.demarkus/bin"
CALLS="${HOME}/calls"
PIN="$(sed -nE 's/^TOOLS_VERSION="([^"]+)".*/\1/p' "${HERE}/../scripts/bootstrap.sh")"
[[ -n "${PIN}" ]] || { echo "FAIL: no TOOLS_VERSION pin in bootstrap.sh" >&2; exit 1; }
cat > "${HOME}/.demarkus/bin/demarkus-plugin" <<FAKE
#!/usr/bin/env bash
printf '%s\n' "\$*" >> "${CALLS}"
in="\$(cat)"
case "\$1" in
  version) echo "${PIN}" ;;
  gate) echo '{"permission":"deny","user_message":"tags","agent_message":"tags"}' ;;
  guidance) echo '{"additional_context":"guidance"}' ;;
esac
FAKE
chmod +x "${HOME}/.demarkus/bin/demarkus-plugin"
fail() { echo "FAIL: $*" >&2; exit 1; }

# gate pipes through with --format cursor and prints the binary's verdict
out="$(echo '{"hook_event_name":"beforeMCPExecution","tool_name":"mark_publish","mcp_server_name":"acme","tool_input":{}}' | bash "${HERE}/gate.sh")"
[[ "${out}" == '{"permission":"deny","user_message":"tags","agent_message":"tags"}' ]] || fail "gate output: ${out}"
grep -q '^gate --format cursor$' "${CALLS}" || fail "gate flags wrong: $(cat "${CALLS}")"

# session-start: bootstrap is skipped when the binary exists; guidance is emitted on stdout only
out="$(echo '{"hook_event_name":"sessionStart","conversation_id":"c"}' | bash "${HERE}/session-start.sh" 2>/dev/null)"
[[ "${out}" == '{"additional_context":"guidance"}' ]] || fail "session-start output: ${out}"
grep -q -- "^guidance --surface knowledge --guidance-file ${HERE}/../context/session-guidance.md --format cursor$" "${CALLS}" || fail "guidance flags wrong: $(cat "${CALLS}")"

# gate fails open without the binary: empty stdout, diagnostic on stderr
rm "${HOME}/.demarkus/bin/demarkus-plugin"
err="${HOME}/gate.err"
out="$(echo '{"hook_event_name":"beforeMCPExecution","tool_name":"mark_publish"}' | bash "${HERE}/gate.sh" 2>"${err}")" || fail "gate exited non-zero without the binary"
[[ -z "${out}" ]] || fail "gate stdout should be empty without the binary: ${out}"
grep -q 'demarkus-plugin missing' "${err}" || fail "gate should log the missing binary on stderr: $(cat "${err}")"
echo "cursor-knowledge hooks: OK"
