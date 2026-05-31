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
#
# --with-mcp-smoke (layered on Stage 2) additionally drives the RFC 6749
# authorization_code + PKCE flow end-to-end from a curl pod:
# /oauth/authorize -> mock IdP -> /auth/callback -> loopback redirect with a
# broker-minted code -> /device/token exchange, with negative (wrong
# verifier -> invalid_grant) and replay (one-shot code) assertions. This is
# the OAuth surface Claude Code's MCP SDK uses via /knowledge-join.

set -euo pipefail

CLUSTER="${CLUSTER:-knowledge-system}"
NAMESPACE="${NAMESPACE:-demarkus}"
RELEASE="${RELEASE:-world-default}"
SERVER_CHART_VERSION="${SERVER_CHART_VERSION:-0.17.9}"
SERVER_CHART="oci://ghcr.io/latebit-io/charts/demarkus-server"
BROKER_RELEASE="${BROKER_RELEASE:-broker}"
BROKER_CHART_VERSION="${BROKER_CHART_VERSION:-0.1.15}"
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
WITH_MCP_SMOKE=false
for arg in "$@"; do
  case "$arg" in
    --with-broker)   WITH_BROKER=true ;;
    --with-argo)     WITH_ARGO=true ;;
    --with-mcp-smoke) WITH_MCP_SMOKE=true ;;
    -h|--help)
      cat <<EOF
usage: up.sh [--with-broker] [--with-argo] [--with-mcp-smoke]

  --with-broker     install mock-oauth2-server + demarkus-broker chart
  --with-argo       install Argo CD + ApplicationSet templating two worlds
                    (replaces the Stage 1 server install)
  --with-mcp-smoke  rebuild the broker image from this checkout,
                    sideload it into kind, install the LOCAL broker
                    chart (deploy/helm/demarkus-broker) with that image,
                    run smoke checks against the MCP gateway listener
                    (\${BROKER_RELEASE}-...:8081), then drive the RFC 6749
                    authorization_code + PKCE flow end-to-end against the
                    broker's :8080 OAuth surface (the surface behind
                    /knowledge-join). Requires --with-broker; not
                    compatible with --with-argo (Stage 4 expects the
                    published chart artifact).

  Combine --with-broker + --with-argo for Stage 4: Argo-managed worlds
  + broker wired across them. up.sh then drives a full OIDC mint flow
  via curl and asserts tokens land in BOTH world Secrets.

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

if [[ "$WITH_MCP_SMOKE" == "true" ]]; then
  if [[ "$WITH_BROKER" != "true" ]]; then
    echo "--with-mcp-smoke requires --with-broker" >&2
    exit 2
  fi
  if [[ "$WITH_ARGO" == "true" ]]; then
    # Stage 4 pulls the published broker chart from OCI and pins its
    # version for reproducibility. Mixing in a local-chart/local-image
    # build would invert that invariant. Run the MCP smoke against
    # Stage 2 instead, then layer Argo on a separate harness invocation
    # if both shapes are needed.
    echo "--with-mcp-smoke is not compatible with --with-argo (Stage 4 pins the OCI chart)" >&2
    exit 2
  fi
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
require openssl
if [[ "$WITH_MCP_SMOKE" == "true" ]]; then
  require docker
  require make
fi

