#!/usr/bin/env bash
# The Go config loaders test against these chart renderings, so a key the
# chart emits that a loader no longer accepts fails a test, not a pod start.
# Usage: check-chart-fixtures.sh [--update]
set -euo pipefail

cd "$(dirname "$0")/.."
chart=deploy/helm/demarkus-knowledge-server

# template|yq expression|fixture
fixtures=(
  "templates/secret-broker-config.yaml|.stringData[\"config.yaml\"]|knowledge/internal/broker/core/testdata/chart-rendered-config.yaml"
  "templates/configmap.yaml|.data[\"config.yaml\"]|server/internal/knowledgeconfig/testdata/chart-rendered-config.yaml"
)

render() {
  helm template knowledge "$chart" -n demarkus \
    -f "$chart/tests/fixtures/valid-values.yaml" \
    --set broker.stateBucket=gs://knowledge-broker-state \
    --set broker.federation.hub=team-a \
    --show-only "$1" | yq "$2" | sed -e '$ { /^$/d; }'
}

fail=0
for entry in "${fixtures[@]}"; do
  IFS='|' read -r template expr fixture <<<"$entry"
  if [ "${1:-}" = "--update" ]; then
    render "$template" "$expr" >"$fixture"
    echo "updated $fixture"
  elif ! render "$template" "$expr" | diff -u "$fixture" -; then
    echo "check-chart-fixtures: $fixture differs from the chart; run scripts/check-chart-fixtures.sh --update" >&2
    fail=1
  fi
done
exit "$fail"
