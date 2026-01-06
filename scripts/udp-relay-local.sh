#!/usr/bin/env bash
# Local UDP relay - receives TCP from kubectl port-forward and converts to UDP
# Usage: ./scripts/udp-relay-local.sh [local_udp_port]
#
# This script:
# 1. Sets up kubectl port-forward for TCP port 5001 from udp-relay pod
# 2. Uses socat to convert incoming TCP to UDP
# 3. Forwards UDP packets to your local receiver

set -euo pipefail

LOCAL_UDP_PORT="${1:-5002}"
TCP_FORWARD_PORT="15001"

echo "=== UDP Relay Setup ==="
echo "This will forward UDP packets from Kubernetes to localhost:${LOCAL_UDP_PORT}"
echo ""

if ! command -v socat &> /dev/null; then
    echo "❌ socat is required but not installed."
    echo "Install with: brew install socat"
    exit 1
fi

# Kill any existing port-forwards and relays
pkill -f "kubectl port-forward.*udp-relay" 2>/dev/null || true
pkill -f "socat.*${TCP_FORWARD_PORT}" 2>/dev/null || true
sleep 1

if ! kubectl get pod udp-relay &>/dev/null; then
    echo "❌ udp-relay pod not found. Deploy it first:"
    echo "   kubectl apply -f k8s/udp-relay/udp-relay.yaml"
    exit 1
fi

echo "Waiting for udp-relay pod to be ready..."
kubectl wait --for=condition=ready pod/udp-relay --timeout=60s

echo "Starting kubectl port-forward (TCP ${TCP_FORWARD_PORT} -> udp-relay:5001)..."
kubectl port-forward pod/udp-relay ${TCP_FORWARD_PORT}:5001 &
PF_PID=$!
sleep 2

# Check if port-forward is running
if ! kill -0 $PF_PID 2>/dev/null; then
    echo "❌ Port-forward failed to start"
    exit 1
fi

# Start TCP to UDP relay
echo "Starting TCP to UDP relay (TCP ${TCP_FORWARD_PORT} -> UDP localhost:${LOCAL_UDP_PORT})..."
socat TCP4-LISTEN:${TCP_FORWARD_PORT},fork,reuseaddr UDP4:localhost:${LOCAL_UDP_PORT} &
SOCAT_PID=$!

echo ""
echo "✅ UDP relay is running!"
echo ""
echo "Packets flow: K8s pods -> udp-relay:5000 (UDP) -> :5001 (TCP) -> port-forward -> localhost:${TCP_FORWARD_PORT} (TCP) -> localhost:${LOCAL_UDP_PORT} (UDP)"
echo ""
echo "Now run your UDP receiver on port ${LOCAL_UDP_PORT}:"
echo "  cd examples/go && go run ./udp_receiver.go ${LOCAL_UDP_PORT}"
echo ""
echo "Press Ctrl+C to stop the relay"

cleanup() {
    echo ""
    echo "Stopping relay..."
    kill $PF_PID 2>/dev/null || true
    kill $SOCAT_PID 2>/dev/null || true
    pkill -f "kubectl port-forward.*udp-relay" 2>/dev/null || true
    pkill -f "socat.*${TCP_FORWARD_PORT}" 2>/dev/null || true
    echo "Done"
}
trap cleanup EXIT

# Wait for Ctrl+C
wait
