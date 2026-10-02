#!/usr/bin/env bash
# kind harness for demarkus development.
#
# Stage 1 (default):            kind cluster + demarkus-server chart.
# Stage 2 (--with-knowledge):   + fake-gcs-server + mock-oauth2-server + the
#                               local demarkus-knowledge-server chart with an
#                               image built from this checkout: the multi-world
#                               server and the broker in one process.
# Stage 3 (--with-argo):        + Argo CD + ApplicationSet templating two worlds
#                               (replaces the Stage 1 server install).
#
# --with-mcp-smoke (layered on Stage 2) drives, from curl pods, the RFC 9728
# and 8414 metadata checks, the RFC 6749 authorization_code + PKCE flow that
# backs /knowledge-join (with the wrong-verifier and replay refusals), the
# MCP tool calls an agent makes after joining (read, write under the identity
# grant, refusals) and the confidential web-client flow (the library's SSO).
#
# Verification runs `demarkus` against each server's own QUIC listener via
# `kubectl exec`, which exercises the full read path without exposing
# anything to the host.

set -euo pipefail

CLUSTER="${CLUSTER:-knowledge-system}"
NAMESPACE="${NAMESPACE:-demarkus}"
RELEASE="${RELEASE:-world-default}"
SERVER_CHART_VERSION="${SERVER_CHART_VERSION:-0.50.0}"
SERVER_CHART="oci://ghcr.io/latebit-io/charts/demarkus-server"
KNOWLEDGE_RELEASE="${KNOWLEDGE_RELEASE:-knowledge}"
KNOWLEDGE_IMAGE="${KNOWLEDGE_IMAGE:-}"
# Ephemeral curl pod image used by the smoke flows. Pinned so behavior is
# reproducible across hosts; busybox sh + curl is all we need.
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
# Federation for the knowledge stage: the hub is the world values-knowledge.yaml
# marks hub; the cadence is short so the smoke waits seconds (defaults 30s, 1m).
FEDERATION_HUB=root
FEDERATION_QUIET_SECONDS=5
FEDERATION_INTERVAL_SECONDS=10

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" &>/dev/null && pwd)"
REPO_ROOT="$(cd -- "$SCRIPT_DIR/../.." &>/dev/null && pwd)"
VALUES_FILE="$SCRIPT_DIR/values-kind.yaml"
KNOWLEDGE_VALUES_FILE="$SCRIPT_DIR/values-knowledge.yaml"
KNOWLEDGE_CHART="$REPO_ROOT/deploy/helm/demarkus-knowledge-server"
ARGO_VALUES_FILE="$SCRIPT_DIR/values-argo.yaml"
MOCK_OIDC_MANIFEST="$SCRIPT_DIR/mock-oidc.yaml"
FAKE_GCS_MANIFEST="$SCRIPT_DIR/fake-gcs.yaml"
KIND_CONFIG="$SCRIPT_DIR/kind-config.yaml"
APPLICATIONSET_MANIFEST="$REPO_ROOT/deploy/k8s/examples/applicationset.yaml"

WITH_KNOWLEDGE=false
WITH_ARGO=false
WITH_MCP_SMOKE=false
for arg in "$@"; do
  case "$arg" in
    --with-knowledge) WITH_KNOWLEDGE=true ;;
    --with-argo)      WITH_ARGO=true ;;
    --with-mcp-smoke) WITH_MCP_SMOKE=true ;;
    -h|--help)
      cat <<EOF
usage: up.sh [--with-knowledge] [--with-argo] [--with-mcp-smoke]

  --with-knowledge  build the demarkus-knowledge image from this checkout,
                    sideload it into kind, and install fake-gcs-server,
                    mock-oauth2-server and the LOCAL demarkus-knowledge-server
                    chart (deploy/helm/demarkus-knowledge-server) with it
  --with-argo       install Argo CD + ApplicationSet templating two worlds
                    (replaces the Stage 1 server install)
  --with-mcp-smoke  drive the OAuth metadata checks, the authorization_code +
                    PKCE flow behind /knowledge-join, the MCP tool calls and
                    the confidential web-client flow against the knowledge
                    Service. Requires --with-knowledge.

env overrides:
  CLUSTER, NAMESPACE, RELEASE, SERVER_CHART_VERSION, KNOWLEDGE_RELEASE,
  KNOWLEDGE_IMAGE (repository:tag to run instead of building),
  ARGO_RELEASE, ARGO_CHART_VERSION, ARGO_REPO_URL, MINT_CURL_IMAGE
EOF
      exit 0
      ;;
    *) echo "unknown arg: $arg" >&2; exit 2 ;;
  esac
done

if [[ "$WITH_MCP_SMOKE" == "true" && "$WITH_KNOWLEDGE" != "true" ]]; then
  echo "--with-mcp-smoke requires --with-knowledge" >&2
  exit 2
fi

require() {
  command -v "$1" >/dev/null 2>&1 || {
    echo "missing required tool: $1" >&2
    exit 1
  }
}