# ensure_broker_signing_key generates a fresh ECDSA P-256 PEM and
# applies it as a Kubernetes Secret named `broker-signing-key`
# (data key `signing-key.pem`) in the given namespace. PR4 review
# called out checked-in test PEMs as a hygiene problem; this keeps
# the key material ephemeral — generated once per harness run, never
# committed to the repo. The chart's existingSigningKeyRef picks
# the Secret up via secretKeyRef, mounting BROKER_SIGNING_KEY into
# the broker pod at startup.
#
# Idempotent: re-runs replace the Secret in place, so a `up.sh`
# rerun against an existing cluster rotates the broker's signing
# key. This is desirable for kind — the cluster is ephemeral and
# rotation surfaces any kid-pinning bugs in PR5+.
ensure_broker_signing_key() {
  local ns="$1"
  echo "--- generating ephemeral broker signing key (ECDSA P-256) for namespace $ns"
  local tmpfile
  tmpfile=$(mktemp)
  # Function-scoped RETURN trap: cleanup runs whether the function
  # exits normally OR via `set -e` propagation when a downstream
  # command (openssl / kubectl) fails. Without this, a mid-function
  # abort would leave the private-key PEM in /tmp until the
  # next tmpfiles sweep — visible to any local process during the
  # window. RETURN is function-local in bash, so this does not
  # clobber outer EXIT/ERR traps.
  trap 'rm -f "$tmpfile"' RETURN
  # No `2>/dev/null` on openssl — a real failure here (missing
  # P-256 support on the host openssl build, /tmp disk pressure)
  # needs to surface in the harness log; silencing it would turn
  # a recoverable misconfiguration into "broker pod fails to
  # start" minutes later with no breadcrumb.
  openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out "$tmpfile"
  kubectl create namespace "$ns" --dry-run=client -o yaml | kubectl apply -f -
  kubectl -n "$ns" create secret generic broker-signing-key \
    --from-file=signing-key.pem="$tmpfile" \
    --dry-run=client -o yaml | kubectl apply -f -
}

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

    ensure_broker_signing_key "$NAMESPACE"

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

  ensure_broker_signing_key "$NAMESPACE"

  if [[ "$WITH_MCP_SMOKE" == "true" ]]; then
    # --with-mcp-smoke replaces the OCI broker install with a local-chart
    # + locally-built-image install so the MCP smoke checks below actually
    # exercise THIS BRANCH's chart wiring (server.mcp.*, the `mcp`
    # Service port, the NetworkPolicy port admission). Image tag is
    # ephemeral so subsequent --with-mcp-smoke runs always rebuild from
    # the current checkout.
    MCP_SMOKE_IMAGE_REGISTRY=demarkus-mcp-smoke
    MCP_SMOKE_IMAGE_TAG="local-$(date +%s)"
    echo "--- building demarkus-broker image $MCP_SMOKE_IMAGE_REGISTRY/demarkus-broker:$MCP_SMOKE_IMAGE_TAG"
    ( cd "$REPO_ROOT" && \
        make image-broker "IMAGE_REGISTRY=$MCP_SMOKE_IMAGE_REGISTRY" "TAG=$MCP_SMOKE_IMAGE_TAG" )
    echo "--- sideloading image into kind cluster $CLUSTER"
    kind load docker-image "$MCP_SMOKE_IMAGE_REGISTRY/demarkus-broker:$MCP_SMOKE_IMAGE_TAG" --name "$CLUSTER"

    echo "--- installing LOCAL demarkus-broker chart from $REPO_ROOT/deploy/helm/demarkus-broker"
    # server.insecureCookies=true is set here (not in values-broker.yaml) so
    # only the --with-mcp-smoke install drops the Secure attribute on the
    # OIDC state cookie. The auth-code smoke below drives /oauth/authorize ->
    # /auth/callback over plain HTTP inside the curl pod; a Secure cookie
    # would not replay on the callback leg and the flow would fail at state
    # validation. The plain Stage 2 install (values-broker.yaml) stays
    # untouched because it never drives a login flow.
    helm upgrade --install "$BROKER_RELEASE" "$REPO_ROOT/deploy/helm/demarkus-broker" \
      --namespace "$NAMESPACE" --create-namespace \
      --values "$BROKER_VALUES_FILE" \
      --set "server.insecureCookies=true" \
      --set "image.repository=$MCP_SMOKE_IMAGE_REGISTRY/demarkus-broker" \
      --set "image.tag=$MCP_SMOKE_IMAGE_TAG" \
      --set "image.pullPolicy=IfNotPresent" \
      --wait --timeout 5m
  else
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
  fi

  BROKER_POD_SELECTOR="app.kubernetes.io/instance=$BROKER_RELEASE,app.kubernetes.io/name=demarkus-broker"
  BROKER_POD=$(kubectl -n "$NAMESPACE" get pods -l "$BROKER_POD_SELECTOR" -o jsonpath='{.items[0].metadata.name}')
  if [[ -z "$BROKER_POD" ]]; then
    echo "no demarkus-broker pod found for selector: $BROKER_POD_SELECTOR" >&2
    exit 1
  fi

  if [[ "$WITH_MCP_SMOKE" == "true" ]]; then
    # MCP gateway smoke checks. These three prove the chart's MCP listener is:
    #   1. bound (port 8081 reachable through the Service)
    #   2. serving RFC 9728 OAuth metadata (well-known endpoint)
    #   3. enforcing auth on /mcp (401 + WWW-Authenticate challenge)
    # That is the smallest evidence that Slice 7's chart wiring works. The
    # auth-code + PKCE flow that backs /knowledge-join is then driven
    # end-to-end against the broker's :8080 OAuth surface in a second pod
    # below (broker-auth-code-grant plan, PR3).
    echo "--- driving MCP gateway smoke checks from an ephemeral curl pod"
    BROKER_MCP_URL="http://$BROKER_RELEASE-demarkus-broker.$NAMESPACE.svc.cluster.local:8081"
    kubectl run -n "$NAMESPACE" mcp-smoke --rm -i --restart=Never \
      --image="$MINT_CURL_IMAGE" --command -- sh -c '
