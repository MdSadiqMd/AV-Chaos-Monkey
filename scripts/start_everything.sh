#!/bin/bash
set -euo pipefail

NUM_PARTICIPANTS=250
TEST_DURATION_SECONDS=600

SKIP_TEST=false
AUTO_DETECT_PARTICIPANTS=true

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
CYAN='\033[0;36m'
MAGENTA='\033[0;35m'
BOLD='\033[1m'
NC='\033[0m'

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
cd "$PROJECT_ROOT"

log_info() { echo -e "${BLUE}[INFO]${NC} $1"; }
log_success() { echo -e "${GREEN}[✓]${NC} $1"; }
log_warning() { echo -e "${YELLOW}[⚠]${NC} $1"; }
log_error() { echo -e "${RED}[✗]${NC} $1"; }
log_config() { echo -e "${MAGENTA}[CONFIG]${NC} $1"; }

detect_docker_memory() {
    local docker_mem_gb=8
    local docker_mem=$(docker info 2>/dev/null | grep "Total Memory" | awk '{print $3}' | sed 's/GiB//')
    if [ -n "$docker_mem" ]; then
        docker_mem_gb="${docker_mem%.*}"
    fi
    echo "$docker_mem_gb"
}

calculate_safe_participants() {
    local docker_mem_gb=$1
    local safe_participants=10

    if [ "$docker_mem_gb" -ge 40 ]; then
        safe_participants=500
    elif [ "$docker_mem_gb" -ge 32 ]; then
        safe_participants=350
    elif [ "$docker_mem_gb" -ge 24 ]; then
        safe_participants=250
    elif [ "$docker_mem_gb" -ge 16 ]; then
        safe_participants=150
    elif [ "$docker_mem_gb" -ge 8 ]; then
        safe_participants=50
    elif [ "$docker_mem_gb" -ge 4 ]; then
        safe_participants=10
    fi

    echo "$safe_participants"
}

show_config() {
    local docker_mem=$(detect_docker_memory)
    local safe_count=$(calculate_safe_participants "$docker_mem")

    log_info "System Information:"
    echo "  System RAM: $(sysctl -n hw.memsize 2>/dev/null | awk '{printf "%.0f", $1/1024/1024/1024}')GB"
    echo "  Docker Memory: ${docker_mem}GB"
    echo "  Safe Participants for Docker: ${safe_count}"
    echo ""

    if [ "$AUTO_DETECT_PARTICIPANTS" = true ]; then
        NUM_PARTICIPANTS="$safe_count"
        log_info "Auto-detected participants: ${NUM_PARTICIPANTS}"
    fi

    if [ "$NUM_PARTICIPANTS" -gt "$safe_count" ]; then
        log_warning "Configured ${NUM_PARTICIPANTS} participants exceeds safe limit (${safe_count}) for ${docker_mem}GB Docker"
        log_warning "Consider: Docker Desktop → Settings → Resources → Memory → increase to 32GB+"
    fi

    log_config "Configuration:"
    echo "  Participants: ${BOLD}${NUM_PARTICIPANTS}${NC}"
    echo "  Duration: ${BOLD}${TEST_DURATION_SECONDS}s${NC}"
    echo "  Skip Test: ${BOLD}${SKIP_TEST}${NC}"
    echo ""
}

export_config() {
    export NUM_PARTICIPANTS
    export TEST_DURATION_SECONDS
    log_success "Exported NUM_PARTICIPANTS=${NUM_PARTICIPANTS}"
    log_success "Exported TEST_DURATION_SECONDS=${TEST_DURATION_SECONDS}"
}

show_config

log_info "Step 1: Checking Docker..."
if ! command -v docker >/dev/null 2>&1; then
    log_error "Docker is not installed!"
    exit 1
fi

docker_ready=false
if [ -S "$HOME/.docker/run/docker.sock" ] || [ -S "/var/run/docker.sock" ]; then
    if docker ps >/dev/null 2>&1; then
        docker_ready=true
    fi
fi

if [ "$docker_ready" = false ]; then
    log_warning "Docker daemon is not running. Starting Docker Desktop..."r "Failed to start Docker Desktop. Please start it manually."

    log_info "Waiting for Docker to fully start (this may take 30-60 seconds)..."
    max_wait=90
    waited=0
    while [ $waited -lt $max_wait ]; do
        if ([ -S "$HOME/.docker/run/docker.sock" ] || [ -S "/var/run/docker.sock" ]) && \
           docker ps >/dev/null 2>&1 && \
           docker info >/dev/null 2>&1; then
            log_success "Docker is fully ready!"
            docker_ready=true
            break
        fi
        waited=$((waited + 3))
        if [ $((waited % 9)) -eq 0 ]; then
            echo ""
            log_info "Still waiting... (${waited}s/${max_wait}s)"
        else
            echo -n "."
        fi
        sleep 3
    done
    echo ""

    if [ "$docker_ready" = false ]; then
        log_error "Docker failed to start after ${max_wait}s."
        log_error "Please start Docker Desktop manually and wait for it to fully load."
        exit 1
    fi
