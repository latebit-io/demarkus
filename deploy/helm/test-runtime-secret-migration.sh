#!/usr/bin/env bash
# test-runtime-secret-migration.sh — verify that upgrading from a chart that
# templated a runtime Secret (resource-policy: keep) to one that no longer
# renders it leaves the live Secret and its broker-written data in place,
# through upgrade and uninstall.
#
# Usage:
#   test-runtime-secret-migration.sh <old_chart_dir> <new_chart_dir> <release> \
#       <namespace> <secret_name> <seed_key> <seed_value_b64> [helm-args...]

set -euo pipefail

if [[ $# -lt 7 ]]; then
  echo "usage: $0 <old-chart> <new-chart> <release> <namespace> <secret> <seed_key> <seed_val_b64> [helm-args...]" >&2
  exit 2
fi

OLD_CHART="$1"
NEW_CHART="$2"
RELEASE="$3"
NAMESPACE="$4"
SECRET="$5"
SEED_KEY="$6"
SEED_VALUE_B64="$7"
shift 7
HELM_ARGS=("$@")

echo "::group::test-runtime-secret-migration: release=$RELEASE namespace=$NAMESPACE secret=$SECRET"

cleanup() {
  set +e
  helm uninstall "$RELEASE" --namespace "$NAMESPACE" --wait 2>/dev/null
  kubectl -n "$NAMESPACE" delete secret "$SECRET" --ignore-not-found 2>/dev/null
  kubectl delete namespace "$NAMESPACE" --ignore-not-found 2>/dev/null
}
trap cleanup EXIT

kubectl create namespace "$NAMESPACE" --dry-run=client -o yaml | kubectl apply -f -

echo "--- helm install (old chart, templates $SECRET)"
helm install "$RELEASE" "$OLD_CHART" --namespace "$NAMESPACE" --wait=false "${HELM_ARGS[@]}"
for _ in $(seq 1 60); do
  kubectl -n "$NAMESPACE" get secret "$SECRET" >/dev/null 2>&1 && break
  sleep 1
done
kubectl -n "$NAMESPACE" get secret "$SECRET" >/dev/null || { echo "FAIL: old chart did not create $SECRET"; exit 1; }

echo "--- seed broker-written data ($SEED_KEY=<redacted>)"
kubectl -n "$NAMESPACE" patch secret "$SECRET" --type=merge \
  -p "{\"data\":{\"${SEED_KEY}\":\"${SEED_VALUE_B64}\"}}"

echo "--- helm upgrade (new chart, no longer renders $SECRET)"
helm upgrade "$RELEASE" "$NEW_CHART" --namespace "$NAMESPACE" --wait=false "${HELM_ARGS[@]}"
if helm get manifest "$RELEASE" --namespace "$NAMESPACE" | grep -q "name: ${SECRET}\$"; then
  echo "FAIL: new chart still renders $SECRET"
  exit 1
fi

assert_seed() { # <phase>
  local got
  got=$(kubectl -n "$NAMESPACE" get secret "$SECRET" -o jsonpath="{.data['${SEED_KEY}']}" 2>/dev/null) || {
    echo "FAIL: $SECRET deleted after $1"
    exit 1
  }
  if [[ "$got" != "$SEED_VALUE_B64" ]]; then
    echo "FAIL: seeded ${SEED_KEY} missing or modified after $1"
    exit 1
  fi
  echo "OK: $SECRET and its data survived $1"
}
assert_seed upgrade

echo "--- helm uninstall"
helm uninstall "$RELEASE" --namespace "$NAMESPACE" --wait
assert_seed uninstall

echo "::endgroup::"
