#!/usr/bin/env bash
# e2e backend parity: seed a file-backed and a CloudNativePG-backed server on
# kind identically, diff the normalized read sweep (store-parity plan, step 5).
# env overrides: CLUSTER, NAMESPACE, SERVER_IMAGE_TAG, CNPG_CHART_VERSION.

set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" &>/dev/null && pwd)"
REPO_ROOT="$(cd -- "$SCRIPT_DIR/.." &>/dev/null && pwd)"

CLUSTER="${CLUSTER:-knowledge-system}"
NAMESPACE="${NAMESPACE:-parity}"
# First server release whose image carries -store/-pg-dsn.
SERVER_IMAGE_TAG="${SERVER_IMAGE_TAG:-0.22.13}"
CNPG_CHART_VERSION="${CNPG_CHART_VERSION:-0.28.3}"

CHART="$REPO_ROOT/deploy/helm/demarkus-server"
KIND_CONFIG="$REPO_ROOT/deploy/kind/kind-config.yaml"
FILE_VALUES="$REPO_ROOT/deploy/kind/values-kind.yaml"
PG_VALUES="$REPO_ROOT/deploy/kind/values-kind-pg.yaml"
FILE_RELEASE=parity-file
PG_RELEASE=parity-pg
PG_CLUSTER=demarkus-pg
PORT=6309

require() {
  command -v "$1" >/dev/null 2>&1 || { echo "missing required tool: $1" >&2; exit 1; }
}
require kind
require helm
require kubectl
require diff

if ! kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
  echo "--- creating kind cluster $CLUSTER"
  kind create cluster --name "$CLUSTER" --config "$KIND_CONFIG"
fi

echo "--- installing CloudNativePG operator (chart $CNPG_CHART_VERSION)"
helm repo add cnpg https://cloudnative-pg.github.io/charts >/dev/null
helm repo update cnpg >/dev/null
helm upgrade --install cnpg cnpg/cloudnative-pg \
  --version "$CNPG_CHART_VERSION" \
  --namespace cnpg-system --create-namespace \
  --wait --timeout 5m

# Fresh namespace every run: the seed sequence is create-only in places, so
# reruns against leftover state would conflict instead of proving parity.
if kubectl get namespace "$NAMESPACE" >/dev/null 2>&1; then
  echo "--- deleting previous $NAMESPACE namespace"
  helm -n "$NAMESPACE" uninstall "$FILE_RELEASE" "$PG_RELEASE" --ignore-not-found >/dev/null \
    || echo "warning: helm uninstall failed; relying on namespace deletion" >&2
  kubectl delete namespace "$NAMESPACE" --timeout=180s
fi
kubectl create namespace "$NAMESPACE"

echo "--- creating Postgres cluster $PG_CLUSTER"
kubectl -n "$NAMESPACE" apply -f - <<EOF
apiVersion: postgresql.cnpg.io/v1
kind: Cluster
metadata:
  name: $PG_CLUSTER
spec:
  instances: 1
  storage:
    size: 1Gi
EOF
kubectl -n "$NAMESPACE" wait --for=condition=Ready "cluster/$PG_CLUSTER" --timeout=300s

echo "--- installing $FILE_RELEASE (file backend) and $PG_RELEASE (postgres backend)"
helm upgrade --install "$FILE_RELEASE" "$CHART" \
  --namespace "$NAMESPACE" \
  --values "$FILE_VALUES" \
  --set image.tag="$SERVER_IMAGE_TAG" \
  --wait --timeout 5m
helm upgrade --install "$PG_RELEASE" "$CHART" \
  --namespace "$NAMESPACE" \
  --values "$PG_VALUES" \
  --set image.tag="$SERVER_IMAGE_TAG" \
  --wait --timeout 5m

FILE_POD="${FILE_RELEASE}-demarkus-server-0"
PG_POD="${PG_RELEASE}-demarkus-server-0"

admin_token() {
  kubectl -n "$NAMESPACE" get secret "$1-demarkus-server-token-values" \
    -o jsonpath='{.data.admin}' | base64 -d
}
FILE_TOKEN="$(admin_token "$FILE_RELEASE")"
PG_TOKEN="$(admin_token "$PG_RELEASE")"

# cli <pod> <args...> — the request CLI against the pod's own listener.
cli() {
  local pod="$1"; shift
  kubectl -n "$NAMESPACE" exec "$pod" -- /demarkus -insecure -no-cache "$@"
}

# lookup_cli <pod> <args...> <scope> — the lookup subcommand (own flag set).
lookup_cli() {
  local pod="$1"; shift
  kubectl -n "$NAMESPACE" exec "$pod" -- /demarkus lookup -insecure "$@"
}

url() { printf 'mark://localhost:%d%s' "$PORT" "$1"; }