else
    log_success "Docker is already running"
fi

if ! docker ps >/dev/null 2>&1; then
    log_error "Docker command failed. Docker may not be fully ready."
    exit 1
fi

log_info "Step 2: Checking for port conflicts..."
port_conflict=false
conflicting_pids=""

while IFS= read -r line; do
    pid=$(echo "$line" | awk '{print $2}')
    cmd=$(echo "$line" | awk '{print $1}')
    if [[ "$cmd" != *"docke"* ]] && [[ "$cmd" != *"com.docke"* ]] && \
       [[ "$line" != *"chaos-monkey-orchestrator"* ]]; then
        port_conflict=true
        conflicting_pids="$conflicting_pids $pid"
    fi
done < <(lsof -i :8080 2>/dev/null | grep LISTEN || true)

if [ "$port_conflict" = true ] && [ -n "$conflicting_pids" ]; then
    log_warning "Port 8080 is in use by non-Docker process(es). Killing..."
    for pid in $conflicting_pids; do
        kill -9 "$pid" 2>/dev/null || true
    done
    sleep 2
    log_success "Port 8080 is now free"
else
    log_success "Port 8080 is available (or used by Docker)"
fi

log_info "Step 3: Building orchestrator with memory optimizations..."
cd "$PROJECT_ROOT"
docker-compose --profile monitoring down orchestrator 2>/dev/null || true
docker-compose build --no-cache orchestrator 2>&1 | grep -vE "obsolete|warning|already" || true
log_success "Orchestrator built"

log_info "Step 4: Starting Docker services..."

log_info "Starting orchestrator..."
orchestrator_output=$(docker-compose --profile monitoring up -d orchestrator 2>&1 || true)
if echo "$orchestrator_output" | grep -qE "Cannot connect|docker.sock"; then
    log_error "Docker is not ready. Waiting a bit more..."
    sleep 5
    if ! docker ps >/dev/null 2>&1; then
        log_error "Docker is still not available. Please ensure Docker Desktop is fully started."
        exit 1
    fi
    # Retry
    docker-compose --profile monitoring up -d orchestrator 2>&1 | grep -vE "obsolete" || true
fi
echo "$orchestrator_output" | grep -vE "obsolete|warning" || true
sleep 5

log_info "Waiting for orchestrator to be healthy..."
max_retries=15
retry=0
while [ $retry -lt $max_retries ]; do
    if curl -sf http://localhost:8080/healthz >/dev/null 2>&1; then
        log_success "Orchestrator is healthy!"
        break
    fi
    retry=$((retry + 1))
    if [ $retry -lt $max_retries ]; then
        echo -n "."
        sleep 2
    fi
done
echo ""

if ! curl -sf http://localhost:8080/healthz >/dev/null 2>&1; then
    log_error "Orchestrator failed to become healthy after ${max_retries} attempts"
    log_info "Checking orchestrator logs..."
    docker logs chaos-monkey-orchestrator 2>&1 | tail -20
    exit 1
fi

log_info "Starting Prometheus and Grafana..."
docker-compose --profile monitoring up -d prometheus grafana 2>&1 | grep -v "obsolete" || true
sleep 3

log_info "Step 5: Verifying all services..."
services_ok=true

if docker ps --format '{{.Names}}' 2>/dev/null | grep -q 'chaos-monkey-orchestrator'; then
    log_success "Orchestrator container is running"
else
    log_error "Orchestrator container is not running"
    services_ok=false
fi

if docker ps --format '{{.Names}}' 2>/dev/null | grep -q 'chaos-monkey-prometheus'; then
    log_success "Prometheus container is running"
else
    log_warning "Prometheus container is not running (optional)"
fi

if docker ps --format '{{.Names}}' 2>/dev/null | grep -q 'chaos-monkey-grafana'; then
    log_success "Grafana container is running"
else
    log_warning "Grafana container is not running (optional)"
fi

if [ "$services_ok" = false ]; then
    log_error "Critical services are not running. Cannot proceed."
    exit 1
fi

log_info "Step 6: Cleaning up old Prometheus targets..."
find config/prometheus-targets -name "*.json" -delete 2>/dev/null || true
docker restart chaos-monkey-prometheus >/dev/null 2>&1 || true
sleep 2
log_success "Prometheus targets cleaned"

echo ""
log_info "Service URLs:"
echo "  Orchestrator: http://localhost:8080"
echo "  Prometheus:   http://localhost:9091"
echo "  Grafana:      http://localhost:3000 (admin/admin)"
echo ""

export_config