# pod_of <namespace> <label selector> prints the first matching pod or fails.
pod_of() {
  local pod
  pod=$(kubectl -n "$1" get pods -l "$2" -o jsonpath='{.items[0].metadata.name}')
  if [[ -z "$pod" ]]; then
    echo "no pod found in namespace $1 for selector: $2" >&2
    return 1
  fi
  printf '%s\n' "$pod"
}

require kind
require helm
require kubectl
require openssl
if [[ "$WITH_KNOWLEDGE" == "true" ]]; then
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

# ensure_knowledge_tls self-signs a certificate for the kind worlds'
# authorities into the knowledge-tls Secret the chart mounts. Nothing verifies
# it in kind: the gateway reaches the world in process and the smoke tests
# dial with -insecure.
ensure_knowledge_tls() {
  local ns="$1" tmpdir sans
  shift
  echo "--- generating a self-signed QUIC certificate for $*"
  sans=$(printf 'DNS:%s,' "$@")
  tmpdir=$(mktemp -d)
  trap 'rm -rf "$tmpdir"' RETURN
  openssl req -x509 -newkey rsa:2048 -nodes -days 7 -subj "/CN=$1" \
    -addext "subjectAltName=${sans%,}" \
    -keyout "$tmpdir/tls.key" -out "$tmpdir/tls.crt" 2>/dev/null
  kubectl -n "$ns" create secret tls knowledge-tls \
    --cert="$tmpdir/tls.crt" --key="$tmpdir/tls.key" \
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
    WORLD_POD=$(pod_of "$world" "app.kubernetes.io/instance=$world,app.kubernetes.io/name=demarkus-server")
    kubectl -n "$world" exec "$WORLD_POD" -- \
      /demarkus ping -insecure "mark://localhost:6309"
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

POD=$(pod_of "$NAMESPACE" "app.kubernetes.io/instance=$RELEASE,app.kubernetes.io/name=demarkus-server")
echo "--- waiting for pod $POD"
kubectl -n "$NAMESPACE" wait --for=condition=ready "pod/$POD" --timeout=120s

echo "--- smoke test: ping the server from inside the pod"
kubectl -n "$NAMESPACE" exec "$POD" -- \
  /demarkus ping -insecure "mark://localhost:6309"

# The chart emits two Secrets when emitRawValues=true (default):
#   <release>-demarkus-server-tokens         server-mounted, hash-only TOML
#   <release>-demarkus-server-token-values   raw admin token
# We deliberately do not print the token here — a kind dev cluster is
# ephemeral and the user can pull it on demand with the command shown below.
TOKEN_SECRET="${RELEASE}-demarkus-server-token-values"

if [[ "$WITH_KNOWLEDGE" == "true" ]]; then
  echo "--- applying fake-gcs-server (the knowledge worlds' buckets)"
  kubectl -n "$NAMESPACE" apply -f "$FAKE_GCS_MANIFEST"
  kubectl -n "$NAMESPACE" rollout status deployment/fake-gcs-server --timeout=120s
  # The chart never creates buckets: the world's and the broker's state.
  # A reused cluster already has them: 409 is fine, anything else fails.
  kubectl run -n "$NAMESPACE" bucket-setup --rm -i --restart=Never \
    --image="$MINT_CURL_IMAGE" --command -- sh -c '
for bucket in kind-world-default kind-'"$FEDERATION_HUB"' kind-broker-state; do
  code=$(curl -sS -o /dev/null -w "%{http_code}" -X POST -H "Content-Type: application/json" \
    -d "{\"name\":\"$bucket\"}" "http://fake-gcs-server.'"$NAMESPACE"'.svc.cluster.local:4443/storage/v1/b")
  case $code in 200|409) ;; *) echo "bucket create $bucket: HTTP $code"; exit 1 ;; esac
