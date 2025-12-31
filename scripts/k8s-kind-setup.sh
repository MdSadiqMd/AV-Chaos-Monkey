#!/usr/bin/env bash
# Auto-setup Kubernetes cluster with kind

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CLUSTER_NAME="${CLUSTER_NAME:-av-chaos-monkey}"

# Find kind and kubectl (check Nix path first, then system)
KIND_CMD=$(command -v kind || which kind || echo "kind")
KUBECTL_CMD=$(command -v kubectl || which kubectl || echo "kubectl")

if ! command -v "$KIND_CMD" &> /dev/null; then
    echo "❌ kind not found. Run: nix develop"
    exit 1
fi

if ! command -v "$KUBECTL_CMD" &> /dev/null; then
    echo "❌ kubectl not found. Run: nix develop"
    exit 1
fi

# Ensure Docker is running
"$SCRIPT_DIR/ensure-docker.sh" || exit 1

# Check if cluster exists
if "$KIND_CMD" get clusters 2>/dev/null | grep -q "^${CLUSTER_NAME}$"; then
    echo "✓ Cluster '${CLUSTER_NAME}' already exists"
    "$KUBECTL_CMD" config use-context "kind-${CLUSTER_NAME}" 2>/dev/null || true
    exit 0
fi

# Create cluster
echo "Creating Kubernetes cluster..."
"$KIND_CMD" create cluster --name "${CLUSTER_NAME}" --wait 5m

# Set context
"$KUBECTL_CMD" config use-context "kind-${CLUSTER_NAME}"

echo "✓ Cluster ready: kind-${CLUSTER_NAME}"
