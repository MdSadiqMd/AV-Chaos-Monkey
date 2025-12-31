#!/bin/bash
set -euo pipefail

# Auto-detects system resources and calculates optimal pod count
# Override with: REPLICAS=5 NUM_PARTICIPANTS=750 ./start_k8s.sh
TEST_DURATION_SECONDS=${TEST_DURATION_SECONDS:-600}

# These will be auto-detected if not provided
USER_REPLICAS=${REPLICAS:-}
USER_PARTICIPANTS=${NUM_PARTICIPANTS:-}

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
K8S_DIR="$PROJECT_ROOT/k8s"

log_info() { echo -e "${BLUE}[INFO]${NC} $1"; }
log_success() { echo -e "${GREEN}[✓]${NC} $1"; }
log_warning() { echo -e "${YELLOW}[⚠]${NC} $1"; }
log_error() { echo -e "${RED}[✗]${NC} $1"; exit 1; }
log_config() { echo -e "${MAGENTA}[CONFIG]${NC} $1"; }

detect_system_memory() {
    local mem_bytes
    if [[ "$(uname)" == "Darwin" ]]; then
        mem_bytes=$(sysctl -n hw.memsize 2>/dev/null || echo 0)
    else
        mem_bytes=$(grep MemTotal /proc/meminfo 2>/dev/null | awk '{print $2 * 1024}' || echo 0)
    fi
    echo $((mem_bytes / 1024 / 1024 / 1024))
}

detect_docker_memory() {
    local docker_mem_gb=8  # Default fallback
    local docker_mem=$(docker info 2>/dev/null | grep "Total Memory" | awk '{print $3}' | sed 's/GiB//')
    if [ -n "$docker_mem" ]; then
        docker_mem_gb="${docker_mem%.*}"
    fi
    echo "$docker_mem_gb"
}

detect_cpu_cores() {
    if [[ "$(uname)" == "Darwin" ]]; then
        sysctl -n hw.ncpu 2>/dev/null || echo 4
    else
        nproc 2>/dev/null || echo 4
    fi
}

calculate_optimal_replicas() {
    local docker_mem=$1
    local replicas=1
    
    # Memory per pod: 2GB (matches StatefulSet resource limits)
    # Reserve 2GB for Prometheus + Grafana + system
    local available_mem=$((docker_mem - 2))
    
    if [ "$available_mem" -ge 20 ]; then
        replicas=10  # 20GB+ → 10 pods
    elif [ "$available_mem" -ge 16 ]; then
        replicas=8   # 16GB+ → 8 pods
    elif [ "$available_mem" -ge 12 ]; then
        replicas=6   # 12GB+ → 6 pods
    elif [ "$available_mem" -ge 8 ]; then
        replicas=4   # 8GB+ → 4 pods
    elif [ "$available_mem" -ge 6 ]; then
        replicas=3   # 6GB+ → 3 pods
    elif [ "$available_mem" -ge 4 ]; then
        replicas=2   # 4GB+ → 2 pods
    else
        replicas=1   # <4GB → 1 pod
    fi
    
    echo "$replicas"
}

# Calculate participants per pod (~150 per 2GB)
calculate_participants_per_pod() {
    echo 150
}

detect_and_configure() {
    local system_mem=$(detect_system_memory)
    local docker_mem=$(detect_docker_memory)
    local cpu_cores=$(detect_cpu_cores)
    
    log_info "System Detection:"
    echo -e "  System RAM:    ${BOLD}${system_mem}GB${NC}"
    echo -e "  Docker Memory: ${BOLD}${docker_mem}GB${NC}"
    echo -e "  CPU Cores:     ${BOLD}${cpu_cores}${NC}"
    echo ""
    
    # Calculate optimal values
    local optimal_replicas=$(calculate_optimal_replicas "$docker_mem")
    local participants_per_pod=$(calculate_participants_per_pod)
    local optimal_participants=$((optimal_replicas * participants_per_pod))
    
    # Use user-provided values if set, otherwise use auto-detected
    if [ -n "$USER_REPLICAS" ]; then
        REPLICAS=$USER_REPLICAS
        log_config "Using user-specified replicas: ${REPLICAS}"
    else
        REPLICAS=$optimal_replicas
        log_config "Auto-detected optimal replicas: ${REPLICAS}"
    fi
    
    if [ -n "$USER_PARTICIPANTS" ]; then
        NUM_PARTICIPANTS=$USER_PARTICIPANTS
        log_config "Using user-specified participants: ${NUM_PARTICIPANTS}"
    else
        NUM_PARTICIPANTS=$((REPLICAS * participants_per_pod))
        log_config "Auto-calculated participants: ${NUM_PARTICIPANTS}"
    fi
    
    # Validate configuration
    local mem_per_pod=2
    local required_mem=$((REPLICAS * mem_per_pod + 2))  # +2 for Prometheus/Grafana
    
    if [ "$required_mem" -gt "$docker_mem" ]; then
        log_warning "Configuration requires ~${required_mem}GB but Docker has ${docker_mem}GB"
        log_warning "Consider: Docker Desktop → Settings → Resources → Memory → ${required_mem}GB+"
        echo ""

        local safe_replicas=$(calculate_optimal_replicas "$docker_mem")
        local safe_participants=$((safe_replicas * participants_per_pod))
        log_info "Suggested safe config for ${docker_mem}GB Docker:"
        echo -e "  REPLICAS=${safe_replicas} NUM_PARTICIPANTS=${safe_participants} ./scripts/start_k8s.sh"
        echo ""
    fi
    
    echo ""
    echo -e "${BOLD}Final Configuration:${NC}"
    echo -e "  Replicas:          ${CYAN}${REPLICAS} pods${NC}"
    echo -e "  Participants:      ${CYAN}${NUM_PARTICIPANTS}${NC} (~$((NUM_PARTICIPANTS / REPLICAS)) per pod)"
    echo -e "  Memory per pod:    ${CYAN}2GB${NC}"
    echo -e "  Total memory:      ${CYAN}$((REPLICAS * 2 + 2))GB${NC} (pods + monitoring)"
    echo -e "  Throughput:        ${CYAN}${REPLICAS}x${NC}"
    echo ""
}