done'

  echo "--- applying mock-oauth2-server (OIDC issuer for broker discovery)"
  kubectl -n "$NAMESPACE" apply -f "$MOCK_OIDC_MANIFEST"
  kubectl -n "$NAMESPACE" rollout status deployment/mock-oauth2-server --timeout=120s

  ensure_broker_signing_key "$NAMESPACE"
  ensure_knowledge_tls "$NAMESPACE" \
    "world-default.$KNOWLEDGE_RELEASE.$NAMESPACE.svc.cluster.local" \
    "$FEDERATION_HUB.$KNOWLEDGE_RELEASE.$NAMESPACE.svc.cluster.local"

  # KNOWLEDGE_IMAGE=<repository>:<tag> runs a published or already loaded
  # image; otherwise the composed image is built from this checkout so the
  # chart wiring under test is this branch's, tagged fresh so reruns rebuild.
  if [[ -z "$KNOWLEDGE_IMAGE" ]]; then
    KNOWLEDGE_IMAGE="demarkus-kind/demarkus-knowledge:local-$(date +%s)"
    echo "--- building demarkus-knowledge image $KNOWLEDGE_IMAGE"
    ( cd "$REPO_ROOT" && \
        make image-knowledge "IMAGE_REGISTRY=${KNOWLEDGE_IMAGE%%/*}" "TAG=${KNOWLEDGE_IMAGE##*:}" )
    echo "--- sideloading image into kind cluster $CLUSTER"
    kind load docker-image "$KNOWLEDGE_IMAGE" --name "$CLUSTER"
  fi

  # Confidential web client for the web-SSO smoke below. The secret is
  # generated per run and only its sha256 enters the rendered config, the
  # same posture as a real deployment. The redirect host is .invalid
  # (RFC 2606): nothing ever connects to it, the smoke only parses the
  # broker's 302 Location to recover the authorization code.
  WEBCLIENT_ID="library-web-smoke"
  WEBCLIENT_SECRET="$(openssl rand -hex 24)"
  WEBCLIENT_SECRET_HASH="$(printf '%s' "$WEBCLIENT_SECRET" | openssl dgst -sha256 -hex | awk '{print $NF}')"
  WEBCLIENT_REDIRECT_URI="https://library.smoke.invalid/auth/callback"

  echo "--- installing LOCAL demarkus-knowledge-server chart from $KNOWLEDGE_CHART"
  # helm --wait blocks on the broker's /readyz, which only flips green after
  # OIDC discovery against the mock issuer and every world open in
  # fake-gcs-server succeeded, so a successful install already proves both.
  helm upgrade --install "$KNOWLEDGE_RELEASE" "$KNOWLEDGE_CHART" \
    --namespace "$NAMESPACE" --create-namespace \
    --values "$KNOWLEDGE_VALUES_FILE" \
    --set "image.repository=${KNOWLEDGE_IMAGE%:*}" \
    --set "image.tag=${KNOWLEDGE_IMAGE##*:}" \
    --set "image.pullPolicy=IfNotPresent" \
    --set "broker.webClients[0].clientID=$WEBCLIENT_ID" \
    --set-string "broker.webClients[0].clientSecretHash=$WEBCLIENT_SECRET_HASH" \
    --set "broker.webClients[0].redirectURIs[0]=$WEBCLIENT_REDIRECT_URI" \
    --set "broker.webClients[0].name=Web SSO smoke client" \
    --set "broker.federation.quietPeriod=${FEDERATION_QUIET_SECONDS}s" \
    --set "broker.federation.interval=${FEDERATION_INTERVAL_SECONDS}s" \
    --wait --timeout 5m

  KNOWLEDGE_POD=$(pod_of "$NAMESPACE" "app.kubernetes.io/instance=$KNOWLEDGE_RELEASE,app.kubernetes.io/name=demarkus-knowledge-server")
  KNOWLEDGE_HTTP_URL="http://$KNOWLEDGE_RELEASE.$NAMESPACE.svc.cluster.local:8080"

  if [[ "$WITH_MCP_SMOKE" == "true" ]]; then
    echo "--- driving MCP gateway smoke checks from an ephemeral curl pod"
    # One listener serves the management API and the gateway; in kind no
    # hostname tells them apart, so both URLs are the knowledge Service.
    BROKER_MCP_URL="$KNOWLEDGE_HTTP_URL"
    BROKER_MGMT_URL="$KNOWLEDGE_HTTP_URL"
    kubectl run -n "$NAMESPACE" mcp-smoke --rm -i --restart=Never \
      --image="$MINT_CURL_IMAGE" --command -- sh -c '
set -eu
BROKER_MCP='"$BROKER_MCP_URL"'
BROKER_MGMT='"$BROKER_MGMT_URL"'
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

# 2. RFC 8414 metadata on the issuer origin (the same mux in kind).
META=$($CURL "$BROKER_MGMT/.well-known/oauth-authorization-server")
echo "$META" | grep -q "issuer" || { echo "FAIL: oauth-authorization-server missing issuer field on management host"; echo "$META"; exit 1; }
echo "OK: management /.well-known/oauth-authorization-server"

# 2b. RFC 9728 s3.1 path-inserted form for resource <host>/mcp.
META=$($CURL "$BROKER_MCP/.well-known/oauth-protected-resource/mcp")
echo "$META" | grep -q "resource" || { echo "FAIL: path-inserted oauth-protected-resource/mcp missing resource field"; echo "$META"; exit 1; }
echo "OK: /.well-known/oauth-protected-resource/mcp"

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

    # The tool calls below write here; the federation check reads its
    # checkpoint in the hub.
    MCP_SMOKE_WORLD="world-default"
    echo "--- driving auth-code + PKCE flow, then MCP tool calls, from an ephemeral curl pod"
    BROKER_OAUTH_URL="$KNOWLEDGE_HTTP_URL"
    kubectl run -n "$NAMESPACE" authcode-smoke --rm -i --restart=Never \
      --image="$MINT_CURL_IMAGE" --command -- sh -c '
