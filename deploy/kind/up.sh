#!/usr/bin/env bash
# kind harness for demarkus development.
#
# Stage 1 (default):    single-node kind cluster + demarkus-server chart.
# Stage 2 (--with-broker):  + mock-oauth2-server + demarkus-broker chart.
# Stage 3 (--with-argo):    + Argo CD + ApplicationSet templating two worlds.
#
# --with-argo replaces the Stage 1 server install with an Argo-managed
# ApplicationSet so the worlds are GitOps-shaped from the start. It is not
# combinable with --with-broker yet (the broker chart values are hard-wired
# to the Stage 1 release name; multi-world broker wiring is a follow-up).
#
# Verification runs `demarkus` against each server's own QUIC listener via
# `kubectl exec`, which exercises the full read path without exposing
# anything to the host.

set -euo pipefail

CLUSTER="${CLUSTER:-knowledge-system}"
NAMESPACE="${NAMESPACE:-demarkus}"
RELEASE="${RELEASE:-world-default}"
SERVER_CHART_VERSION="${SERVER_CHART_VERSION:-0.17.9}"
SERVER_CHART="oci://ghcr.io/latebit-io/charts/demarkus-server"
BROKER_RELEASE="${BROKER_RELEASE:-broker}"
BROKER_CHART_VERSION="${BROKER_CHART_VERSION:-0.1.1}"
BROKER_CHART="oci://ghcr.io/latebit-io/charts/demarkus-broker"
ARGO_NAMESPACE="${ARGO_NAMESPACE:-argocd}"
ARGO_RELEASE="${ARGO_RELEASE:-argocd}"
ARGO_CHART_VERSION="${ARGO_CHART_VERSION:-7.7.0}"
ARGO_REPO_URL="${ARGO_REPO_URL:-https://argoproj.github.io/argo-helm}"
# Must match the elements list in deploy/k8s/examples/applicationset.yaml.
ARGO_WORLDS=(world-a world-b)

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" &>/dev/null && pwd)"
REPO_ROOT="$(cd -- "$SCRIPT_DIR/../.." &>/dev/null && pwd)"
VALUES_FILE="$SCRIPT_DIR/values-kind.yaml"
BROKER_VALUES_FILE="$SCRIPT_DIR/values-broker.yaml"
ARGO_VALUES_FILE="$SCRIPT_DIR/values-argo.yaml"
MOCK_OIDC_MANIFEST="$SCRIPT_DIR/mock-oidc.yaml"
KIND_CONFIG="$SCRIPT_DIR/kind-config.yaml"
APPLICATIONSET_MANIFEST="$REPO_ROOT/deploy/k8s/examples/applicationset.yaml"

WITH_BROKER=false
WITH_ARGO=false
for arg in "$@"; do
  case "$arg" in
    --with-broker) WITH_BROKER=true ;;
    --with-argo)   WITH_ARGO=true ;;
    -h|--help)
      cat <<EOF
usage: up.sh [--with-broker | --with-argo]

  --with-broker    also install mock-oauth2-server + demarkus-broker chart
  --with-argo      install Argo CD + ApplicationSet templating two worlds
                   (replaces the Stage 1 server install)

env overrides:
  CLUSTER, NAMESPACE, RELEASE, SERVER_CHART_VERSION,
  BROKER_RELEASE, BROKER_CHART_VERSION,
  ARGO_NAMESPACE, ARGO_RELEASE, ARGO_CHART_VERSION, ARGO_REPO_URL
EOF
      exit 0
      ;;
    *) echo "unknown arg: $arg" >&2; exit 2 ;;
  esac
done

if [[ "$WITH_BROKER" == "true" && "$WITH_ARGO" == "true" ]]; then
  cat >&2 <<EOF
--with-broker and --with-argo cannot be combined: the broker chart values
at $BROKER_VALUES_FILE are hard-wired to the Stage 1 release name
'world-default', while --with-argo provisions world-a/world-b instead.
Multi-world broker wiring is a follow-up.
EOF
  exit 2
fi

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

