#!/usr/bin/env bash
# setup-pi-repos.sh — split the pi plugins out of this monorepo into their own
# git repositories with `git subtree split` (history preserved), create the
# GitHub repos, and push. Run this from the demarkus monorepo after the plugins
# are committed to the current branch.
#
# Why separate repos: pi's `git:` installer reads a repository's ROOT
# package.json and has no subdirectory syntax, so a monorepo subdir can't be
# `pi install`ed directly. A standalone repo enables the one-liner:
#   pi install git:github.com/latebit-io/demarkus-pi-memory
#
# Re-runnable: it recreates the split branch each time and force-pushes the
# split history to the target repo's default branch, so a later monorepo commit
# to plugins/<name>/ re-syncs with another run.
#
# Usage:
#   plugins/setup-pi-repos.sh                 # split + create + push both
#   plugins/setup-pi-repos.sh --dry-run       # show the plan, touch nothing remote
#   ORG=myorg VISIBILITY=private plugins/setup-pi-repos.sh
#
# Env:
#   ORG         GitHub owner for the new repos (default: latebit-io)
#   VISIBILITY  public|private for `gh repo create` (default: public)
#   BRANCH      default branch name for the new repos (default: main)

set -euo pipefail

ORG="${ORG:-latebit-io}"
VISIBILITY="${VISIBILITY:-public}"
BRANCH="${BRANCH:-main}"
DRY_RUN=0
# Validate args before any destructive (split/create/push) work — a typo'd flag
# must not be silently ignored.
if (( $# > 0 )); then
  case "$1" in
    --dry-run) DRY_RUN=1 ;;
    *) echo "error: unknown argument: $1 (usage: setup-pi-repos.sh [--dry-run])" >&2; exit 2 ;;
  esac
  if (( $# > 1 )); then
    echo "error: too many arguments (usage: setup-pi-repos.sh [--dry-run])" >&2
    exit 2
  fi
fi

# plugin-subdir : target-repo-name
PLUGINS=(
  "plugins/pi-memory:demarkus-pi-memory"
  "plugins/pi-knowledge:demarkus-pi-knowledge"
)

die() { echo "error: $*" >&2; exit 1; }
run() { if (( DRY_RUN )); then echo "  [dry-run] $*"; else echo "  + $*"; "$@"; fi; }

# Must run from the monorepo root (where plugins/ lives) and inside a git repo.
REPO_ROOT="$(git rev-parse --show-toplevel 2>/dev/null)" || die "not inside a git repository"
cd "${REPO_ROOT}"
command -v gh >/dev/null 2>&1 || die "gh CLI is required (brew install gh)"

echo "Splitting pi plugins from ${REPO_ROOT} → ${ORG}/* (${VISIBILITY}), default branch '${BRANCH}'"
(( DRY_RUN )) && echo "(dry run — no branches recreated, no repos created, no pushes)"

for entry in "${PLUGINS[@]}"; do
  prefix="${entry%%:*}"
  repo="${entry##*:}"
  split_branch="pi-split/${repo}"
  echo
  echo "== ${prefix} → ${ORG}/${repo} =="

  # Precondition: the plugin must exist in committed history for subtree to split it.
  git cat-file -e "HEAD:${prefix}/package.json" 2>/dev/null \
    || die "${prefix} is not committed on the current branch yet. Commit the plugin to the monorepo first (it is staged), then re-run."

  # 1) Split the subdir's history into a dedicated branch (recreate if present).
  git rev-parse --verify --quiet "${split_branch}" >/dev/null && run git branch -D "${split_branch}"
  run git subtree split --prefix="${prefix}" -b "${split_branch}"

  # 2) Create the GitHub repo if it doesn't exist yet.
  if gh repo view "${ORG}/${repo}" >/dev/null 2>&1; then
    echo "  repo ${ORG}/${repo} already exists — will push to it"
  else
    desc="$(awk 'NR==1{sub(/^# /,"");print;exit}' "${prefix}/README.md" 2>/dev/null || echo "${repo}")"
    run gh repo create "${ORG}/${repo}" --"${VISIBILITY}" --description "${repo} — pi extension (split from latebit-io/demarkus)" --disable-wiki
  fi

  # 3) Push the split history to the repo's default branch.
  remote_url="https://github.com/${ORG}/${repo}.git"
  run git push --force "${remote_url}" "${split_branch}:${BRANCH}"

  # 4) Tidy the local split branch.
  run git branch -D "${split_branch}"

  echo "  done → pi install git:github.com/${ORG}/${repo}"
done

echo
echo "All set. Install with:"
echo "  pi install npm:pi-mcp-adapter"
echo "  pi install git:github.com/${ORG}/demarkus-pi-memory"
echo "  pi install git:github.com/${ORG}/demarkus-pi-knowledge"
