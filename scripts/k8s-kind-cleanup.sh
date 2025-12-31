#!/usr/bin/env bash
# Cleanup Kubernetes cluster

set -euo pipefail

CLUSTER_NAME="${CLUSTER_NAME:-av-chaos-monkey}"

if kind get clusters 2>/dev/null | grep -q "^${CLUSTER_NAME}$"; then
    kind delete cluster --name "${CLUSTER_NAME}"
    kubectl config delete-context "kind-${CLUSTER_NAME}" 2>/dev/null || true
    echo "✓ Cluster deleted"
else
    echo "Cluster '${CLUSTER_NAME}' does not exist"
fi