set -eu
BROKER_MCP='"$BROKER_MCP_URL"'
CURL="curl -sS --connect-timeout 5 --max-time 15"

# Wait for the MCP Service endpoint to be reachable. Service object
# Endpoints can lag the Pod-Ready transition by a beat or two even
# after helm --wait returns, so a curl on a fresh pod can race.
for attempt in $(seq 1 15); do
  HTTP=$($CURL -o /dev/null -w "%{http_code}" "$BROKER_MCP/.well-known/oauth-protected-resource" 2>/dev/null || echo "")
  if [ "$HTTP" = "200" ]; then break; fi
  sleep 2
done

# 1. RFC 9728 metadata endpoint. Returns the resource server
#    identity + a pointer at the auth-server metadata. No auth.
META=$($CURL "$BROKER_MCP/.well-known/oauth-protected-resource")
echo "$META" | grep -q "resource" || { echo "FAIL: oauth-protected-resource missing resource field"; echo "$META"; exit 1; }
echo "OK: /.well-known/oauth-protected-resource"

# 2. RFC 8414 metadata endpoint. Auth-server metadata — aliases
#    the broker OIDC Discovery handler. Plain GET, no auth.
META=$($CURL "$BROKER_MCP/.well-known/oauth-authorization-server")
echo "$META" | grep -q "issuer" || { echo "FAIL: oauth-authorization-server missing issuer field"; echo "$META"; exit 1; }
echo "OK: /.well-known/oauth-authorization-server"

# 3. /mcp without auth must 401 with a WWW-Authenticate challenge
#    per RFC 6750 + RFC 9728 — point fresh clients at the metadata
#    endpoint they need to bootstrap the auth flow.
HDR=$($CURL -D - -o /dev/null -X POST "$BROKER_MCP/mcp" -H "Content-Type: application/json" -d "{}")
echo "$HDR" | grep -qi "^HTTP/.* 401" || { echo "FAIL: POST /mcp without bearer did not 401"; echo "$HDR"; exit 1; }
echo "$HDR" | grep -qi "^WWW-Authenticate: Bearer" || { echo "FAIL: 401 response missing WWW-Authenticate: Bearer"; echo "$HDR"; exit 1; }
echo "OK: POST /mcp 401 + WWW-Authenticate"
'
    echo "--- MCP gateway smoke checks passed"

    # Auth-code + PKCE end-to-end (broker-auth-code-grant plan, PR3).
    # The three checks above prove the gateway demands a bearer; this
    # proves the broker can actually MINT one via the RFC 6749
    # authorization_code grant that Claude Code's MCP SDK drives — the
    # exact surface that was a stub (`unsupported_response_type`) before
    # PRs #155/#156. PKCE is S256-only; we compute the verifier +
    # challenge on the host (openssl is already a harness requirement)
    # and pass them into the pod, because the curlimages/curl pod has no
    # guaranteed openssl for the base64url(sha256(verifier)) derivation.
    AUTHCODE_CLIENT_ID="mcp-smoke-client"
    # Loopback redirect per RFC 8252 — the broker refuses anything else.
    # The port is arbitrary: nothing ever connects to it, we only parse
    # the broker's 302 Location to recover the authorization code.
    AUTHCODE_REDIRECT_URI="http://127.0.0.1:33000/callback"
    AUTHCODE_CLIENT_STATE="$(openssl rand -hex 16)"
    AUTHCODE_VERIFIER="$(openssl rand -hex 32)"
    AUTHCODE_CHALLENGE="$(printf '%s' "$AUTHCODE_VERIFIER" \
      | openssl dgst -sha256 -binary | openssl base64 -A | tr '+/' '-_' | tr -d '=')"

    echo "--- driving auth-code + PKCE flow from an ephemeral curl pod"
    BROKER_OAUTH_URL="http://$BROKER_RELEASE-demarkus-broker.$NAMESPACE.svc.cluster.local:8080"
    kubectl run -n "$NAMESPACE" authcode-smoke --rm -i --restart=Never \
      --image="$MINT_CURL_IMAGE" --command -- sh -c '
