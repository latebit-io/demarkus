#!/usr/bin/env bash
# kind harness for demarkus development.
#
# Stage 1 (default):                          kind cluster + demarkus-server chart.
# Stage 2 (--with-broker):                    + mock-oauth2-server + demarkus-broker.
# Stage 3 (--with-argo):                      + Argo CD + ApplicationSet templating two worlds.
# Stage 4 (--with-argo --with-broker):         + broker against the Argo worlds, then
#                                              drive a full OIDC mint flow via curl and
#                                              assert tokens land in BOTH world Secrets.
#
# Stage 3 replaces the Stage 1 server install with an Argo-managed
# ApplicationSet; the worlds are GitOps-shaped from the start. Stage 4 layers
# the broker on top of Stage 3 and uses deploy/kind/values-broker-argo.yaml
# (multi-world wiring + server.insecureCookies=true so curl can drive the OIDC
# state cookie over plain HTTP).
#
# Verification runs `demarkus` against each server's own QUIC listener via
# `kubectl exec`, which exercises the full read path without exposing
# anything to the host. Stage 4 also runs an ephemeral curl pod that drives
# /auth/login -> /authorize -> /auth/callback and asserts the broker's
# mint response references both worlds.

set -euo pipefail

CLUSTER="${CLUSTER:-knowledge-system}"
NAMESPACE="${NAMESPACE:-demarkus}"
RELEASE="${RELEASE:-world-default}"
SERVER_CHART_VERSION="${SERVER_CHART_VERSION:-0.17.9}"
SERVER_CHART="oci://ghcr.io/latebit-io/charts/demarkus-server"
BROKER_RELEASE="${BROKER_RELEASE:-broker}"
BROKER_CHART_VERSION="${BROKER_CHART_VERSION:-0.1.3}"
BROKER_CHART="oci://ghcr.io/latebit-io/charts/demarkus-broker"
# Ephemeral curl pod image used by the Stage 4 mint smoke test. Pinned so
# behavior is reproducible across hosts; busybox sh + curl is all we need.
MINT_CURL_IMAGE="${MINT_CURL_IMAGE:-curlimages/curl:8.11.1}"
# Hard-coded to match deploy/k8s/examples/applicationset.yaml which pins
# metadata.namespace: argocd. Making this env-overridable would silently
# break: the script would install Argo into the override namespace while
# the ApplicationSet still landed in argocd, and the kubectl waits below
# would target the wrong namespace.
ARGO_NAMESPACE=argocd
ARGO_RELEASE="${ARGO_RELEASE:-argocd}"
ARGO_CHART_VERSION="${ARGO_CHART_VERSION:-7.7.0}"
ARGO_REPO_URL="${ARGO_REPO_URL:-https://argoproj.github.io/argo-helm}"
# Must match the elements list in deploy/k8s/examples/applicationset.yaml.
ARGO_WORLDS=(world-a world-b)

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" &>/dev/null && pwd)"
REPO_ROOT="$(cd -- "$SCRIPT_DIR/../.." &>/dev/null && pwd)"
VALUES_FILE="$SCRIPT_DIR/values-kind.yaml"
BROKER_VALUES_FILE="$SCRIPT_DIR/values-broker.yaml"
BROKER_ARGO_VALUES_FILE="$SCRIPT_DIR/values-broker-argo.yaml"
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
usage: up.sh [--with-broker] [--with-argo]

  --with-broker    install mock-oauth2-server + demarkus-broker chart
  --with-argo      install Argo CD + ApplicationSet templating two worlds
                   (replaces the Stage 1 server install)

  Combine both for Stage 4: Argo-managed worlds + broker wired across
  them. up.sh then drives a full OIDC mint flow via curl and asserts
  tokens land in BOTH world Secrets.

env overrides:
  CLUSTER, NAMESPACE, RELEASE, SERVER_CHART_VERSION,
  BROKER_RELEASE, BROKER_CHART_VERSION,
  ARGO_RELEASE, ARGO_CHART_VERSION, ARGO_REPO_URL,
  MINT_CURL_IMAGE
EOF
      exit 0
      ;;
    *) echo "unknown arg: $arg" >&2; exit 2 ;;
  esac
done

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

  # Stage 4: layer the broker on top of the Argo-managed worlds and drive
  # a real OIDC mint. The broker is universe-singleton (one per ApplicationSet
  # multi-world deployment), not per-world, so it installs via helm direct
  # rather than through Argo — see /journal/2026-05-14.md for why we didn't
  # template it inside the ApplicationSet.
  if [[ "$WITH_BROKER" == "true" ]]; then
    echo "--- applying mock-oauth2-server (OIDC issuer; JSON_CONFIG sets interactiveLogin=false so curl can drive /authorize without an HTML form)"
    kubectl create namespace "$NAMESPACE" --dry-run=client -o yaml | kubectl apply -f -
    kubectl -n "$NAMESPACE" apply -f "$MOCK_OIDC_MANIFEST"
    kubectl -n "$NAMESPACE" rollout status deployment/mock-oauth2-server --timeout=120s

    echo "--- installing demarkus-broker chart $BROKER_CHART_VERSION (multi-world wiring)"
    # helm --wait blocks on /readyz, which only flips green after OIDC
    # discovery succeeds. RBAC fan-out across world-a + world-b namespaces
    # also runs at install time — a failure here means the cross-namespace
    # Role/RoleBinding pattern broke under multi-world.
    helm upgrade --install "$BROKER_RELEASE" "$BROKER_CHART" \
      --version "$BROKER_CHART_VERSION" \
      --namespace "$NAMESPACE" --create-namespace \
      --values "$BROKER_ARGO_VALUES_FILE" \
      --wait --timeout 5m

    BROKER_POD_SELECTOR="app.kubernetes.io/instance=$BROKER_RELEASE,app.kubernetes.io/name=demarkus-broker"
    BROKER_POD=$(kubectl -n "$NAMESPACE" get pods -l "$BROKER_POD_SELECTOR" -o jsonpath='{.items[0].metadata.name}')
    if [[ -z "$BROKER_POD" ]]; then
      echo "no demarkus-broker pod found for selector: $BROKER_POD_SELECTOR" >&2
      exit 1
    fi

    echo "--- driving OIDC mint flow from an ephemeral curl pod"
    # We synthesize the redirect chain by hand rather than letting curl
    # follow -L. The reason: the broker's redirectURL points at localhost
    # (the cookie's Path=/auth/callback scope only matches if the host
    # part is the same as the broker we hit), so we have to re-target the
    # callback URL at the broker's in-cluster Service while preserving the
    # signed state cookie. -L would chase the redirect to literal
    # localhost:8080 inside the curl pod, which is itself.
    kubectl run -n "$NAMESPACE" mint-smoke --rm -i --restart=Never \
      --image="$MINT_CURL_IMAGE" --command -- sh -c '