set -eu
BROKER='"$BROKER_OAUTH_URL"'
BROKER_MCP='"$BROKER_MCP_URL"'
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
grep -Eq "\"access_token\":\"[^\"]+\"" /tmp/tok.json || { echo "FAIL: access_token missing/empty in token response"; cat /tmp/tok.json; exit 1; }
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

# 6b. A public login rotates its refresh token on every refresh, through the
#     Service so the replicas share the login. The token two rotations back
#     is reuse: it revokes the login, so the newest token stops working too.
FIRST=$(sed -n "s/.*\"refresh_token\":\"\([^\"]*\)\".*/\1/p" /tmp/tok.json)
[ -n "$FIRST" ] || { echo "FAIL: refresh_token missing from the auth-code exchange"; exit 1; }
refresh_public() {
  $CURL -o /tmp/pref.json -w "%{http_code}" -X POST "$BROKER/device/token" \
    --data-urlencode "grant_type=refresh_token" --data-urlencode "refresh_token=$1"
}
HTTP=$(refresh_public "$FIRST")
SECOND=$(sed -n "s/.*\"refresh_token\":\"\([^\"]*\)\".*/\1/p" /tmp/pref.json)
[ "$HTTP" = "200" ] && [ -n "$SECOND" ] && [ "$SECOND" != "$FIRST" ] || { echo "FAIL: refresh did not rotate (got $HTTP)"; cat /tmp/pref.json; exit 1; }
HTTP=$(refresh_public "$SECOND")
THIRD=$(sed -n "s/.*\"refresh_token\":\"\([^\"]*\)\".*/\1/p" /tmp/pref.json)
[ "$HTTP" = "200" ] && [ -n "$THIRD" ] && [ "$THIRD" != "$SECOND" ] || { echo "FAIL: rotated token did not refresh (got $HTTP)"; cat /tmp/pref.json; exit 1; }
echo "OK: a public refresh token rotates"
for TOKEN in "$FIRST" "$THIRD"; do
  HTTP=$(refresh_public "$TOKEN")
  [ "$HTTP" = "400" ] && grep -q "invalid_grant" /tmp/pref.json || { echo "FAIL: reuse did not revoke the login (got $HTTP)"; cat /tmp/pref.json; exit 1; }
done
echo "OK: a reused refresh token revokes its login"

# 7. TOOL CALLS — the Bearer minted above drives the MCP gateway, so the
#    smoke covers what an agent does after joining: read, write, and the
#    refusals. The token is read from the file and never echoed.
TOKEN=$(sed -n "s/.*\"access_token\":\"\([^\"]*\)\".*/\1/p" /tmp/tok.json)
[ -n "$TOKEN" ] || { echo "FAIL: could not read the access token for tool calls"; exit 1; }
WORLD='"$MCP_SMOKE_WORLD"'
DOC="mark://$WORLD/smoke/tools-$CLIENT_STATE.md"
printf "Authorization: Bearer %s\nContent-Type: application/json\nAccept: application/json, text/event-stream\n" "$TOKEN" > /tmp/mcp.hdrs

# rpc posts the JSON-RPC request on stdin; the answer lands in /tmp/rpc.out
# (plain JSON or one SSE data line, both are grepped the same way).
rpc() {
  cat > /tmp/req.json
  $CURL -X POST "$BROKER_MCP/mcp" -H @/tmp/mcp.hdrs -D /tmp/rpc.h -o /tmp/rpc.out -d @/tmp/req.json
}
# tool <name> <arguments json>
tool() {
  RPC_ID=$((RPC_ID + 1))
  rpc <<EOF
{"jsonrpc":"2.0","id":$RPC_ID,"method":"tools/call","params":{"name":"$1","arguments":$2}}
EOF
}
# want <label> <pattern>: the last answer is a tool success holding pattern.
want() {
  if grep -q "\"isError\":true" /tmp/rpc.out || ! grep -q "$2" /tmp/rpc.out; then
    echo "FAIL: $1"; cat /tmp/rpc.out; exit 1
  fi
  echo "OK: $1"
}
# refused <label> <pattern>: the last answer is a tool error holding pattern.
refused() {
  if ! grep -q "\"isError\":true" /tmp/rpc.out || ! grep -q "$2" /tmp/rpc.out; then
    echo "FAIL: $1"; cat /tmp/rpc.out; exit 1
  fi
  echo "OK: $1"
}
RPC_ID=0

rpc <<EOF
{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"kind-smoke","version":"0"}}}
EOF
SESSION=$(awk "tolower(\$1)==\"mcp-session-id:\"{print \$2}" /tmp/rpc.h | tr -d "\r")
[ -n "$SESSION" ] || { echo "FAIL: initialize returned no Mcp-Session-Id"; cat /tmp/rpc.h /tmp/rpc.out; exit 1; }
printf "Mcp-Session-Id: %s\n" "$SESSION" >> /tmp/mcp.hdrs
rpc <<EOF
{"jsonrpc":"2.0","method":"notifications/initialized"}
EOF
echo "OK: MCP session initialized with the minted Bearer"

