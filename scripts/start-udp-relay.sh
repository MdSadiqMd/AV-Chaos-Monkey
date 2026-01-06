#!/bin/bash
# Start the UDP relay chain to receive packets from Kubernetes on my Mac
#
# This script starts:
# 1. kubectl port-forward for TCP connection to udp-relay pod
# 2. Local TCP-to-UDP relay
# 3. Local UDP receiver
#
# Usage: ./scripts/start-udp-relay.sh

set -e

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
NC='\033[0m'

echo -e "${CYAN}=== UDP Relay Chain ===${NC}"
echo ""

if ! kubectl get pod udp-relay &>/dev/null; then
    echo -e "${RED}❌ udp-relay pod not found${NC}"
    echo "Run the chaos test first: ./scripts/start_everything.sh run"
    exit 1
fi

echo -e "${YELLOW}Waiting for udp-relay pod to be ready...${NC}"
kubectl wait --for=condition=ready pod/udp-relay --timeout=60s

echo -e "${YELLOW}Cleaning up existing processes...${NC}"
pkill -9 -f 'kubectl port-forward.*udp-relay' 2>/dev/null || true
pkill -9 -f 'go run.*udp-relay' 2>/dev/null || true
pkill -9 -f 'go run.*udp_receiver' 2>/dev/null || true
sleep 1

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(dirname "$SCRIPT_DIR")"

echo ""
echo -e "${GREEN}Starting UDP relay chain...${NC}"
echo ""

# Start kubectl port-forward in background
echo -e "${CYAN}[1/3] Starting kubectl port-forward (TCP 15001 -> udp-relay:5001)${NC}"
kubectl port-forward udp-relay 15001:5001 &
PF_PID=$!
sleep 2

if ! kill -0 $PF_PID 2>/dev/null; then
    echo -e "${RED}❌ kubectl port-forward failed to start${NC}"
    exit 1
fi
echo -e "${GREEN}✓ Port-forward running (PID: $PF_PID)${NC}"

# Start local TCP-to-UDP relay in background
echo -e "${CYAN}[2/3] Starting TCP-to-UDP relay (TCP 15001 -> UDP localhost:5002)${NC}"
cd "$PROJECT_ROOT"
go run ./tools/udp-relay/main.go &
RELAY_PID=$!
sleep 2

if ! kill -0 $RELAY_PID 2>/dev/null; then
    echo -e "${RED}❌ TCP-to-UDP relay failed to start${NC}"
    kill $PF_PID 2>/dev/null
    exit 1
fi
echo -e "${GREEN}✓ TCP-to-UDP relay running (PID: $RELAY_PID)${NC}"

# Start UDP receiver in foreground
echo -e "${CYAN}[3/3] Starting UDP receiver on port 5002${NC}"
echo ""
echo -e "${GREEN}=== Ready to receive UDP packets ===${NC}"
echo -e "Press Ctrl+C to stop all processes"
echo ""

cleanup() {
    echo ""
    echo -e "${YELLOW}Stopping all processes...${NC}"
    kill $PF_PID 2>/dev/null || true
    kill $RELAY_PID 2>/dev/null || true
    pkill -9 -f 'go run.*udp_receiver' 2>/dev/null || true
    echo -e "${GREEN}Done${NC}"
}
trap cleanup EXIT

go run ./examples/go/udp_receiver.go 5002
