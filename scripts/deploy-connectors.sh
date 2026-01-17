#!/bin/bash
# Deploys WebRTC connector pods for high-scale client connections
# Connectors act as a proxy layer between clients and orchestrator pods
# This offloads connection handling from orchestrators
set -e

REPLICAS="${1:-3}"
PARTICIPANTS="${2:-1500}"

# Auto-calculate replicas: ~500 connections per connector
if [ -z "$1" ]; then
    REPLICAS=$(( (PARTICIPANTS + 499) / 500 ))
    if [ "$REPLICAS" -lt 2 ]; then REPLICAS=2; fi
    if [ "$REPLICAS" -gt 10 ]; then REPLICAS=10; fi
fi

echo "Deploying $REPLICAS WebRTC connector pods for ~$PARTICIPANTS participants..."

# Apply connector deployment (includes HPA)
kubectl apply -f k8s/connector/connector.yaml

# Scale to desired replicas
kubectl scale deployment webrtc-connector --replicas=$REPLICAS

echo "Waiting for connectors to be ready..."
kubectl rollout status deployment/webrtc-connector --timeout=120s

echo ""
echo "✅ WebRTC connectors deployed!"
kubectl get pods -l app=webrtc-connector
echo ""
echo "Connector configuration:"
echo "  - Replicas: $REPLICAS"
echo "  - Target participants: $PARTICIPANTS"
echo "  - Connections per connector: ~$((PARTICIPANTS / REPLICAS))"
echo ""
echo "To use connectors from the receiver:"
echo "  go run examples/go/webrtc_receiver.go http://localhost:8080 <test_id> $PARTICIPANTS"
echo ""
echo "To manually scale:"
echo "  kubectl scale deployment webrtc-connector --replicas=N"
