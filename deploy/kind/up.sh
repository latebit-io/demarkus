#!/usr/bin/env bash
# Stage 1 of the kind harness: bring up a single-node kind cluster and install
# the demarkus-server chart from GHCR. Verification runs `demarkus fetch`
# against the server's own QUIC listener via `kubectl exec`, which exercises
# the full read path without exposing anything to the host.
#
# Subsequent stages will layer in the broker (Stage 2) and Argo CD with an
# ApplicationSet over multiple worlds (Stage 3).

set -euo pipefail

CLUSTER="${CLUSTER:-knowledge-system}"
NAMESPACE="${NAMESPACE:-demarkus}"
RELEASE="${RELEASE:-world-default}"
SERVER_CHART_VERSION="${SERVER_CHART_VERSION:-0.17.8}"
SERVER_CHART="oci://ghcr.io/latebit-io/charts/demarkus-server"

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" &>/dev/null && pwd)"
VALUES_FILE="$SCRIPT_DIR/values-kind.yaml"
KIND_CONFIG="$SCRIPT_DIR/kind-config.yaml"

require() {
  command -v "$1" >/dev/null 2>&1 || {
    echo "missing required tool: $1" >&2
    exit 1
  }
}

require kind
require helm
require kubectl

if kind get clusters | grep -qx "$CLUSTER"; then
  echo "--- kind cluster '$CLUSTER' already exists, reusing"
else
  echo "--- creating kind cluster '$CLUSTER'"
  kind create cluster --name "$CLUSTER" --config "$KIND_CONFIG"
fi

kubectl config use-context "kind-$CLUSTER" >/dev/null

echo "--- installing demarkus-server chart $SERVER_CHART_VERSION"
helm upgrade --install "$RELEASE" "$SERVER_CHART" \
  --version "$SERVER_CHART_VERSION" \
  --namespace "$NAMESPACE" --create-namespace \
  --values "$VALUES_FILE" \
  --wait --timeout 5m

POD_SELECTOR="app.kubernetes.io/instance=$RELEASE,app.kubernetes.io/name=demarkus-server"
POD=$(kubectl -n "$NAMESPACE" get pods -l "$POD_SELECTOR" -o jsonpath='{.items[0].metadata.name}')
if [[ -z "$POD" ]]; then
  echo "no demarkus-server pod found for selector: $POD_SELECTOR" >&2
  exit 1
fi
echo "--- waiting for pod $POD"
kubectl -n "$NAMESPACE" wait --for=condition=ready "pod/$POD" --timeout=120s

echo "--- smoke test: fetch /.well-known/agent-manifest.md from inside the pod"
kubectl -n "$NAMESPACE" exec "$POD" -- \
  demarkus -insecure fetch "mark://localhost:6309/.well-known/agent-manifest.md"

# The chart emits two Secrets when emitRawValues=true (default):
#   <release>-demarkus-server-tokens         server-mounted, hash-only TOML
#   <release>-demarkus-server-token-values   raw admin token
# We deliberately do not print the token here — a kind dev cluster is
# ephemeral and the user can pull it on demand with the command shown below.
TOKEN_SECRET="${RELEASE}-demarkus-server-token-values"

cat <<EOF

ready.

cluster:   kind-$CLUSTER
namespace: $NAMESPACE
release:   $RELEASE
service:   $RELEASE-demarkus-server.$NAMESPACE.svc.cluster.local:6309 (UDP)

interact from inside the cluster:
  kubectl -n $NAMESPACE exec -it $POD -- \\
    demarkus -insecure fetch mark://localhost:6309/.well-known/agent-manifest.md

retrieve the admin token (when you need it):
  kubectl -n $NAMESPACE get secret $TOKEN_SECRET -o jsonpath='{.data.admin}' | base64 -d

tear down:
  $SCRIPT_DIR/down.sh
EOF
