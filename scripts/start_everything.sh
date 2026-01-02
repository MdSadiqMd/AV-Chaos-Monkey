#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

cd "$PROJECT_ROOT"
COMMAND="${1:-k8s}"

case "$COMMAND" in
  run|"")
    # run and deploy to Kubernetes
    go run ./tools/k8s-start "${@:2}"
    ;;
  build)
    # build and start everything with Docker Compose
    go run ./tools/start "${@:2}"
    ;;
esac
