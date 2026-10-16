#!/usr/bin/env bash
# Replace testdata/upgrade with the ~/.demarkus and ~/.mark a released plugin
# writes, which the upgrade tests read with current code. Runs in Docker: its
# own HOME and processes, so a live soul here is untouched.
#
# Usage: bash scripts/capture-upgrade-fixture.sh <tools-version>   e.g. 0.64.3
set -euo pipefail

version="${1:?usage: capture-upgrade-fixture.sh <tools-version>}"
root="$(cd "$(dirname "$0")/.." && pwd)"
out="$root/testdata/upgrade"
# Beside out, not in it: out's .gitignore would un-ignore a leftover.
partial="$root/testdata/upgrade.partial"
trap 'rm -rf "$partial"' EXIT
rm -rf "$partial"
mkdir -p "$partial"

docker run --rm -i -e VERSION="$version" -e DEBIAN_FRONTEND=noninteractive \
  -v "$partial:/out" debian:stable-slim bash -euo pipefail -s <<'CONTAINER'
apt-get update -qq >/dev/null
apt-get install -y -qq curl ca-certificates procps >/dev/null
export HOME=/home/fixture
mkdir -p "$HOME/project" "$HOME/other-project"
arch="$(dpkg --print-architecture)"
base="https://github.com/latebit-io/demarkus/releases/download/tools/v$VERSION"
asset="demarkus-plugin_${VERSION}_linux_${arch}.tar.gz"
cd /tmp
curl -fsSLO "$base/$asset"
curl -fsSLO "$base/demarkus-tools_checksums.txt"
grep " $asset\$" demarkus-tools_checksums.txt | sha256sum -c - >/dev/null
tar -xzf "$asset" -C /usr/local/bin demarkus-plugin

demarkus-plugin provision init default
demarkus-plugin provision status

# Agent writes through the released MCP tools: a hub, a versioned document
# with typed metadata, a journal entry, an archived document, a graph crawl.
# One call per second: a version must land before the next write expects it.
call() {
  printf '{"jsonrpc":"2.0","id":%s,"method":"tools/call","params":{"name":"%s","arguments":%s}}\n' "$1" "$2" "$3"
  sleep 1
}
{
  printf '%s\n' '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"capture","version":"1"}}}'
  printf '%s\n' '{"jsonrpc":"2.0","method":"notifications/initialized"}'
  call 2 mark_publish '{"url":"/fixture/index.md","expected_version":0,"body":"# Fixture\n\n- [Decision](/fixture/adr/0001-decision.md): the first decision\n- [Old note](/fixture/old.md): archived\n","metadata":{"type":"Hub","tags":"fixture,hub","importance":"0.8"}}'
  call 3 mark_publish '{"url":"/fixture/adr/0001-decision.md","expected_version":0,"body":"# Decision\n\nWe keep markdown.\n","metadata":{"type":"ADR","tags":"fixture,decision","importance":"0.7"}}'
  call 4 mark_publish '{"url":"/fixture/adr/0001-decision.md","expected_version":1,"body":"# Decision\n\nWe keep markdown, versioned.\n\n## Why\n\nHistory matters.\n","metadata":{"type":"ADR","tags":"fixture,decision","importance":"0.7","rel-related":"/fixture/index.md"}}'
  call 5 mark_append '{"url":"/fixture/","route":"today","tags":"fixture,journal","body":"\n## Captured\n\nUpgrade fixture written by the released plugin.\n"}'
  call 6 mark_publish '{"url":"/fixture/old.md","expected_version":0,"body":"# Old note\n\nSuperseded.\n","metadata":{"type":"Note","tags":"fixture","importance":"0.2"}}'
  call 7 mark_archive '{"url":"/fixture/old.md"}'
  call 8 mark_graph '{"url":"/fixture/index.md"}'
  sleep 1
} | demarkus-plugin mcp-serve >/tmp/mcp.log
if grep -q '"isError":true' /tmp/mcp.log; then
  cat /tmp/mcp.log >&2
  exit 1
fi

# Registry rows, all offline: a joined QUIC memory bound at join, a second
# one bound as the default of another project, a knowledge system with its
# policy mirror, and a plain promote target.
demarkus-plugin registry memory-join mark://team.example.com:6309 --token fixture-remote-token --bind "$HOME/project"
demarkus-plugin registry memory-join mark://lab.example.com:6309 --token fixture-lab-token
demarkus-plugin registry memory-default --bind "$HOME/other-project" --set lab
demarkus-plugin registry knowledge-register acme
printf '%s\n' '# Policy' '' 'strictness: block' 'require_tags: category' 'allow_tags.category: runbook|decision' >/tmp/policy.md
demarkus-plugin registry policy-mirror acme </tmp/policy.md
demarkus-plugin registry promote-target add archive "$HOME/.demarkus/soul/promoted" "Promoted notes"

if pkill -TERM -f demarkus-server; then
  sleep 1
fi
mkdir /out/home
tar -C "$HOME" --exclude=.demarkus/bin --exclude=.demarkus/logs --exclude=.mark/cache \
  --exclude='*.pid' --exclude='*.lock' -cf - .demarkus .mark | tar -C /out/home -xf -
# The tests read home to rebase the absolute paths the plugin recorded.
printf 'captured: %s\nplugin: %s\nhome: %s\n' "$(date -u +%Y-%m-%d)" "$(demarkus-plugin version)" "$HOME" >/out/CAPTURE
CONTAINER

# One capture, replaced in place: support spans the previous release only,
# and git history keeps the older ones.
rm -rf "${out:?}/home" "${out:?}/CAPTURE"
mv "$partial/home" "$partial/CAPTURE" "$out/"
echo "captured tools/v$version into $out"