set -eu
BROKER='"$BROKER_OAUTH_URL"'
CLIENT_ID='"$AUTHCODE_CLIENT_ID"'
REDIRECT_URI='"$AUTHCODE_REDIRECT_URI"'
CLIENT_STATE='"$AUTHCODE_CLIENT_STATE"'
VERIFIER='"$AUTHCODE_VERIFIER"'
CHALLENGE='"$AUTHCODE_CHALLENGE"'
CURL="curl -sS --connect-timeout 5 --max-time 15"

# 0. Wait for the broker OAuth Service to answer (Endpoints can lag
#    helm --wait by a beat or two on a fresh pod).
for attempt in $(seq 1 15); do
  if $CURL -o /dev/null "$BROKER/healthz"; then break; fi
  sleep 2
done
$CURL -f -o /dev/null "$BROKER/healthz" || { echo "FAIL: broker OAuth surface unreachable"; exit 1; }

# 1. GET /oauth/authorize with PKCE S256. curl -G --data-urlencode
#    encodes each query param (redirect_uri has reserved chars). The
#    broker validates loopback redirect_uri + S256 challenge, sets the
#    signed state cookie (Secure dropped via insecureCookies above), and
#    302s to the mock IdP.
$CURL -G -c /tmp/cookies -D /tmp/authz.h -o /dev/null \
  --data-urlencode "response_type=code" \
  --data-urlencode "client_id=$CLIENT_ID" \
  --data-urlencode "redirect_uri=$REDIRECT_URI" \
  --data-urlencode "code_challenge=$CHALLENGE" \
  --data-urlencode "code_challenge_method=S256" \
  --data-urlencode "state=$CLIENT_STATE" \
  "$BROKER/oauth/authorize"
IDP=$(awk "/^[Ll]ocation:/{print \$2}" /tmp/authz.h | tr -d "\r")
[ -n "$IDP" ] || { echo "FAIL: no Location from /oauth/authorize"; cat /tmp/authz.h; exit 1; }

# 2. Mock IdP /authorize (interactiveLogin=false) auto-approves and 302s
#    back to the broker redirectURL with code+state.
$CURL -D /tmp/idp.h -o /dev/null "$IDP"
CB=$(awk "/^[Ll]ocation:/{print \$2}" /tmp/idp.h | tr -d "\r")
[ -n "$CB" ] || { echo "FAIL: no Location from IdP /authorize"; cat /tmp/idp.h; exit 1; }
IDP_CODE=$(printf "%s" "$CB" | sed -n "s/.*[?&]code=\([^&]*\).*/\1/p")
IDP_STATE=$(printf "%s" "$CB" | sed -n "s/.*[?&]state=\([^&]*\).*/\1/p")
[ -n "$IDP_CODE" ] && [ -n "$IDP_STATE" ] || { echo "FAIL: bad IdP callback URL: $CB"; exit 1; }

# 3. Broker /auth/callback, replaying the state cookie (re-targeted at the
#    in-cluster Service, not the literal localhost the IdP echoed). The
#    AuthCodeID in the signed state routes this to authCodeCallback, which
#    exchanges the IdP code, mints a broker authorization code, and 302s to
#    OUR loopback redirect_uri with code+state+iss.
$CURL -b /tmp/cookies -D /tmp/cb.h -o /dev/null "$BROKER/auth/callback?code=$IDP_CODE&state=$IDP_STATE"
RU=$(awk "/^[Ll]ocation:/{print \$2}" /tmp/cb.h | tr -d "\r")
[ -n "$RU" ] || { echo "FAIL: no Location from /auth/callback"; cat /tmp/cb.h; exit 1; }
case "$RU" in
  *"error="*) echo "FAIL: auth-code callback returned an OAuth error: $RU"; exit 1 ;;