check_prerequisites() {
    log_info "Checking prerequisites..."
    
    if ! command -v kubectl &> /dev/null; then
        log_error "kubectl not found. Install with: brew install kubectl"
    fi
    
    if ! command -v docker &> /dev/null; then
        log_error "docker not found. Install Docker Desktop."
    fi
    
    if ! kubectl cluster-info &> /dev/null; then
        log_error "Kubernetes cluster not available. Enable Kubernetes in Docker Desktop:
  Settings → Kubernetes → Enable Kubernetes"
    fi
    
    log_success "Prerequisites OK"
}

build_image() {
    log_info "Building Docker image..."
    cd "$PROJECT_ROOT"
    docker build -t chaos-monkey-orchestrator:latest . --quiet
    log_success "Docker image built"
}

# Generate StatefulSet YAML dynamically
update_replicas() {
    log_info "Generating Kubernetes manifests for $REPLICAS replicas..."
    
    # Generate orchestrator StatefulSet
    cat > "$K8S_DIR/orchestrator/orchestrator.yaml" << EOF
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: orchestrator
  labels:
    app: orchestrator
spec:
  serviceName: orchestrator
  replicas: $REPLICAS
  podManagementPolicy: Parallel
  selector:
    matchLabels:
      app: orchestrator
  template:
    metadata:
      labels:
        app: orchestrator
      annotations:
        prometheus.io/scrape: "true"
        prometheus.io/port: "8080"
        prometheus.io/path: "/metrics"
    spec:
      containers:
      - name: orchestrator
        image: chaos-monkey-orchestrator:latest
        imagePullPolicy: IfNotPresent
        ports:
        - containerPort: 8080
          name: http
        env:
        - name: LOG_LEVEL
          value: "info"
        - name: POD_NAME
          valueFrom:
            fieldRef:
              fieldPath: metadata.name
        - name: TOTAL_PARTITIONS
          value: "$REPLICAS"
        resources:
          requests:
            cpu: "1"
            memory: "2Gi"
          limits:
            cpu: "4"
            memory: "2Gi"
        livenessProbe:
          httpGet:
            path: /healthz
            port: 8080
          initialDelaySeconds: 10
          periodSeconds: 10
        readinessProbe:
          httpGet:
            path: /healthz
            port: 8080
          initialDelaySeconds: 5
          periodSeconds: 5
        command:
        - /bin/sh
        - -c
        - |
          export PARTITION_ID=\$(echo \$POD_NAME | grep -o '[0-9]*\$')
          echo "Starting orchestrator partition \$PARTITION_ID of \$TOTAL_PARTITIONS"
          exec /usr/local/bin/orchestrator -http :8080
---
apiVersion: v1
kind: Service
metadata:
  name: orchestrator
  labels:
    app: orchestrator
spec:
  type: ClusterIP
  ports:
  - port: 8080
    targetPort: 8080
    protocol: TCP
    name: http
  selector:
    app: orchestrator
---
apiVersion: v1
kind: Service
metadata:
  name: orchestrator-headless
  labels:
    app: orchestrator
spec:
  clusterIP: None
  ports:
  - port: 8080
    targetPort: 8080
    name: http
  selector:
    app: orchestrator
EOF

    # Generate initial Prometheus config (will be updated with pod IPs later)
    cat > "$K8S_DIR/monitoring/prometheus.yaml" << 'EOF'
apiVersion: v1
kind: ConfigMap
metadata:
  name: prometheus-config
data:
  prometheus.yml: |
    global:
      scrape_interval: 5s
      evaluation_interval: 5s

    scrape_configs:
      - job_name: 'orchestrator'
        static_configs:
          - targets: ['placeholder:8080']
        metrics_path: /metrics

      - job_name: 'prometheus'
        static_configs:
          - targets: ['localhost:9090']
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: prometheus
  labels:
    app: prometheus
spec:
  replicas: 1
  selector:
    matchLabels:
      app: prometheus
  template:
    metadata:
      labels:
        app: prometheus
    spec:
      serviceAccountName: prometheus
      containers:
      - name: prometheus
        image: prom/prometheus:latest
        ports:
        - containerPort: 9090
        volumeMounts:
        - name: config
          mountPath: /etc/prometheus
        args:
        - '--config.file=/etc/prometheus/prometheus.yml'
        - '--storage.tsdb.path=/prometheus'
        - '--web.enable-lifecycle'
      volumes:
      - name: config
        configMap:
          name: prometheus-config
---
apiVersion: v1
kind: Service
metadata:
  name: prometheus
spec:
  type: NodePort
  ports:
  - port: 9090
    targetPort: 9090
    nodePort: 30090
  selector:
    app: prometheus
EOF

    log_success "Generated manifests for $REPLICAS replicas"
}

