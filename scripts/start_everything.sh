#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

cd "$PROJECT_ROOT"
COMMAND="${1:-run}"

UDP_TARGET_HOST="${UDP_TARGET_HOST:-}"  # Empty = use in-cluster udp-receiver
UDP_TARGET_PORT="${UDP_TARGET_PORT:-5000}"  # In-cluster UDP receiver

case "$COMMAND" in
  run|"")
    # run and deploy to Kubernetes
    go run ./tools/k8s-start \
      --udp-target-host="$UDP_TARGET_HOST" \
      --udp-target-port="$UDP_TARGET_PORT" \
      "${@:2}"
    ;;
  build)
    # build and start everything with Docker Compose
    go run ./tools/start "${@:2}"
    ;;
esac