RPC_ID=$((RPC_ID + 1))
rpc <<EOF
{"jsonrpc":"2.0","id":$RPC_ID,"method":"tools/list","params":{}}
EOF
for name in mark_fetch mark_publish mark_worlds mark_lookup_all; do
  grep -q "\"name\":\"$name\"" /tmp/rpc.out || { echo "FAIL: tools/list is missing $name"; exit 1; }
done
echo "OK: tools/list names the read, write and directory tools"

tool mark_worlds "{}"
want "mark_worlds lists the configured world" "$WORLD"
# A fresh kind world has no agent manifest, so the world answers not-found:
# any protocol status proves the gateway reached the world in process.
tool mark_discover "{\"url\":\"mark://$WORLD/\"}"
want "mark_discover reaches the world" "status: "

# Write path: the identity grant reaches the world in process, so the
# first publish lands with no token minted or projected.
tool mark_publish "{\"url\":\"$DOC\",\"body\":\"# Smoke\\n\\nfirst line\\n\",\"expected_version\":0,\"metadata\":{\"tags\":\"smoke\"}}"
want "mark_publish creates a document under the identity grant" "version: 1"
tool mark_fetch "{\"url\":\"$DOC\"}"
want "mark_fetch returns what was published" "first line"
tool mark_append "{\"url\":\"$DOC\",\"body\":\"second line\\n\"}"
want "mark_append resolves the version itself" "version: 2"
tool mark_versions "{\"url\":\"$DOC\"}"
want "mark_versions shows both versions" "current: 2"
tool mark_list "{\"url\":\"mark://$WORLD/smoke/\"}"
want "mark_list shows the document" "tools-$CLIENT_STATE.md"
tool mark_explore "{\"url\":\"$DOC\"}"
want "mark_explore returns the card" "## Outline"
tool mark_lookup "{\"url\":\"mark://$WORLD/\",\"query\":\"smoke\"}"
want "mark_lookup answers from the catalog" "status: ok"

# Host case is not identity: the world is reached however its name is typed.
UPPER=$(printf "%s" "$WORLD" | tr "a-z" "A-Z")
tool mark_fetch "{\"url\":\"mark://$UPPER/smoke/tools-$CLIENT_STATE.md\",\"force\":true}"
want "a world name is case insensitive in a tool URL" "second line"

# A stale version offers a merge candidate and publishes nothing.
tool mark_publish "{\"url\":\"$DOC\",\"body\":\"# Smoke\\n\\nfirst line, edited\\n\",\"expected_version\":1}"
want "a stale publish returns a merge candidate" "status: merge-candidate"

# Refusals, in the words an agent sees.
tool mark_fetch "{\"url\":\"mark://no-such-world/x.md\"}"
refused "an unknown world is named as such" "unknown world"
tool mark_publish "{\"url\":\"$DOC\",\"body\":\"x\"}"
refused "a publish without a version is refused" "expected_version is required"
tool mark_publish "{\"url\":\"$DOC\",\"body\":\"x\",\"expected_version\":2,\"metadata\":\"tags: smoke\"}"
refused "metadata that is not an object is refused" "metadata must be an object"

tool mark_archive "{\"url\":\"$DOC\"}"
want "mark_archive retires the smoke document" "archived: true"
echo "OK: MCP tool calls end-to-end through the gateway"

# 8. FEDERATION: an edit reaches the checkpoint of its world in the hub
#    within quietPeriod + interval + two 1.5 s commits (shard, manifest),
#    plus 2 s of polling. The deriver holds no token.
HUB='"$FEDERATION_HUB"'
QUIET='"$FEDERATION_QUIET_SECONDS"'
INTERVAL='"$FEDERATION_INTERVAL_SECONDS"'
MANIFEST="mark://$HUB/graph/worlds/$WORLD/manifest.md"
# The first checkpoint follows the startup rebuild of every world.
for attempt in $(seq 1 60); do
  tool mark_fetch "{\"url\":\"$MANIFEST\",\"force\":true}"
  grep -q "Format: demarkus-graph-world/v1" /tmp/rpc.out && break
  sleep 2
