#!/usr/bin/env bash
# test-upgrade-wipe.sh — verify a chart's resource-policy: keep + lookup-skip
# pattern preserves out-of-band Secret mutations across helm upgrade and
# helm uninstall.
#
# This is the kind-side end-to-end regression test for the property the
# §6.1 and §6.3 charts both claim: that broker-minted token entries
# appended to a chart-managed Secret survive `helm upgrade <release>`
# (re-render must not overwrite live Secret content) and `helm uninstall
# <release>` (Secret must persist so a subsequent reinstall picks up the
# existing state).
#
# Usage:
#   test-upgrade-wipe.sh <chart_path> <release_name> <namespace> \
#                       <secret_name> <seed_key> <seed_value_b64> \
#                       [helm-args...]
#
# The seeded data simulates a broker mint: kubectl-patch adds a fake key
# into the Secret's data map after install, then upgrade + uninstall must
# leave that key intact.

set -euo pipefail

if [[ $# -lt 6 ]]; then
  echo "usage: $0 <chart> <release> <namespace> <secret> <seed_key> <seed_val_b64> [helm-args...]" >&2
  exit 2
fi

CHART="$1"
RELEASE="$2"
NAMESPACE="$3"
SECRET="$4"
SEED_KEY="$5"
SEED_VALUE_B64="$6"
shift 6
HELM_ARGS=("$@")

echo "::group::test-upgrade-wipe: chart=$CHART release=$RELEASE namespace=$NAMESPACE secret=$SECRET"

cleanup() {
  set +e
  helm uninstall "$RELEASE" --namespace "$NAMESPACE" --wait 2>/dev/null
  kubectl -n "$NAMESPACE" delete secret "$SECRET" --ignore-not-found 2>/dev/null
  kubectl delete namespace "$NAMESPACE" --ignore-not-found 2>/dev/null
}
trap cleanup EXIT

kubectl create namespace "$NAMESPACE" --dry-run=client -o yaml | kubectl apply -f -

echo "--- helm install"
helm install "$RELEASE" "$CHART" --namespace "$NAMESPACE" --wait=false "${HELM_ARGS[@]}"

echo "--- wait for Secret $SECRET"
for _ in $(seq 1 60); do
  if kubectl -n "$NAMESPACE" get secret "$SECRET" >/dev/null 2>&1; then
    break
  fi
  sleep 1
done
if ! kubectl -n "$NAMESPACE" get secret "$SECRET" >/dev/null 2>&1; then
  echo "FAIL: Secret $SECRET not created within 60s of install"
  kubectl -n "$NAMESPACE" get secret
  exit 1
fi

echo "--- assert resource-policy=keep annotation present (post-install)"
policy=$(kubectl -n "$NAMESPACE" get secret "$SECRET" -o jsonpath='{.metadata.annotations.helm\.sh/resource-policy}')
if [[ "$policy" != "keep" ]]; then
  echo "FAIL: $SECRET annotation helm.sh/resource-policy=$policy, want 'keep'"
  exit 1
fi
echo "OK: keep annotation present"

echo "--- seed broker-minted data ($SEED_KEY=<redacted>)"
# Strategic-merge patch: adds the key without touching other entries, and
# handles the case where data is absent (empty {} in YAML is stored as nil,
# which breaks JSON-patch `add /data/X` but merge-patch creates the map).
kubectl -n "$NAMESPACE" patch secret "$SECRET" --type=merge \
  -p "{\"data\":{\"${SEED_KEY}\":\"${SEED_VALUE_B64}\"}}"

echo "--- helm upgrade (re-renders templates; lookup-skip + keep should preserve Secret)"
helm upgrade "$RELEASE" "$CHART" --namespace "$NAMESPACE" --wait=false "${HELM_ARGS[@]}"

echo "--- assert seeded data persists after upgrade"
got=$(kubectl -n "$NAMESPACE" get secret "$SECRET" -o jsonpath="{.data.${SEED_KEY}}")
if [[ "$got" != "$SEED_VALUE_B64" ]]; then
  echo "FAIL: seeded ${SEED_KEY} missing or modified after upgrade"
  echo "  got:  ${got}"
  echo "  want: ${SEED_VALUE_B64}"
  exit 1
fi
echo "OK: seeded data survived upgrade"

echo "--- helm uninstall"
helm uninstall "$RELEASE" --namespace "$NAMESPACE" --wait

echo "--- assert Secret survives uninstall (resource-policy: keep)"
if ! kubectl -n "$NAMESPACE" get secret "$SECRET" >/dev/null 2>&1; then
  echo "FAIL: $SECRET deleted on uninstall — resource-policy=keep did not hold"
  exit 1
fi
got=$(kubectl -n "$NAMESPACE" get secret "$SECRET" -o jsonpath="{.data.${SEED_KEY}}")
if [[ "$got" != "$SEED_VALUE_B64" ]]; then
  echo "FAIL: seeded ${SEED_KEY} lost after uninstall"
  exit 1
fi
echo "OK: Secret + seeded data survived uninstall"

echo "::endgroup::"
echo "PASS: $CHART upgrade-wipe regression"
