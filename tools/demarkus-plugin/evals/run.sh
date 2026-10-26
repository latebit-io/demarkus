#!/usr/bin/env bash
# Runs the Claude Code memory plugin's eval suite: the shipped plugin is
# copied beside this suite (claude plugin eval reads evals/ under the plugin)
# so no eval file ships with the plugin. Extra arguments go to the eval.
# Prompts are regenerated and hooks run a working-tree build, so nothing waits
# on a release; the session header is fixed (see session-start.sh below).
set -euo pipefail

cd "$(dirname "$0")/../../.."
go -C tools run ./plugin-prompts write
work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT
cp -R plugins/claude-code/. "${work}/"
go -C tools build -o "${work}/eval-bin/demarkus-plugin" ./demarkus-plugin
cp -R tools/demarkus-plugin/evals "${work}/evals"
rm -rf "${work}/evals/results"

# The sandbox soul is empty, so the real header would contradict the mocks.
# The stand-in installs the build for gates and nudges and prints a header
# per case: EVAL_PROJECT=new in a case's env is a project with no hub.
guidance="$(awk 'BEGIN { ORS = "" }
  !body && (/^<!--.*-->$/ || /^$/) { next }
  { body = 1; gsub(/\\/, "\\\\"); gsub(/"/, "\\\""); gsub(/\t/, "\\t"); print (n++ ? "\\n" : "") $0 }' \
  "${work}/context/session-guidance.md")"
head='Project slug: `cwd`. Project root: `/cwd/`. Bound store: `demarkus-memory`.'
stands='Where it stands: no git facts (eval sandbox).'
for state in known new; do
  case "${state}" in
    known) project='Project: known, newest journal entry `/cwd/journal/2026-10-11.md`.' ;;
    new) project='Project: new, no hub at `/cwd/index.md` yet.' ;;
  esac
  printf '{"hookSpecificOutput":{"hookEventName":"SessionStart","additionalContext":"%s"}}\n' \
    "${head} ${project}\\n\\n${stands}\\n\\n${guidance}" >"${work}/hooks/session-start.${state}.json"
done
# Each run gets a temp HOME; refuse rather than replace the operator's binary.
# No header on failure, so the /cwd/ graders fail: gates and nudges fail open
# and would let a run without the build pass.
stand_in='#!/usr/bin/env bash
set -uo pipefail
HOOK_DIR="$(cd "$(dirname "$0")" && pwd)"
[[ "${HOME}" != "@HOME@" ]] || { echo "[eval] HOME is the operator home; not installing" >&2; exit 1; }
mkdir -p "${HOME}/.demarkus/bin" &&
  install -m 0755 "${HOOK_DIR}/../eval-bin/demarkus-plugin" "${HOME}/.demarkus/bin/" ||
  { echo "[eval] demarkus-plugin install failed" >&2; exit 1; }
cat "${HOOK_DIR}/session-start.${EVAL_PROJECT:-known}.json"'
printf '%s\n' "${stand_in//@HOME@/${HOME}}" >"${work}/hooks/session-start.sh"

mkdir -p tools/demarkus-plugin/evals/results
# A haiku judge misreads correct briefs. Write is gated; soul-export saves
# into the run's sandbox, and other cases do not allow it.
claude plugin eval --trust-plugin --no-publish --ablation none --judge-model sonnet -j 4 --allow-tools Write \
  --output-dir "tools/demarkus-plugin/evals/results/$(date +%Y%m%dT%H%M%S)" "$@" "${work}"