# seed <pod> <token> — the identical write sequence for both backends.
# Every lookup query below matches documents with distinct importance so
# result order never depends on the wall-clock modified tiebreak.
seed() {
  local pod="$1" token="$2"
  echo "--- seeding $pod"
  cli "$pod" -X PUBLISH -auth "$token" \
    -meta tags=hub,parity -meta importance=0.9 \
    -body '# Parity World

Seeded by e2e-backend-parity.sh.
' "$(url /index.md)" >/dev/null
  cli "$pod" -X PUBLISH -auth "$token" -expected-version 0 \
    -meta tags=go,auth -meta importance=0.7 \
    -body '# Alpha

First version.
' "$(url /notes/alpha.md)" >/dev/null
  cli "$pod" -X PUBLISH -auth "$token" -expected-version 1 \
    -meta tags=go,auth -meta importance=0.7 \
    -body '# Alpha

Second version, edited body.
' "$(url /notes/alpha.md)" >/dev/null
  cli "$pod" -X APPEND -auth "$token" -expected-version 2 \
    -body 'Appended line.
' "$(url /notes/alpha.md)" >/dev/null
  cli "$pod" -X PUBLISH -auth "$token" \
    -meta tags=unicode,notes -meta importance=0.5 \
    -body '# Beta — ünïcode ☃

Multibyte content: 日本語 emoji 🎈.
' "$(url /notes/beta.md)" >/dev/null
  cli "$pod" -X PUBLISH -auth "$token" \
    -meta tags=deep,parity -meta importance=0.4 \
    -body '# Gamma

Nested path document.
' "$(url /deep/nested/gamma.md)" >/dev/null
  cli "$pod" -X PUBLISH -auth "$token" \
    -meta tags=stale,notes -meta importance=0.3 \
    -body '# Old

To be archived.
' "$(url /notes/old.md)" >/dev/null
  cli "$pod" -X ARCHIVE -auth "$token" "$(url /notes/old.md)" >/dev/null
}

# normalize — mask wall-clock values so only real divergence diffs.
normalize() {
  sed -E 's/[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]+)?Z/<ts>/g'
}

# record <outfile> <label> <cmd...> — run one check, append its normalized
# output. The exit status is part of the compared output, so an error path
# (archived doc, missing version) must fail identically on both backends.
record() {
  local out="$1" label="$2" status; shift 2
  {
    echo "=== $label"
    if "$@" 2>&1; then status=0; else status=$?; fi
    echo "exit_status=$status"
    echo
  } | normalize >> "$out"
}

# sweep <pod> <token> <outfile> — every read verb over the seeded world.
# Reads authenticate: the admin token's read grant on /** makes every seeded
# path private, so an unauthenticated sweep would only compare error pages.
sweep() {
  local pod="$1" token="$2" out="$3"
  : > "$out"
  local p
  for p in /index.md /notes/alpha.md /notes/beta.md /deep/nested/gamma.md /notes/old.md; do
    record "$out" "FETCH $p" cli "$pod" -auth "$token" "$(url "$p")"
    record "$out" "VERSIONS $p" cli "$pod" -auth "$token" -X VERSIONS "$(url "$p")"
  done
  for p in /notes/alpha.md/v1 /notes/alpha.md/v2 /notes/alpha.md/v3 /notes/alpha.md/v4; do
    record "$out" "FETCH $p" cli "$pod" -auth "$token" "$(url "$p")"
  done
  for p in / /notes/ /deep/ /deep/nested/ /missing/; do
    record "$out" "LIST $p" cli "$pod" -auth "$token" -X LIST "$(url "$p")"
  done
  record "$out" "LIST -include-archived /notes/" cli "$pod" -auth "$token" -X LIST -include-archived "$(url /notes/)"
  local q
  for q in go unicode parity stale nomatch; do
    record "$out" "LOOKUP $q /" lookup_cli "$pod" -auth "$token" -query "$q" "$(url /)"
  done
  record "$out" "LOOKUP parity /deep/" lookup_cli "$pod" -auth "$token" -query parity "$(url /deep/)"
}

seed "$FILE_POD" "$FILE_TOKEN"
seed "$PG_POD" "$PG_TOKEN"

WORKDIR="$(mktemp -d)"
echo "--- sweeping both backends (outputs in $WORKDIR)"
sweep "$FILE_POD" "$FILE_TOKEN" "$WORKDIR/file.txt"
sweep "$PG_POD" "$PG_TOKEN" "$WORKDIR/pg.txt"

echo "--- diffing"
if diff -u "$WORKDIR/file.txt" "$WORKDIR/pg.txt"; then
  echo "PASS: file and postgres backends are observably identical ($(grep -c '^=== ' "$WORKDIR/file.txt") checks)"
else
  echo "FAIL: backend outputs diverge (see diff above; raw sweeps in $WORKDIR)" >&2
  exit 1
fi

cat <<EOF

tear down:
  helm -n $NAMESPACE uninstall $FILE_RELEASE $PG_RELEASE
  kubectl delete namespace $NAMESPACE
  helm -n cnpg-system uninstall cnpg   # optional
EOF