if [[ "$WITH_ARGO" == "true" ]]; then
  echo "--- installing argo-cd chart $ARGO_CHART_VERSION"
  # `helm repo add` is idempotent across reruns when the URL matches; with a
  # different URL it errors. Force-update so reruns don't fail if the user
  # has the repo registered under a different URL.
  helm repo add argo "$ARGO_REPO_URL" --force-update >/dev/null
  helm repo update argo >/dev/null
  helm upgrade --install "$ARGO_RELEASE" argo/argo-cd \
    --version "$ARGO_CHART_VERSION" \
    --namespace "$ARGO_NAMESPACE" --create-namespace \
    --values "$ARGO_VALUES_FILE" \
    --wait --timeout 10m

  echo "--- applying ApplicationSet from $APPLICATIONSET_MANIFEST"
  kubectl apply -f "$APPLICATIONSET_MANIFEST"

  # The ApplicationSet controller has to reconcile before the per-world
  # Applications exist; `kubectl wait` errors out immediately if the named
  # resource is absent, so poll for creation first, then wait on health.
  for world in "${ARGO_WORLDS[@]}"; do
    echo "--- waiting for Application/$world to be created"
    for _ in $(seq 1 60); do
      if kubectl -n "$ARGO_NAMESPACE" get application "$world" >/dev/null 2>&1; then
        break
      fi
      sleep 2
    done
    if ! kubectl -n "$ARGO_NAMESPACE" get application "$world" >/dev/null 2>&1; then
      echo "Application/$world never materialized — check the ApplicationSet controller logs" >&2
      exit 1
    fi
  done

  echo "--- waiting for Applications to become Healthy"
  # Two-step wait: Synced first (chart pulled + manifests applied), then
  # Healthy (workloads ready). Splitting these surfaces a clearer failure
  # mode when OCI pull or RBAC blocks the sync vs. when a pod is crashing.
  for world in "${ARGO_WORLDS[@]}"; do
    kubectl -n "$ARGO_NAMESPACE" wait --for=jsonpath='{.status.sync.status}'=Synced \
      "application/$world" --timeout=5m
    kubectl -n "$ARGO_NAMESPACE" wait --for=jsonpath='{.status.health.status}'=Healthy \
      "application/$world" --timeout=5m
  done

  for world in "${ARGO_WORLDS[@]}"; do
    echo "--- smoke test for world $world"
    WORLD_POD_SELECTOR="app.kubernetes.io/instance=$world,app.kubernetes.io/name=demarkus-server"
    WORLD_POD=$(kubectl -n "$world" get pods -l "$WORLD_POD_SELECTOR" -o jsonpath='{.items[0].metadata.name}')
    if [[ -z "$WORLD_POD" ]]; then
      echo "no demarkus-server pod found in namespace $world for selector $WORLD_POD_SELECTOR" >&2
      exit 1
    fi
    kubectl -n "$world" exec "$WORLD_POD" -- \
      /demarkus -insecure -no-cache "mark://localhost:6309/.well-known/agent-manifest.md"
  done

  cat <<EOF

ready (with argo).

cluster:         kind-$CLUSTER
argo namespace:  $ARGO_NAMESPACE
argo release:    $ARGO_RELEASE
worlds:          ${ARGO_WORLDS[*]}

inspect Applications:
  kubectl -n $ARGO_NAMESPACE get application

open the Argo UI:
  kubectl -n $ARGO_NAMESPACE port-forward svc/$ARGO_RELEASE-server 8080:80
  open http://localhost:8080

retrieve the Argo CD initial admin password:
  kubectl -n $ARGO_NAMESPACE get secret argocd-initial-admin-secret \\
    -o jsonpath='{.data.password}' | base64 -d ; echo

server fetch from inside a world (replace world-a with any of: ${ARGO_WORLDS[*]}):
  kubectl -n world-a exec -it \\
    \$(kubectl -n world-a get pods -l app.kubernetes.io/name=demarkus-server -o jsonpath='{.items[0].metadata.name}') -- \\
    /demarkus -insecure -no-cache mark://localhost:6309/.well-known/agent-manifest.md

