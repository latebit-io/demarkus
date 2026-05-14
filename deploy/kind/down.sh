#!/usr/bin/env bash
set -euo pipefail

CLUSTER="${CLUSTER:-knowledge-system}"

if kind get clusters | grep -qx "$CLUSTER"; then
  kind delete cluster --name "$CLUSTER"
else
  echo "kind cluster '$CLUSTER' not found, nothing to do"
fi