done
want "the hub holds a checkpoint of $WORLD" "Format: demarkus-graph-world/v1"
FED_PATH="/smoke/federation-$CLIENT_STATE.md"
tool mark_publish "{\"url\":\"mark://$WORLD$FED_PATH\",\"body\":\"# Federation smoke\\n\\nSee [tools]($DOC).\\n\",\"expected_version\":0,\"metadata\":{\"tags\":\"smoke\"}}"
want "mark_publish creates the federation smoke document" "version: 1"
START=$(date +%s)
BOUND=$((QUIET + INTERVAL + 5))
# Polls the manifest, which is written last and names the shard (its prefix
# grows with the world); the shard is read only when its pin moves.
SEEN=""
while :; do
  tool mark_fetch "{\"url\":\"$MANIFEST\",\"force\":true}"
  LEN=$(grep -o "Prefix-Length: [0-9]*" /tmp/rpc.out | cut -d" " -f2)
  SHARD="/graph/worlds/$WORLD/$(printf "%s" "$FED_PATH" | sha256sum | cut -c1-"${LEN:-1}").md"
  PIN=$(grep -o "| $SHARD | [0-9]* |" /tmp/rpc.out || true)
  if [ -n "$PIN" ] && [ "$PIN" != "$SEEN" ]; then
    SEEN=$PIN
    tool mark_fetch "{\"url\":\"mark://$HUB$SHARD\",\"force\":true}"
    VERSION=$(grep -o "version: [0-9]*" /tmp/rpc.out | head -1 | cut -d" " -f2)
    if [ "$PIN" = "| $SHARD | $VERSION |" ] && grep -q "$FED_PATH" /tmp/rpc.out && grep -q "$DOC" /tmp/rpc.out; then
      break
    fi
  fi
  if [ $(($(date +%s) - START)) -ge "$BOUND" ]; then
    echo "FAIL: the edit did not reach $MANIFEST within $BOUND s (last pin: [$SEEN])"; cat /tmp/rpc.out; exit 1
  fi
  sleep 1
done
echo "OK: the edit and its link reached the hub manifest in $(($(date +%s) - START)) s (bound $BOUND s)"
# The export follows its checkpoint: at most one interval more, plus its commit.
EXPORT_BOUND=$((BOUND + INTERVAL + 2))
until tool mark_fetch "{\"url\":\"mark://$HUB/graph.md\",\"force\":true}" && grep -q "mark://$WORLD$FED_PATH" /tmp/rpc.out; do
  if [ $(($(date +%s) - START)) -ge "$EXPORT_BOUND" ]; then
    echo "FAIL: the edit did not reach mark://$HUB/graph.md within $EXPORT_BOUND s"; cat /tmp/rpc.out; exit 1
  fi
  sleep 1
done
echo "OK: the edit reached the hub /graph.md in $(($(date +%s) - START)) s (bound $EXPORT_BOUND s)"
tool mark_archive "{\"url\":\"mark://$WORLD$FED_PATH\"}"
want "mark_archive retires the federation smoke document" "archived: true"
'
    echo "--- auth-code + PKCE flow and MCP tool calls passed"

    # The federation step above ran with no token: none of the Secrets the
    # chart or broker would hold a hub token in may exist.
    HUB_TOKEN_SECRETS=$(kubectl -n "$NAMESPACE" get secret --ignore-not-found -o name \
      "$FEDERATION_HUB-token-values" "$FEDERATION_HUB-tokens" "$FEDERATION_HUB-static-tokens" \
      "demarkus-broker-agent-token-$FEDERATION_HUB")
    [[ -z "$HUB_TOKEN_SECRETS" ]] || { echo "FAIL: token Secrets for the hub: $HUB_TOKEN_SECRETS" >&2; exit 1; }
    echo "--- federation ran with no token Secret for the hub"

    # Confidential web-client flow (phase-1b web SSO). Proves the chart's
    # webClients rendering reached the broker AND the broker enforces the
    # confidential contract: registered https redirect accepted (loopback
    # path untouched — covered above), token exchange demands the client
    # secret without burning the code on a missing-auth attempt, and the
    # minted refresh token is bound to the client.
    WEB_CLIENT_STATE="$(openssl rand -hex 16)"
    WEB_VERIFIER="$(openssl rand -hex 32)"
    WEB_CHALLENGE="$(printf '%s' "$WEB_VERIFIER" \
      | openssl dgst -sha256 -binary | openssl base64 -A | tr '+/' '-_' | tr -d '=')"

    echo "--- driving confidential web-client flow from an ephemeral curl pod"
    kubectl run -n "$NAMESPACE" webclient-smoke --rm -i --restart=Never \
      --image="$MINT_CURL_IMAGE" --command -- sh -c '
set -eu
BROKER='"$BROKER_OAUTH_URL"'
CLIENT_ID='"$WEBCLIENT_ID"'
CLIENT_SECRET='"$WEBCLIENT_SECRET"'
REDIRECT_URI='"$WEBCLIENT_REDIRECT_URI"'
CLIENT_STATE='"$WEB_CLIENT_STATE"'
VERIFIER='"$WEB_VERIFIER"'
CHALLENGE='"$WEB_CHALLENGE"'
CURL="curl -sS --connect-timeout 5 --max-time 15"

# 0. Wait for the broker OAuth Service to answer.
for attempt in $(seq 1 15); do
  if $CURL -o /dev/null "$BROKER/healthz"; then break; fi
  sleep 2
done
$CURL -f -o /dev/null "$BROKER/healthz" || { echo "FAIL: broker OAuth surface unreachable"; exit 1; }