deploy_k8s() {
    log_info "Deploying to Kubernetes..."
    
    # Create dashboard ConfigMap from JSON file
    log_info "Creating Grafana dashboard ConfigMap..."
    kubectl create configmap grafana-dashboard \
        --from-file=chaos-monkey-dashboard.json="$PROJECT_ROOT/config/grafana/dashboard.json" \
        --dry-run=client -o yaml | kubectl apply -f -
    
    # Apply RBAC first (needed for Prometheus to discover pods)
    kubectl apply -f "$K8S_DIR/monitoring/prometheus-rbac.yaml"
    
    # Apply manifests
    kubectl apply -f "$K8S_DIR/orchestrator/orchestrator.yaml"
    kubectl apply -f "$K8S_DIR/monitoring/prometheus.yaml"
    kubectl apply -f "$K8S_DIR/monitoring/grafana.yaml"
    
    log_info "Waiting for pods to be ready..."
    kubectl wait --for=condition=ready pod -l app=orchestrator --timeout=120s
    
    log_success "Kubernetes deployment ready"
}

wait_for_pods() {
    log_info "Waiting for all $REPLICAS orchestrator pods..."
    
    local ready=0
    local max_wait=60
    local waited=0
    
    while [ $ready -lt $REPLICAS ] && [ $waited -lt $max_wait ]; do
        ready=$(kubectl get pods -l app=orchestrator --no-headers 2>/dev/null | grep -c "Running" || echo 0)
        echo -ne "\r  Pods ready: $ready/$REPLICAS"
        sleep 2
        waited=$((waited + 2))
    done
    echo ""
    
    if [ $ready -lt $REPLICAS ]; then
        log_warning "Only $ready/$REPLICAS pods ready after ${max_wait}s"
    else
        log_success "All $REPLICAS pods ready"
    fi
}

# Update Prometheus config with actual pod IPs
update_prometheus_targets() {
    log_info "Configuring Prometheus with orchestrator pod IPs..."
    
    # Get all orchestrator pod IPs
    local pod_ips=$(kubectl get pods -l app=orchestrator -o jsonpath='{range .items[*]}{.status.podIP}{"\n"}{end}' 2>/dev/null | grep -v '^$')
    
    if [ -z "$pod_ips" ]; then
        log_warning "Could not get pod IPs, Prometheus may not scrape correctly"
        return
    fi
    
    # Build targets list
    local targets=""
    while IFS= read -r ip; do
        if [ -n "$ip" ]; then
            targets="${targets}            - '${ip}:8080'\n"
        fi
    done <<< "$pod_ips"
    
    # Generate updated prometheus.yaml with actual pod IPs
    cat > "$K8S_DIR/monitoring/prometheus.yaml" << EOF
apiVersion: v1
kind: ConfigMap
metadata:
  name: prometheus-config
data:
  prometheus.yml: |
    global:
      scrape_interval: 5s
      evaluation_interval: 5s

    scrape_configs:
      - job_name: 'orchestrator'
        static_configs:
          - targets:
$(echo -e "$targets")
        metrics_path: /metrics

      - job_name: 'prometheus'
        static_configs:
          - targets: ['localhost:9090']
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: prometheus
  labels:
    app: prometheus
spec:
  replicas: 1
  selector:
    matchLabels:
      app: prometheus
  template:
    metadata:
      labels:
        app: prometheus
    spec:
      serviceAccountName: prometheus
      containers:
      - name: prometheus
        image: prom/prometheus:latest
        ports:
        - containerPort: 9090
        volumeMounts:
        - name: config
          mountPath: /etc/prometheus
        args:
        - '--config.file=/etc/prometheus/prometheus.yml'
        - '--storage.tsdb.path=/prometheus'
        - '--web.enable-lifecycle'
      volumes:
      - name: config
        configMap:
          name: prometheus-config
---
apiVersion: v1
kind: Service
metadata:
  name: prometheus
spec:
  type: NodePort
  ports:
  - port: 9090
    targetPort: 9090
    nodePort: 30090
  selector:
    app: prometheus
EOF

    # Apply the updated config
    kubectl apply -f "$K8S_DIR/monitoring/prometheus.yaml" >/dev/null
    
    # Restart Prometheus to pick up new config
    kubectl rollout restart deployment/prometheus >/dev/null
    
    # Wait for Prometheus to be ready
    kubectl wait --for=condition=ready pod -l app=prometheus --timeout=60s >/dev/null 2>&1 || true
    
    local target_count=$(echo "$pod_ips" | wc -l | tr -d ' ')
    log_success "Prometheus configured with $target_count orchestrator targets"
}