esac
AUTH_CODE=$(printf "%s" "$RU" | sed -n "s/.*[?&]code=\([^&]*\).*/\1/p")
RET_STATE=$(printf "%s" "$RU" | sed -n "s/.*[?&]state=\([^&]*\).*/\1/p")
[ -n "$AUTH_CODE" ] || { echo "FAIL: no broker authorization code in redirect: $RU"; exit 1; }
[ "$RET_STATE" = "$CLIENT_STATE" ] || { echo "FAIL: state mismatch (CSRF guard): got [$RET_STATE] want [$CLIENT_STATE]"; exit 1; }
echo "$RU" | grep -q "iss=" || { echo "FAIL: success redirect missing iss (RFC 9207)"; exit 1; }
echo "OK: /oauth/authorize -> broker authorization code issued (state echoed, iss present)"

# 4. NEGATIVE — a WRONG code_verifier must be rejected, proving the PKCE
#    challenge is actually checked. Redeem preserves the one-shot code on a
#    verifier mismatch, so the positive exchange in step 5 still succeeds.
HTTP=$($CURL -o /tmp/neg.json -w "%{http_code}" -X POST "$BROKER/device/token" \
  --data-urlencode "grant_type=authorization_code" \
  --data-urlencode "code=$AUTH_CODE" \
  --data-urlencode "client_id=$CLIENT_ID" \
  --data-urlencode "redirect_uri=$REDIRECT_URI" \
  --data-urlencode "code_verifier=wrong-$VERIFIER")
[ "$HTTP" = "400" ] || { echo "FAIL: wrong code_verifier did not 400 (got $HTTP)"; cat /tmp/neg.json; exit 1; }
grep -q "invalid_grant" /tmp/neg.json || { echo "FAIL: wrong code_verifier not invalid_grant"; cat /tmp/neg.json; exit 1; }
echo "OK: wrong code_verifier rejected with invalid_grant (PKCE enforced)"

# 5. POSITIVE — the correct verifier mints a Bearer token. Assert response
#    shape only; the raw token material never lands in CI logs.
HTTP=$($CURL -o /tmp/tok.json -w "%{http_code}" -X POST "$BROKER/device/token" \
  --data-urlencode "grant_type=authorization_code" \
  --data-urlencode "code=$AUTH_CODE" \
  --data-urlencode "client_id=$CLIENT_ID" \
  --data-urlencode "redirect_uri=$REDIRECT_URI" \
  --data-urlencode "code_verifier=$VERIFIER")
[ "$HTTP" = "200" ] || { echo "FAIL: authorization_code exchange did not 200 (got $HTTP)"; cat /tmp/tok.json; exit 1; }
grep -q "\"token_type\":\"Bearer\"" /tmp/tok.json || { echo "FAIL: token_type Bearer missing from token response"; exit 1; }
grep -q "\"access_token\":\"..*\"" /tmp/tok.json || { echo "FAIL: access_token missing/empty in token response"; exit 1; }
echo "OK: authorization_code + verifier exchange minted a Bearer token"

# 6. REPLAY — the code is one-shot; re-presenting it with the correct
#    verifier must now fail.
HTTP=$($CURL -o /tmp/replay.json -w "%{http_code}" -X POST "$BROKER/device/token" \
  --data-urlencode "grant_type=authorization_code" \
  --data-urlencode "code=$AUTH_CODE" \
  --data-urlencode "client_id=$CLIENT_ID" \
  --data-urlencode "redirect_uri=$REDIRECT_URI" \
  --data-urlencode "code_verifier=$VERIFIER")
[ "$HTTP" = "400" ] || { echo "FAIL: consumed code replay did not 400 (got $HTTP)"; cat /tmp/replay.json; exit 1; }
echo "OK: authorization code is one-shot (replay rejected)"
echo "OK: auth-code + PKCE flow end-to-end"
'
    echo "--- auth-code + PKCE flow passed"
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
broker MCP svc: $BROKER_RELEASE-demarkus-broker.$NAMESPACE.svc.cluster.local:8081 (HTTP)
mock OIDC:      mock-oauth2-server.$NAMESPACE.svc.cluster.local:8080/default

probe the broker (from another shell):
  kubectl -n $NAMESPACE port-forward svc/$BROKER_RELEASE-demarkus-broker 8080:8080
  curl http://localhost:8080/healthz
  curl http://localhost:8080/readyz

probe the MCP gateway:
  kubectl -n $NAMESPACE port-forward svc/$BROKER_RELEASE-demarkus-broker 8081:8081
  curl http://localhost:8081/.well-known/oauth-protected-resource

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