tear down:
  $SCRIPT_DIR/down.sh
EOF
  exit 0
fi

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
  /demarkus -insecure -no-cache "mark://localhost:6309/.well-known/agent-manifest.md"

# The chart emits two Secrets when emitRawValues=true (default):
#   <release>-demarkus-server-tokens         server-mounted, hash-only TOML
#   <release>-demarkus-server-token-values   raw admin token
# We deliberately do not print the token here — a kind dev cluster is
# ephemeral and the user can pull it on demand with the command shown below.
TOKEN_SECRET="${RELEASE}-demarkus-server-token-values"

if [[ "$WITH_BROKER" == "true" ]]; then
  echo "--- applying mock-oauth2-server (OIDC issuer for broker discovery)"
  kubectl -n "$NAMESPACE" apply -f "$MOCK_OIDC_MANIFEST"
  kubectl -n "$NAMESPACE" rollout status deployment/mock-oauth2-server --timeout=120s

  echo "--- installing demarkus-broker chart $BROKER_CHART_VERSION"
  # helm install --wait blocks on the broker's readiness probe, which hits
  # /readyz. /readyz only flips green after OIDC discovery + JWKS fetch
  # against the mock issuer have completed, so a successful install here
  # already proves discovery works end-to-end.
  helm upgrade --install "$BROKER_RELEASE" "$BROKER_CHART" \
    --version "$BROKER_CHART_VERSION" \
    --namespace "$NAMESPACE" --create-namespace \
    --values "$BROKER_VALUES_FILE" \
    --wait --timeout 5m

  BROKER_POD_SELECTOR="app.kubernetes.io/instance=$BROKER_RELEASE,app.kubernetes.io/name=demarkus-broker"
  BROKER_POD=$(kubectl -n "$NAMESPACE" get pods -l "$BROKER_POD_SELECTOR" -o jsonpath='{.items[0].metadata.name}')
  if [[ -z "$BROKER_POD" ]]; then
    echo "no demarkus-broker pod found for selector: $BROKER_POD_SELECTOR" >&2
    exit 1
  fi

  cat <<EOF

ready (with broker).

cluster:        kind-$CLUSTER
namespace:      $NAMESPACE
server release: $RELEASE
broker release: $BROKER_RELEASE
broker pod:     $BROKER_POD
server svc:     $RELEASE-demarkus-server.$NAMESPACE.svc.cluster.local:6309 (UDP)
broker svc:     $BROKER_RELEASE-demarkus-broker.$NAMESPACE.svc.cluster.local:8080 (HTTP)
mock OIDC:      mock-oauth2-server.$NAMESPACE.svc.cluster.local:8080/default

probe the broker (from another shell):
  kubectl -n $NAMESPACE port-forward svc/$BROKER_RELEASE-demarkus-broker 8080:8080
  curl http://localhost:8080/healthz
  curl http://localhost:8080/readyz

server fetch from inside the cluster:
  kubectl -n $NAMESPACE exec -it $POD -- \\
    /demarkus -insecure -no-cache mark://localhost:6309/.well-known/agent-manifest.md

retrieve the admin token (when you need it):
  kubectl -n $NAMESPACE get secret $TOKEN_SECRET -o jsonpath='{.data.admin}' | base64 -d

tear down:
  $SCRIPT_DIR/down.sh
EOF
  exit 0
fi

cat <<EOF

ready.

cluster:   kind-$CLUSTER
namespace: $NAMESPACE
release:   $RELEASE
service:   $RELEASE-demarkus-server.$NAMESPACE.svc.cluster.local:6309 (UDP)

interact from inside the cluster:
  kubectl -n $NAMESPACE exec -it $POD -- \\
    /demarkus -insecure -no-cache mark://localhost:6309/.well-known/agent-manifest.md

retrieve the admin token (when you need it):
  kubectl -n $NAMESPACE get secret $TOKEN_SECRET -o jsonpath='{.data.admin}' | base64 -d

add the broker (next stage):
  $0 --with-broker

tear down:
  $SCRIPT_DIR/down.sh
EOF