# Port forward for local access
setup_port_forward() {
    log_info "Setting up port forwarding..."
    
    # Kill any existing port-forwards
    pkill -f "kubectl port-forward" 2>/dev/null || true
    sleep 1
    
    # Forward orchestrator (load balanced via service)
    kubectl port-forward svc/orchestrator 8080:8080 &>/dev/null &
    
    # Forward Prometheus
    kubectl port-forward svc/prometheus 9091:9090 &>/dev/null &
    
    # Forward Grafana
    kubectl port-forward svc/grafana 3000:3000 &>/dev/null &
    
    sleep 2
    log_success "Port forwarding active"
    echo -e "  Orchestrator: ${CYAN}http://localhost:8080${NC}"
    echo -e "  Prometheus:   ${CYAN}http://localhost:9091${NC}"
    echo -e "  Grafana:      ${CYAN}http://localhost:3000${NC} (admin/admin)"
}

# Run chaos test
run_test() {
    log_info "Starting chaos test with $NUM_PARTICIPANTS participants across $REPLICAS pods..."
    echo ""
    
    export NUM_PARTICIPANTS
    export TEST_DURATION_SECONDS
    export BASE_URL="http://localhost:8080"
    
    cd "$SCRIPT_DIR"
    exec ./chaos_test.sh
}

show_status() {
    echo ""
    echo -e "${BOLD}${GREEN}╔════════════════════════════════════════════════════════════════╗${NC}"
    echo -e "${BOLD}${GREEN}║     ALL SERVICES READY - ${REPLICAS}x THROUGHPUT               ║${NC}"
    echo -e "${BOLD}${GREEN}╚════════════════════════════════════════════════════════════════╝${NC}"
    echo ""
}

cleanup() {
    log_info "Cleaning up Kubernetes resources..."
    kubectl delete -f "$K8S_DIR/orchestrator/orchestrator.yaml" --ignore-not-found=true
    kubectl delete -f "$K8S_DIR/monitoring/prometheus.yaml" --ignore-not-found=true
    kubectl delete -f "$K8S_DIR/monitoring/grafana.yaml" --ignore-not-found=true
    kubectl delete -f "$K8S_DIR/monitoring/prometheus-rbac.yaml" --ignore-not-found=true
    kubectl delete configmap grafana-dashboard --ignore-not-found=true
    pkill -f "kubectl port-forward" 2>/dev/null || true
    log_success "Cleanup complete"
}

if [ "${1:-}" = "--cleanup" ] || [ "${1:-}" = "cleanup" ]; then
    cleanup
    exit 0
fi

if [ "${1:-}" = "--status" ] || [ "${1:-}" = "status" ]; then
    echo -e "${BOLD}Kubernetes Status:${NC}"
    kubectl get pods -l app=orchestrator
    echo ""
    kubectl get svc
    exit 0
fi

main() {
    echo ""
    echo -e "${BOLD}${CYAN}╔════════════════════════════════════════════════════════════════╗${NC}"
    echo -e "${BOLD}${CYAN}║     AV CHAOS MONKEY - KUBERNETES MODE                          ║${NC}"
    echo -e "${BOLD}${CYAN}╚════════════════════════════════════════════════════════════════╝${NC}"
    echo ""
    
    check_prerequisites
    detect_and_configure  # Auto-detect system resources
    build_image
    update_replicas
    deploy_k8s
    wait_for_pods
    update_prometheus_targets  # Configure Prometheus with actual pod IPs
    setup_port_forward
    show_status
    run_test
}

trap 'pkill -f "kubectl port-forward" 2>/dev/null || true' EXIT
main "$@"
