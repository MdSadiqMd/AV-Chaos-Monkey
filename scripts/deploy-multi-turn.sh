#!/bin/bash
# Deploy and scale TURN servers for high-scale WebRTC testing
# This distributes ICE candidate gathering load across multiple coturn instances
# Usage: ./deploy-multi-turn.sh [replicas] [participants]
#   replicas: Number of TURN server replicas (default: auto-calculated)
#   participants: Expected number of participants (used for auto-calculation)
set -e

PARTICIPANTS="${2:-1500}"
# Auto-calculate replicas: ~500 participants per TURN server
AUTO_REPLICAS=$(( (PARTICIPANTS + 499) / 500 ))
REPLICAS="${1:-$AUTO_REPLICAS}"
NAMESPACE="${NAMESPACE:-default}"

if [ "$REPLICAS" -lt 1 ]; then REPLICAS=1; fi
if [ "$REPLICAS" -gt 10 ]; then REPLICAS=10; fi

echo "Deploying $REPLICAS TURN servers for ~$PARTICIPANTS participants..."

# Delete old single-replica deployment if exists
kubectl delete deployment coturn --ignore-not-found=true 2>/dev/null || true

# Apply coturn StatefulSet (includes HPA for auto-scaling)
kubectl apply -f k8s/coturn/coturn.yaml

# Scale to desired replicas
kubectl scale statefulset coturn --replicas=$REPLICAS

echo "Waiting for TURN servers to be ready..."
kubectl rollout status statefulset/coturn --timeout=180s

# Update orchestrator with new TURN_REPLICAS value
echo "Updating orchestrator TURN_REPLICAS to $REPLICAS..."
kubectl set env statefulset/orchestrator TURN_REPLICAS="$REPLICAS" 2>/dev/null || true

echo ""
echo "✅ TURN servers deployed!"
kubectl get pods -l app=coturn
echo ""
echo "Verifying TURN server DNS..."
for i in $(seq 0 $((REPLICAS-1))); do
    echo "  coturn-$i.coturn -> $(kubectl exec orchestrator-0 -- nslookup coturn-$i.coturn 2>/dev/null | grep Address | tail -1 || echo 'pending...')"
done
echo ""
echo "TURN server distribution (~500 participants per server):"
for i in $(seq 0 $((REPLICAS-1))); do
    echo "  - Participants $i, $((i+REPLICAS)), $((i+2*REPLICAS))... → coturn-$i"
done
echo ""
echo "To manually scale:"
echo "  kubectl scale statefulset coturn --replicas=N"
echo "  kubectl set env statefulset/orchestrator TURN_REPLICAS=N"