# 1. Discovery advertises the confidential auth methods.
DISC=$($CURL "$BROKER/.well-known/openid-configuration")
echo "$DISC" | grep -q "client_secret_basic" || { echo "FAIL: discovery missing client_secret_basic"; exit 1; }
echo "OK: discovery advertises client_secret_basic"

# 2. NEGATIVE — an unregistered https redirect for the registered client
#    must be refused BEFORE redirect trust (JSON 400, no Location).
HTTP=$($CURL -G -o /tmp/badredir.json -w "%{http_code}" \
  --data-urlencode "response_type=code" \
  --data-urlencode "client_id=$CLIENT_ID" \
  --data-urlencode "redirect_uri=https://attacker.invalid/steal" \
  --data-urlencode "code_challenge=$CHALLENGE" \
  --data-urlencode "code_challenge_method=S256" \
  --data-urlencode "state=$CLIENT_STATE" \
  "$BROKER/oauth/authorize")
[ "$HTTP" = "400" ] || { echo "FAIL: unregistered redirect did not 400 (got $HTTP)"; cat /tmp/badredir.json; exit 1; }
echo "OK: unregistered redirect_uri rejected for registered client"

# 3. /oauth/authorize with the REGISTERED https redirect -> 302 to IdP.
$CURL -G -c /tmp/cookies -D /tmp/authz.h -o /dev/null \
  --data-urlencode "response_type=code" \
  --data-urlencode "client_id=$CLIENT_ID" \
  --data-urlencode "redirect_uri=$REDIRECT_URI" \
  --data-urlencode "code_challenge=$CHALLENGE" \
  --data-urlencode "code_challenge_method=S256" \
  --data-urlencode "state=$CLIENT_STATE" \
  "$BROKER/oauth/authorize"
IDP=$(awk "/^[Ll]ocation:/{print \$2}" /tmp/authz.h | tr -d "\r")
[ -n "$IDP" ] || { echo "FAIL: no Location from /oauth/authorize (web client)"; cat /tmp/authz.h; exit 1; }

# 4. Mock IdP auto-approves -> code+state for the broker callback.
$CURL -D /tmp/idp.h -o /dev/null "$IDP"
CB=$(awk "/^[Ll]ocation:/{print \$2}" /tmp/idp.h | tr -d "\r")
IDP_CODE=$(printf "%s" "$CB" | sed -n "s/.*[?&]code=\([^&]*\).*/\1/p")
IDP_STATE=$(printf "%s" "$CB" | sed -n "s/.*[?&]state=\([^&]*\).*/\1/p")
[ -n "$IDP_CODE" ] && [ -n "$IDP_STATE" ] || { echo "FAIL: bad IdP callback URL: $CB"; exit 1; }

# 5. /auth/callback -> 302 to the REGISTERED https redirect with the code.
$CURL -b /tmp/cookies -D /tmp/cb.h -o /dev/null "$BROKER/auth/callback?code=$IDP_CODE&state=$IDP_STATE"
RU=$(awk "/^[Ll]ocation:/{print \$2}" /tmp/cb.h | tr -d "\r")
case "$RU" in
  "$REDIRECT_URI"*) ;;
  *) echo "FAIL: callback did not redirect to registered URI: $RU"; exit 1 ;;
esac
case "$RU" in
  *"error="*) echo "FAIL: web-client callback returned an OAuth error: $RU"; exit 1 ;;
esac
AUTH_CODE=$(printf "%s" "$RU" | sed -n "s/.*[?&]code=\([^&]*\).*/\1/p")
RET_STATE=$(printf "%s" "$RU" | sed -n "s/.*[?&]state=\([^&]*\).*/\1/p")
[ -n "$AUTH_CODE" ] || { echo "FAIL: no broker authorization code in redirect: $RU"; exit 1; }
[ "$RET_STATE" = "$CLIENT_STATE" ] || { echo "FAIL: state mismatch: got [$RET_STATE] want [$CLIENT_STATE]"; exit 1; }
echo "OK: authorize -> callback -> code on registered https redirect"

# 6. NEGATIVE — exchange WITHOUT the client secret must 401
#    invalid_client with a Basic challenge, and must NOT burn the code.
HTTP=$($CURL -D /tmp/noauth.h -o /tmp/noauth.json -w "%{http_code}" -X POST "$BROKER/device/token" \
  --data-urlencode "grant_type=authorization_code" \
  --data-urlencode "code=$AUTH_CODE" \
  --data-urlencode "client_id=$CLIENT_ID" \
  --data-urlencode "redirect_uri=$REDIRECT_URI" \
  --data-urlencode "code_verifier=$VERIFIER")
