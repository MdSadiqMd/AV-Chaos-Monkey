#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

cd "$PROJECT_ROOT"

echo "Cleaning up WebRTC connectors..."
kubectl delete deployment webrtc-connector --ignore-not-found=true 2>/dev/null || true

echo "Cleaning up TURN servers..."
kubectl delete statefulset coturn --ignore-not-found=true 2>/dev/null || true
kubectl delete service coturn coturn-lb --ignore-not-found=true 2>/dev/null || true

echo "Running main cleanup..."
go run ./tools/k8s-start cleanup "$@"
