#!/usr/bin/env bash
# Runs the Claude Code memory plugin's eval suite: the shipped plugin is
# copied beside this suite (claude plugin eval reads evals/ under the plugin)
# so no eval file ships with the plugin. Extra arguments go to the eval.
# Hooks run the released demarkus-plugin, so a binary change is evaluable
# only after release; the session header is fixed (see session-start.sh below).
set -euo pipefail

cd "$(dirname "$0")/../../.."
work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT
cp -R plugins/claude-code/. "${work}/"
cp -R tools/demarkus-plugin/evals "${work}/evals"
rm -rf "${work}/evals/results"

# The sandbox soul is empty, so the real header would contradict the mocks.
# The stand-in keeps bootstrap (gates and nudges need the binary) and prints
# a header per case: EVAL_PROJECT=new in a case's env is a project with no hub.
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
cat >"${work}/hooks/session-start.sh" <<'EOF'
#!/usr/bin/env bash
set -uo pipefail
HOOK_DIR="$(cd "$(dirname "$0")" && pwd)"
bash "${HOOK_DIR}/../scripts/bootstrap.sh" 1>&2 || echo "[eval] bootstrap failed" >&2
cat "${HOOK_DIR}/session-start.${EVAL_PROJECT:-known}.json"
EOF

mkdir -p tools/demarkus-plugin/evals/results
# A haiku judge misreads correct briefs.
claude plugin eval --trust-plugin --no-publish --ablation none --judge-model sonnet \
  --output-dir "tools/demarkus-plugin/evals/results/$(date +%Y%m%dT%H%M%S)" "$@" "${work}"