[ "$HTTP" = "401" ] || { echo "FAIL: secretless exchange did not 401 (got $HTTP)"; cat /tmp/noauth.json; exit 1; }
grep -q "invalid_client" /tmp/noauth.json || { echo "FAIL: secretless exchange not invalid_client"; cat /tmp/noauth.json; exit 1; }
grep -qi "^WWW-Authenticate: Basic" /tmp/noauth.h || { echo "FAIL: 401 missing WWW-Authenticate: Basic"; cat /tmp/noauth.h; exit 1; }
echo "OK: token exchange without client secret rejected (invalid_client + Basic challenge)"

# 7. POSITIVE — exchange WITH HTTP Basic mints a Bearer + refresh token,
#    proving the failed attempt above preserved the one-shot code.
HTTP=$($CURL -u "$CLIENT_ID:$CLIENT_SECRET" -o /tmp/tok.json -w "%{http_code}" -X POST "$BROKER/device/token" \
  --data-urlencode "grant_type=authorization_code" \
  --data-urlencode "code=$AUTH_CODE" \
  --data-urlencode "client_id=$CLIENT_ID" \
  --data-urlencode "redirect_uri=$REDIRECT_URI" \
  --data-urlencode "code_verifier=$VERIFIER")
[ "$HTTP" = "200" ] || { echo "FAIL: Basic-auth exchange did not 200 (got $HTTP)"; cat /tmp/tok.json; exit 1; }
grep -q "\"token_type\":\"Bearer\"" /tmp/tok.json || { echo "FAIL: token_type Bearer missing"; exit 1; }
REFRESH=$(sed -n "s/.*\"refresh_token\":\"\([^\"]*\)\".*/\1/p" /tmp/tok.json)
[ -n "$REFRESH" ] || { echo "FAIL: refresh_token missing from web-client exchange"; exit 1; }
echo "OK: Basic-auth exchange minted a Bearer token (code survived the failed attempt)"

# 8. NEGATIVE — the refresh token is client-bound: refresh WITHOUT
#    client auth must 401 invalid_client.
HTTP=$($CURL -o /tmp/refnoauth.json -w "%{http_code}" -X POST "$BROKER/device/token" \
  --data-urlencode "grant_type=refresh_token" \
  --data-urlencode "refresh_token=$REFRESH")
[ "$HTTP" = "401" ] || { echo "FAIL: unauthenticated refresh of bound token did not 401 (got $HTTP)"; cat /tmp/refnoauth.json; exit 1; }
grep -q "invalid_client" /tmp/refnoauth.json || { echo "FAIL: unauthenticated refresh not invalid_client"; cat /tmp/refnoauth.json; exit 1; }
echo "OK: client-bound refresh token refuses to mint without client auth"

# 9. POSITIVE — refresh WITH Basic auth mints a fresh id_token.
HTTP=$($CURL -u "$CLIENT_ID:$CLIENT_SECRET" -o /tmp/ref.json -w "%{http_code}" -X POST "$BROKER/device/token" \
  --data-urlencode "grant_type=refresh_token" \
  --data-urlencode "refresh_token=$REFRESH")
[ "$HTTP" = "200" ] || { echo "FAIL: Basic-auth refresh did not 200 (got $HTTP)"; cat /tmp/ref.json; exit 1; }
grep -Eq "\"id_token\":\"[^\"]+\"" /tmp/ref.json || { echo "FAIL: id_token missing from authenticated refresh"; exit 1; }
echo "OK: bound refresh token mints with client auth"

# 10. A login bound to a web client keeps its token: the client secret
#     guards it, so the refresh hands the same one back.
NEXT=$(sed -n "s/.*\"refresh_token\":\"\([^\"]*\)\".*/\1/p" /tmp/ref.json)
[ "$NEXT" = "$REFRESH" ] || { echo "FAIL: a client-bound refresh token changed"; exit 1; }
echo "OK: a client-bound refresh token is not rotated"
echo "OK: confidential web-client flow end-to-end"
'
    echo "--- confidential web-client flow passed"
  fi

  cat <<EOF

ready (with knowledge).

cluster:           kind-$CLUSTER
namespace:         $NAMESPACE
server release:    $RELEASE
knowledge release: $KNOWLEDGE_RELEASE
knowledge pod:     $KNOWLEDGE_POD
server svc:        $RELEASE-demarkus-server.$NAMESPACE.svc.cluster.local:6309 (UDP)
knowledge svc:     $KNOWLEDGE_RELEASE.$NAMESPACE.svc.cluster.local:6309 (UDP), :8080 (HTTP: OAuth + MCP)
mock OIDC:         mock-oauth2-server.$NAMESPACE.svc.cluster.local:8080/default
fake GCS:          fake-gcs-server.$NAMESPACE.svc.cluster.local:4443

probe the broker and the MCP gateway (from another shell):
  kubectl -n $NAMESPACE port-forward svc/$KNOWLEDGE_RELEASE 8080:8080
  curl http://localhost:8080/healthz
  curl http://localhost:8080/.well-known/oauth-protected-resource

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

add the knowledge system (next stage):
  $0 --with-knowledge --with-mcp-smoke

tear down:
  $SCRIPT_DIR/down.sh
EOF