set -eu

BROKER=http://'"$BROKER_RELEASE"'-demarkus-broker.'"$NAMESPACE"'.svc.cluster.local:8080

# Hard timeouts so a DNS or TCP stall fails the smoke test in seconds
# instead of pinning the kubectl-run pod open until cluster cleanup.
CURL="curl -sS --connect-timeout 5 --max-time 15"

# 0. Wait for the broker Service to be reachable. helm --wait returned
#    on /readyz green, but the Service endpoints object can lag the pod-
#    Ready transition by a second or two, and curl on a fresh pod that
#    races that window sees "Failed to connect after 1 ms".
for attempt in $(seq 1 15); do
  if $CURL -o /dev/null "$BROKER/healthz"; then
    break
  fi
  sleep 2
done
$CURL -f -o /dev/null "$BROKER/healthz" || { echo "FAIL: broker unreachable after retries"; exit 1; }

# 1. /auth/login — broker signs a state cookie (Secure dropped because
#    server.insecureCookies=true in values-broker-argo.yaml) and 302s
#    to mock-oauth2-server.
$CURL -c /tmp/cookies -D /tmp/login.h -o /dev/null "$BROKER/auth/login"
IDP=$(awk "/^[Ll]ocation:/{print \$2}" /tmp/login.h | tr -d "\r")
[ -n "$IDP" ] || { echo "FAIL: no Location from /auth/login"; cat /tmp/login.h; exit 1; }

# 2. /authorize — mock-oauth2-server is configured with
#    interactiveLogin=false so it 302s back with code+state.
$CURL -D /tmp/idp.h -o /dev/null "$IDP"
CB=$(awk "/^[Ll]ocation:/{print \$2}" /tmp/idp.h | tr -d "\r")
[ -n "$CB" ] || { echo "FAIL: no Location from /authorize"; cat /tmp/idp.h; exit 1; }

# 3. Extract code+state from the callback URL the IdP redirected to.
CODE=$(printf "%s" "$CB" | sed -n "s/.*[?&]code=\([^&]*\).*/\1/p")
STATE=$(printf "%s" "$CB" | sed -n "s/.*[?&]state=\([^&]*\).*/\1/p")
[ -n "$CODE" ] && [ -n "$STATE" ] || { echo "FAIL: bad callback URL: $CB"; exit 1; }

# 4. /auth/callback against the broker (not the literal localhost the
#    IdP redirected to). The state cookie from step 1 replays.
MINT=$($CURL -b /tmp/cookies "$BROKER/auth/callback?code=$CODE&state=$STATE")

# 5. Print the mint response with the raw bearer tokens redacted. Worlds,
#    labels, and expiry stay visible for debugging; the secret material
#    that the broker just minted does NOT land in terminal/CI logs.
SAFE=$(echo "$MINT" | sed "s/\"token\":\"[^\"]*\"/\"token\":\"REDACTED\"/g")
echo "mint response: $SAFE"

# 6. Both worlds must appear in the mint response. The broker writes a
#    token into each world Secret as part of Mint() before returning
#    200, so this implicitly asserts the cross-namespace RBAC worked.
echo "$MINT" | grep -q "\"world\":\"world-a\"" || { echo "FAIL: world-a missing from mint response"; exit 1; }
echo "$MINT" | grep -q "\"world\":\"world-b\"" || { echo "FAIL: world-b missing from mint response"; exit 1; }
echo "OK: mint succeeded for both worlds"
'

    cat <<EOF

ready (with argo + broker).

cluster:         kind-$CLUSTER
argo namespace:  $ARGO_NAMESPACE
broker ns:       $NAMESPACE
broker release:  $BROKER_RELEASE
broker pod:      $BROKER_POD
worlds:          ${ARGO_WORLDS[*]}
mock OIDC:       mock-oauth2-server.$NAMESPACE.svc.cluster.local:8080/default

inspect minted tokens in each world (decoded TOML):
  for w in ${ARGO_WORLDS[*]}; do
    echo "=== \$w ==="
    kubectl -n \$w get secret \$w-demarkus-server-tokens -o jsonpath='{.data.tokens\\.toml}' | base64 -d
  done

re-run the mint flow (handy for poking at logs):
  kubectl run -n $NAMESPACE mint-replay --rm -i --restart=Never \\
    --image=$MINT_CURL_IMAGE -- curl -sS http://$BROKER_RELEASE-demarkus-broker:8080/healthz

tear down:
  $SCRIPT_DIR/down.sh
EOF
    exit 0
  fi

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
