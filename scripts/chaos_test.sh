#!/bin/bash
set -uo pipefail

NUM_PARTICIPANTS="${NUM_PARTICIPANTS:-50}"
TEST_DURATION_SECONDS="${TEST_DURATION_SECONDS:-600}"
NUM_SPIKES=70
SPIKE_INTERVAL_SECONDS=5

# Chaos Intensity Levels (0.0 - 1.0)
CHAOS_INTENSITY=1.0
PACKET_LOSS_MAX_PERCENT=25
JITTER_MAX_MS=200
FRAME_DROP_MAX_PERCENT=60
BITRATE_REDUCTION_MIN_PERCENT=30

# Spike Distribution (percentages must sum to ~100)
SPIKE_TYPE_PACKET_LOSS=30
SPIKE_TYPE_JITTER=25
SPIKE_TYPE_FRAME_DROP=20
SPIKE_TYPE_BITRATE_REDUCE=15
SPIKE_TYPE_COMBINED=10

# Spike Duration Range
SPIKE_MIN_DURATION_SECONDS=2
SPIKE_MAX_DURATION_SECONDS=8

# Participant Targeting
TARGET_SINGLE_PARTICIPANT_PERCENT=40
TARGET_MULTIPLE_PARTICIPANTS_PERCENT=35
TARGET_ALL_PARTICIPANTS_PERCENT=25

# Monitoring Configuration
METRICS_UPDATE_INTERVAL=2
ENABLE_GRAFANA=true
GRAFANA_URL="http://localhost:3000"
PROMETHEUS_URL="http://localhost:9091"

# API Configuration
BASE_URL="${BASE_URL:-http://localhost:8080}"
TEST_ID="chaos_test_$(date +%s)"

# Visual Output Configuration
ENABLE_COLORS=true
ENABLE_PROGRESS_BARS=true

if [[ "${ENABLE_COLORS}" == "true" ]] && [[ -t 1 ]]; then
    RED='\033[0;31m'
    GREEN='\033[0;32m'
    YELLOW='\033[1;33m'
    BLUE='\033[0;34m'
    MAGENTA='\033[0;35m'
    CYAN='\033[0;36m'
    WHITE='\033[1;37m'
    BOLD='\033[1m'
    NC='\033[0m'
else
    RED=''
    GREEN=''
    YELLOW=''
    BLUE=''
    MAGENTA=''
    CYAN=''
    WHITE=''
    BOLD=''
    NC=''
fi

log_info() {
    echo -e "${BLUE}[INFO]${NC} $1"
}

log_success() {
    echo -e "${GREEN}[✓]${NC} $1"
}

log_warning() {
    echo -e "${YELLOW}[⚠]${NC} $1"
}

log_error() {
    echo -e "${RED}[✗]${NC} $1" >&2
}

log_chaos() {
    echo -e "${MAGENTA}[CHAOS]${NC} $1"
}

log_metrics() {
    echo -e "${CYAN}[METRICS]${NC} $1"
}

show_progress() {
    local current=$1
    local total=$2
    local width=50
    local percentage=$((current * 100 / total))
    local filled=$((current * width / total))
    local empty=$((width - filled))

    printf "\r${CYAN}[${NC}"
    printf "%${filled}s" | tr ' ' '█'
    printf "%${empty}s" | tr ' ' '░'
    printf "${CYAN}]${NC} ${BOLD}${percentage}%%${NC}"
}

command_exists() {
    command -v "$1" >/dev/null 2>&1
}

check_docker_memory() {
    local docker_mem=$(docker info 2>/dev/null | grep "Total Memory" | awk '{print $3}' | sed 's/GiB//')
    if [ -n "$docker_mem" ]; then
        docker_mem_int=${docker_mem%.*}
        if [ "$docker_mem_int" -lt 10 ] && [ "$NUM_PARTICIPANTS" -gt 100 ]; then
            log_warning "Docker only has ${docker_mem}GB memory but ${NUM_PARTICIPANTS} participants requested"
            log_warning "This may cause OOM crashes! Either:"
            log_warning "  1. Reduce NUM_PARTICIPANTS to 100 or less"
            log_warning "  2. Increase Docker Desktop memory: Settings -> Resources -> Memory"
            echo ""
            read -p "Continue anyway? (y/N) " -n 1 -r
            echo ""
            if [[ ! $REPLY =~ ^[Yy]$ ]]; then
                log_error "Aborted. Please configure Docker Desktop memory."
                exit 1
            fi
        fi
        log_info "Docker memory: ${docker_mem}GB"
    fi
}

check_api_health() {
    local max_retries=5
    local retry_count=0

    # Ensure kubectl port-forward is running for Kubernetes mode
    if command -v kubectl &> /dev/null && timeout 3 kubectl get pods -l app=orchestrator --no-headers 2>/dev/null | grep -q "Running"; then
        local pf_pid=$(pgrep -f "kubectl port-forward.*8080" | head -1)
        if [ -z "$pf_pid" ]; then
            log_info "Starting kubectl port-forward for orchestrator..."
            nohup kubectl port-forward svc/orchestrator 8080:8080 > /dev/null 2>&1 &
            sleep 2
        else
            # Check if process is suspended (state T)
            local pf_state=$(ps -o state= -p "$pf_pid" 2>/dev/null | tr -d ' ')
            if [ "$pf_state" = "T" ]; then
                log_info "Port-forward is suspended, restarting..."
                kill -9 "$pf_pid" 2>/dev/null
                nohup kubectl port-forward svc/orchestrator 8080:8080 > /dev/null 2>&1 &
                sleep 2
            fi
        fi
    fi

    while [ $retry_count -lt $max_retries ]; do
        if curl -sf --max-time 3 "${BASE_URL}/healthz" >/dev/null 2>&1; then
            return 0
        fi
        retry_count=$((retry_count + 1))
        # Try restarting port-forward on first failure
        if [ $retry_count -eq 1 ] && command -v kubectl &> /dev/null; then
            local pf_pid=$(pgrep -f "kubectl port-forward.*8080" | head -1)
            if [ -n "$pf_pid" ]; then
                log_info "Port-forward may be stuck, restarting..."
                kill -9 "$pf_pid" 2>/dev/null
                nohup kubectl port-forward svc/orchestrator 8080:8080 > /dev/null 2>&1 &
                sleep 2
            fi
        fi
        sleep 1
    done
    return 1
}

random_range() {
    local min=$1
    local max=$2
    echo $((RANDOM % (max - min + 1) + min))
}

random_float() {
    local min=$1
    local max=$2
    local precision=${3:-2}
    awk "BEGIN {printf \"%.${precision}f\", $min + rand() * ($max - $min)}"
}

multiply_int_float() {
    local int_val=$1
    local float_val=$2
    awk "BEGIN {printf \"%.0f\", $int_val * $float_val}"
}

select_random_participants() {
    local count=$1
    local total=$2
    local selected=()

    for ((i=0; i<count; i++)); do
        local pid=$((RANDOM % total + 1001))
        selected+=($pid)
    done

    echo "${selected[@]}"
}


api_create_test() {
    local num_participants=$1
    local duration=$2

    # Check if running in Kubernetes with multiple pods
    local k8s_pods=$(kubectl get pods -l app=orchestrator --no-headers 2>/dev/null | grep -c "Running" || echo 0)

    if [ "$k8s_pods" -gt 1 ]; then
        log_info "Creating test with ${num_participants} participants across ${k8s_pods} Kubernetes pods..."

        # Get all orchestrator pod names
        local pod_names=$(kubectl get pods -l app=orchestrator -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null | grep -v '^$')

        # Create tests in parallel using kubectl exec
        local pids=()
        local tmpdir=$(mktemp -d)
        local json_payload="{\"test_id\":\"${TEST_ID}\",\"num_participants\":${num_participants},\"video\":{\"width\":1280,\"height\":720,\"fps\":30,\"bitrate_kbps\":2500,\"codec\":\"h264\"},\"audio\":{\"sample_rate\":48000,\"channels\":1,\"bitrate_kbps\":128,\"codec\":\"opus\"},\"duration_seconds\":${duration},\"backend_rtp_base_port\":\"5000\"}"

        while IFS= read -r pod_name; do
            if [ -n "$pod_name" ]; then
                (
                    kubectl exec "$pod_name" -- wget -q -O- --post-data="$json_payload" --header='Content-Type: application/json' "http://localhost:8080/api/v1/test/create" >/dev/null 2>&1 && echo "1" > "$tmpdir/$pod_name"
                ) &
                pids+=($!)
            fi
        done <<< "$pod_names"

        # Wait for all background jobs
        for pid in "${pids[@]}"; do
            wait $pid 2>/dev/null || true
        done

        # Count successes
        local created=$(ls -1 "$tmpdir" 2>/dev/null | wc -l | tr -d ' ')
        rm -rf "$tmpdir"

        if [ "$created" -gt 0 ]; then
            # Each pod automatically handles its partition of participants
            local total_participants=$((num_participants))
            log_success "Test created on ${created}/${k8s_pods} pods: ${TEST_ID} (${total_participants} participants distributed across partitions)"
        else
            log_error "Failed to create test on any pod"
            exit 1
        fi
    else
        # Single pod or Docker mode
        log_info "Creating test with ${num_participants} participants..."

        local response=$(curl -sf -X POST "${BASE_URL}/api/v1/test/create" \
            -H "Content-Type: application/json" \
            -d "{
                \"test_id\": \"${TEST_ID}\",
                \"num_participants\": ${num_participants},
                \"video\": {
                    \"width\": 1280,
                    \"height\": 720,
                    \"fps\": 30,
                    \"bitrate_kbps\": 2500,
                    \"codec\": \"h264\"
                },
                \"audio\": {
                    \"sample_rate\": 48000,
                    \"channels\": 1,
                    \"bitrate_kbps\": 128,
                    \"codec\": \"opus\"
                },
                \"duration_seconds\": ${duration},
                \"backend_rtp_base_port\": \"5000\"
            }")

        if [ $? -eq 0 ]; then
            log_success "Test created: ${TEST_ID}"
            echo "$response"
        else
            log_error "Failed to create test"
            exit 1
        fi
    fi
}

api_start_test() {
    log_info "Starting test..."

    # Check if running in Kubernetes with multiple pods
    local k8s_pods=$(kubectl get pods -l app=orchestrator --no-headers 2>/dev/null | grep -c "Running" || echo 0)

    if [ "$k8s_pods" -gt 1 ]; then
        local pod_names=$(kubectl get pods -l app=orchestrator -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null | grep -v '^$')

        # Start tests in parallel using kubectl exec
        local pids=()
        local tmpdir=$(mktemp -d)

        while IFS= read -r pod_name; do
            if [ -n "$pod_name" ]; then
                (
                    kubectl exec "$pod_name" -- wget -q -O- --post-data='' "http://localhost:8080/api/v1/test/${TEST_ID}/start" >/dev/null 2>&1 && echo "1" > "$tmpdir/$pod_name"
                ) &
                pids+=($!)
            fi
        done <<< "$pod_names"

        # Wait for all
        for pid in "${pids[@]}"; do
            wait $pid 2>/dev/null || true
        done

        local started=$(ls -1 "$tmpdir" 2>/dev/null | wc -l | tr -d ' ')
        rm -rf "$tmpdir"

        if [ "$started" -gt 0 ]; then
            log_success "Test started on ${started}/${k8s_pods} pods"
        else
            log_error "Failed to start test on any pod"
            exit 1
        fi
    else
        if curl -sf -X POST "${BASE_URL}/api/v1/test/${TEST_ID}/start" >/dev/null; then
            log_success "Test started"
        else
            log_error "Failed to start test"
            exit 1
        fi
    fi
}

api_stop_test() {
    log_info "Stopping test..."

    # Check if running in Kubernetes with multiple pods
    local k8s_pods=$(kubectl get pods -l app=orchestrator --no-headers 2>/dev/null | grep -c "Running" || echo 0)

    if [ "$k8s_pods" -gt 1 ]; then
        local pod_names=$(kubectl get pods -l app=orchestrator -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null | grep -v '^$')

        # Stop tests in parallel using kubectl exec
        local pids=()
        local tmpdir=$(mktemp -d)

        while IFS= read -r pod_name; do
            if [ -n "$pod_name" ]; then
                (
                    kubectl exec "$pod_name" -- wget -q -O- --post-data='' "http://localhost:8080/api/v1/test/${TEST_ID}/stop" >/dev/null 2>&1 && echo "1" > "$tmpdir/$pod_name"
                ) &
                pids+=($!)
            fi
        done <<< "$pod_names"

        # Wait for all
        for pid in "${pids[@]}"; do
            wait $pid 2>/dev/null || true
        done

        local stopped=$(ls -1 "$tmpdir" 2>/dev/null | wc -l | tr -d ' ')
        rm -rf "$tmpdir"

        log_success "Test stopped on ${stopped}/${k8s_pods} pods"
    else
        local response=$(curl -sf -X POST "${BASE_URL}/api/v1/test/${TEST_ID}/stop")
        if [ $? -eq 0 ]; then
            log_success "Test stopped"
            echo "$response"
        else
            log_warning "Failed to stop test gracefully"
        fi
    fi
}

api_get_metrics() {
    # For Kubernetes: aggregate metrics from all pods
    if kubectl get pods -l app=orchestrator >/dev/null 2>&1; then
        local all_metrics=""
        local total_frames=0 total_packets=0 total_bytes=0 total_bitrate=0
        local total_jitter=0 total_loss=0 total_mos=0 total_participants=0
        local pod_count=0
        local state="unknown"

        for pod_name in $(kubectl get pods -l app=orchestrator -o jsonpath='{.items[*].metadata.name}' 2>/dev/null); do
            local metrics=$(kubectl exec "$pod_name" -- wget -qO- "http://localhost:8080/api/v1/test/${TEST_ID}/metrics" 2>/dev/null || echo "")
            if [ -n "$metrics" ] && echo "$metrics" | jq -e . >/dev/null 2>&1; then
                # Extract and sum values
                local agg=$(echo "$metrics" | jq -r '.aggregate // empty')
                if [ -n "$agg" ]; then
                    total_frames=$((total_frames + $(echo "$agg" | jq -r '.total_frames_sent // 0')))
                    total_packets=$((total_packets + $(echo "$agg" | jq -r '.total_packets_sent // 0')))
                    total_bytes=$((total_bytes + $(echo "$agg" | jq -r '.total_bytes_sent // 0')))
                    total_bitrate=$((total_bitrate + $(echo "$agg" | jq -r '.total_bitrate_kbps // 0')))
                    total_participants=$((total_participants + $(echo "$metrics" | jq -r '.participants | length // 0')))

                    # For averages, accumulate for later division
                    local jitter=$(echo "$agg" | jq -r '.avg_jitter_ms // 0')
                    local loss=$(echo "$agg" | jq -r '.avg_packet_loss // 0')
                    local mos=$(echo "$agg" | jq -r '.avg_mos_score // 0')
                    total_jitter=$(echo "$total_jitter + $jitter" | bc 2>/dev/null || echo "$total_jitter")
                    total_loss=$(echo "$total_loss + $loss" | bc 2>/dev/null || echo "$total_loss")
                    total_mos=$(echo "$total_mos + $mos" | bc 2>/dev/null || echo "$total_mos")

                    state=$(echo "$metrics" | jq -r '.state // "unknown"')
                    pod_count=$((pod_count + 1))
                fi
            fi
        done

        # Calculate averages
        if [ $pod_count -gt 0 ]; then
            local avg_jitter=$(echo "scale=2; $total_jitter / $pod_count" | bc 2>/dev/null || echo "0")
            local avg_loss=$(echo "scale=2; $total_loss / $pod_count" | bc 2>/dev/null || echo "0")
            local avg_mos=$(echo "scale=2; $total_mos / $pod_count" | bc 2>/dev/null || echo "0")

            # Return aggregated JSON
            cat <<EOF
{
    "test_id": "${TEST_ID}",
    "state": "${state}",
    "aggregate": {
        "total_frames_sent": ${total_frames},
        "total_packets_sent": ${total_packets},
        "total_bytes_sent": ${total_bytes},
        "total_bitrate_kbps": ${total_bitrate},
        "avg_jitter_ms": ${avg_jitter},
        "avg_packet_loss": ${avg_loss},
        "avg_mos_score": ${avg_mos}
    },
    "participants": $(printf '%.0s{"id":1},' $(seq 1 $total_participants) | sed 's/,$//' | sed 's/^/[/;s/$/]/')
}
EOF
        else
            echo ""
        fi
    else
        # Docker mode: single endpoint
        curl -sf "${BASE_URL}/api/v1/test/${TEST_ID}/metrics" 2>/dev/null || echo ""
    fi
}

api_inject_spike() {
    local spike_id=$1
    local spike_type=$2
    local participant_ids=$3
    local duration=$4
    local params=$5

    local pids_json=$(echo "$participant_ids" | awk '{
        printf "["
        for (i=1; i<=NF; i++) {
            if (i>1) printf ","
            printf "%s", $i
        }
        printf "]"
    }')

    local payload=$(cat <<EOF
{
    "spike_id": "${spike_id}",
    "type": "${spike_type}",
    "participant_ids": ${pids_json},
    "duration_seconds": ${duration},
    "params": ${params}
}
EOF
)

    local response
    response=$(curl -s -X POST "${BASE_URL}/api/v1/test/${TEST_ID}/spike" \
        -H "Content-Type: application/json" \
        -d "$payload" 2>&1)
    local exit_code=$?

    if [ $exit_code -eq 0 ] && echo "$response" | grep -q '"injected":true'; then
        return 0
    else
        if echo "$response" | grep -q "Test not found"; then
            log_error "Test ${TEST_ID} not found - cannot inject spikes"
            return 1
        elif echo "$response" | grep -q "already active"; then
            # Spike with this ID already active - not a real error
            return 0
        else
            log_warning "Failed to inject spike ${spike_id}"
            return 1
        fi
    fi
}

generate_packet_loss_spike() {
    local spike_id=$1
    local participants=$2
    local duration=$3

    local max_loss=$(multiply_int_float $PACKET_LOSS_MAX_PERCENT $CHAOS_INTENSITY)
    local loss_percent=$(random_range 1 $max_loss)
    local pattern=$([ $((RANDOM % 2)) -eq 0 ] && echo "random" || echo "burst")

    local params="{\"loss_percentage\": \"${loss_percent}\", \"pattern\": \"${pattern}\"}"

    if api_inject_spike "$spike_id" "rtp_packet_loss" "$participants" "$duration" "$params"; then
        log_chaos "Injected packet loss spike: ${loss_percent}% loss, pattern=${pattern}"
        return 0
    fi
    return 1
}

generate_jitter_spike() {
    local spike_id=$1
    local participants=$2
    local duration=$3

    local base_latency=$(random_range 10 50)
    local max_jitter=$(multiply_int_float $JITTER_MAX_MS $CHAOS_INTENSITY)
    local jitter=$(random_range 20 $max_jitter)

    local params="{\"base_latency_ms\": \"${base_latency}\", \"jitter_std_dev_ms\": \"${jitter}\"}"

    if api_inject_spike "$spike_id" "network_jitter" "$participants" "$duration" "$params"; then
        log_chaos "Injected jitter spike: base=${base_latency}ms, jitter=${jitter}ms"
        return 0
    fi
    return 1
}

generate_frame_drop_spike() {
    local spike_id=$1
    local participants=$2
    local duration=$3

    local max_drop=$(multiply_int_float $FRAME_DROP_MAX_PERCENT $CHAOS_INTENSITY)
    local drop_percent=$(random_range 10 $max_drop)

    local params="{\"drop_percentage\": \"${drop_percent}\"}"

    if api_inject_spike "$spike_id" "frame_drop" "$participants" "$duration" "$params"; then
        log_chaos "Injected frame drop spike: ${drop_percent}% frames dropped"
        return 0
    fi
    return 1
}

generate_bitrate_reduce_spike() {
    local spike_id=$1
    local participants=$2
    local duration=$3

    local reduction_percent=$(random_range $BITRATE_REDUCTION_MIN_PERCENT 80)
    local new_bitrate=$((2500 * (100 - reduction_percent) / 100))
    local transition=$(random_range 1 5)

    local params="{\"new_bitrate_kbps\": \"${new_bitrate}\", \"transition_seconds\": \"${transition}\"}"

    if api_inject_spike "$spike_id" "bitrate_reduce" "$participants" "$duration" "$params"; then
        log_chaos "Injected bitrate reduction spike: ${new_bitrate}kbps (${reduction_percent}% reduction)"
        return 0
    fi
    return 1
}

generate_combined_spike() {
    local spike_id=$1
    local participants=$2
    local duration=$3

    local spike_count=$(random_range 2 3)
    local success_count=0

    for ((i=1; i<=spike_count; i++)); do
        local type_rand=$((RANDOM % 4))
        case $type_rand in
            0) generate_packet_loss_spike "${spike_id}_p${i}" "$participants" "$duration" && success_count=$((success_count + 1)) ;;
            1) generate_jitter_spike "${spike_id}_j${i}" "$participants" "$duration" && success_count=$((success_count + 1)) ;;
            2) generate_frame_drop_spike "${spike_id}_f${i}" "$participants" "$duration" && success_count=$((success_count + 1)) ;;
            3) generate_bitrate_reduce_spike "${spike_id}_b${i}" "$participants" "$duration" && success_count=$((success_count + 1)) ;;
        esac
        sleep 0.2  # Small delay between combined spikes
    done

    if [ $success_count -gt 0 ]; then
        log_chaos "Injected combined spike with ${success_count}/${spike_count} effects"
        return 0
    fi
    return 1
}

select_participants_for_spike() {
    local rand=$((RANDOM % 100))

    if [ $rand -lt $TARGET_SINGLE_PARTICIPANT_PERCENT ]; then
        # Single participant
        echo "$(random_range 1001 $((1000 + NUM_PARTICIPANTS)))"
    elif [ $rand -lt $((TARGET_SINGLE_PARTICIPANT_PERCENT + TARGET_MULTIPLE_PARTICIPANTS_PERCENT)) ]; then
        # Multiple participants (2-5)
        local count=$(random_range 2 5)
        select_random_participants $count $NUM_PARTICIPANTS
    else
        # All participants (empty array means all)
        echo ""
    fi
}

inject_random_spike() {
    local spike_num=$1
    local spike_id="chaos_spike_${spike_num}_$(date +%s)"
    local duration=$(random_range $SPIKE_MIN_DURATION_SECONDS $SPIKE_MAX_DURATION_SECONDS)
    local participants=$(select_participants_for_spike)

    local spike_type_rand=$((RANDOM % 100))

    if [ $spike_type_rand -lt $SPIKE_TYPE_PACKET_LOSS ]; then
        generate_packet_loss_spike "$spike_id" "$participants" "$duration"
    elif [ $spike_type_rand -lt $((SPIKE_TYPE_PACKET_LOSS + SPIKE_TYPE_JITTER)) ]; then
        generate_jitter_spike "$spike_id" "$participants" "$duration"
    elif [ $spike_type_rand -lt $((SPIKE_TYPE_PACKET_LOSS + SPIKE_TYPE_JITTER + SPIKE_TYPE_FRAME_DROP)) ]; then
        generate_frame_drop_spike "$spike_id" "$participants" "$duration"
    elif [ $spike_type_rand -lt $((SPIKE_TYPE_PACKET_LOSS + SPIKE_TYPE_JITTER + SPIKE_TYPE_FRAME_DROP + SPIKE_TYPE_BITRATE_REDUCE)) ]; then
        generate_bitrate_reduce_spike "$spike_id" "$participants" "$duration"
    else
        generate_combined_spike "$spike_id" "$participants" "$duration"
    fi
}

display_metrics_summary() {
    local metrics_json=$1

    if [ -z "$metrics_json" ] || [ "$metrics_json" == "null" ]; then
        return
    fi

    if ! command_exists jq; then
        log_warning "jq not installed - install with: brew install jq"
        return
    fi

    if ! echo "$metrics_json" | jq -e . >/dev/null 2>&1; then
        return
    fi

    local state="running"
    local test_info=$(curl -sf "${BASE_URL}/api/v1/test/${TEST_ID}" 2>/dev/null)
    if [ -n "$test_info" ] && echo "$test_info" | jq -e . >/dev/null 2>&1; then
        state=$(echo "$test_info" | jq -r '.state // "running"')
    fi

    local elapsed=$(echo "$metrics_json" | jq -r '.elapsed_seconds // 0')
    local total_frames=$(echo "$metrics_json" | jq -r '.aggregate.total_frames_sent // 0')
    local total_packets=$(echo "$metrics_json" | jq -r '.aggregate.total_packets_sent // 0')
    local avg_jitter=$(echo "$metrics_json" | jq -r '(.aggregate.avg_jitter_ms // 0) * 100 | floor / 100')
    local avg_loss=$(echo "$metrics_json" | jq -r '(.aggregate.avg_packet_loss // 0) * 100 | floor / 100')
    local avg_mos=$(echo "$metrics_json" | jq -r '(.aggregate.avg_mos_score // 0) * 100 | floor / 100')
    local total_bitrate=$(echo "$metrics_json" | jq -r '.aggregate.total_bitrate_kbps // 0')
    local participant_count=$(echo "$metrics_json" | jq -r '(.participants // []) | length')
    if [ "$participant_count" = "0" ]; then
        participant_count="$NUM_PARTICIPANTS (aggregate only)"
    fi

    printf "\033[2K\r"

    log_metrics "State: ${BOLD}${state}${NC} | Elapsed: ${elapsed}s | Participants: ${participant_count}"
    log_metrics "Frames: ${BOLD}${total_frames}${NC} | Packets: ${BOLD}${total_packets}${NC} | Bitrate: ${BOLD}${total_bitrate}kbps${NC}"
    log_metrics "Avg Jitter: ${BOLD}${avg_jitter}ms${NC} | Avg Loss: ${BOLD}${avg_loss}%${NC} | Avg MOS: ${BOLD}${avg_mos}${NC}"

    local participants=$(echo "$metrics_json" | jq -r '.participants // empty')
    if [ -n "$participants" ] && [ "$participants" != "null" ]; then
        local problem_count=$(echo "$metrics_json" | jq -r '[.participants[]? | select((.packet_loss_percent // 0) > 5 or (.jitter_ms // 0) > 50)] | length' 2>/dev/null)
        if [ -n "$problem_count" ] && [ "$problem_count" -gt 0 ] 2>/dev/null; then
            local top_problems=$(echo "$metrics_json" | jq -r '[.participants[]? | select((.packet_loss_percent // 0) > 5 or (.jitter_ms // 0) > 50)] | sort_by(-.packet_loss_percent) | .[0:5][] | "P\(.participant_id): loss=\((.packet_loss_percent // 0) * 100 | floor / 100)% jitter=\((.jitter_ms // 0) | floor)ms"' 2>/dev/null)
            if [ -n "$top_problems" ]; then
                echo "$top_problems" | while read -r line; do
                    log_warning "$line"
                done
                if [ "$problem_count" -gt 5 ]; then
                    log_warning "... and $((problem_count - 5)) more participants with issues"
                fi
            fi
        fi
    fi
}

setup_grafana_dashboard() {
    if [ "${ENABLE_GRAFANA}" != "true" ]; then
        return
    fi

    log_info "Setting up Grafana monitoring..."

    # Ensure kubectl port-forward is running for Grafana in K8s mode
    if command -v kubectl &> /dev/null && ! pgrep -f "kubectl port-forward.*3000" > /dev/null 2>&1; then
        if timeout 3 kubectl get pods -l app=grafana --no-headers 2>/dev/null | grep -q "Running"; then
            log_info "Restarting kubectl port-forward for Grafana..."
            nohup kubectl port-forward svc/grafana 3000:3000 > /dev/null 2>&1 &
            sleep 2
        fi
    fi

    if ! curl -sf --max-time 3 "${GRAFANA_URL}/api/health" >/dev/null 2>&1; then
        log_warning "Grafana not accessible at ${GRAFANA_URL}"
        log_warning "Start it with: docker-compose --profile monitoring up -d"
        return
    fi

    log_success "Grafana is accessible"
    log_info "Grafana Dashboard URL: ${GRAFANA_URL}"
    log_info "Prometheus URL: ${PROMETHEUS_URL}"
    log_info ""
    log_info "To view metrics in Grafana:"
    log_info "1. Login at ${GRAFANA_URL} (admin/admin)"
    log_info "2. Go to Dashboards > Import"
    log_info "3. Use Prometheus data source (should be auto-configured)"
    log_info "4. Query metrics like: rtp_frames_sent_total, rtp_jitter_ms, rtp_packet_loss_percent"
    log_info ""

    if [ -n "${GRAFANA_API_KEY:-}" ]; then
        create_grafana_dashboard_via_api
    fi
}

create_grafana_dashboard_via_api() {
    log_info "Grafana API key detected - dashboard creation skipped (manual setup recommended)"
}

# Register test with Prometheus file-based service discovery
register_prometheus_target() {
    local test_id=$1
    local script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
    local project_root="$(cd "$script_dir/.." && pwd)"
    local targets_dir="${project_root}/config/prometheus/targets"

    mkdir -p "$targets_dir"

    local orchestrator_host="localhost"

    local prometheus_in_docker=false
    local orchestrator_in_docker=false

    if command -v docker >/dev/null 2>&1; then
        prometheus_in_docker=$(docker ps --format '{{.Names}}' 2>/dev/null | grep -q 'chaos-monkey-prometheus' && echo "true" || echo "false")
        orchestrator_in_docker=$(docker ps --format '{{.Names}}' 2>/dev/null | grep -q 'chaos-monkey-orchestrator' && echo "true" || echo "false")
    fi

    if [ "$prometheus_in_docker" = "true" ] && [ "$orchestrator_in_docker" = "true" ]; then
        orchestrator_host="orchestrator"
        log_info "Both Prometheus and Orchestrator in Docker - using 'orchestrator' hostname"
    elif [ "$prometheus_in_docker" = "true" ] && [ "$orchestrator_in_docker" = "false" ]; then
        orchestrator_host="host.docker.internal"
        log_info "Prometheus in Docker, Orchestrator on host - using host.docker.internal"
    else
        orchestrator_host="localhost"
        log_info "Using localhost for Prometheus target"
    fi

    local target_file="${targets_dir}/${test_id}.json"
    cat > "$target_file" <<EOF
[
  {
    "targets": ["${orchestrator_host}:8080"],
    "labels": {
      "test_id": "${test_id}",
      "job": "chaos-monkey"
    }
  }
]
EOF

    log_info "Registered test ${test_id} with Prometheus (${target_file})"
    log_info "Prometheus will scrape: http://${orchestrator_host}:8080/api/v1/test/${test_id}/metrics?format=prometheus"
}

unregister_prometheus_target() {
    local test_id=$1
    local script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
    local project_root="$(cd "$script_dir/.." && pwd)"
    local targets_dir="${project_root}/config/prometheus/targets"
    local target_file="${targets_dir}/${test_id}.json"

    if [ -f "$target_file" ]; then
        rm -f "$target_file"
        log_info "Unregistered test ${test_id} from Prometheus"
    fi
}

cleanup_old_prometheus_targets() {
    local script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
    local project_root="$(cd "$script_dir/.." && pwd)"
    local targets_dir="${project_root}/config/prometheus/targets"

    if [ ! -d "$targets_dir" ]; then
        return
    fi

    local cleaned=0
    for target_file in "$targets_dir"/*.json; do
        if [ -f "$target_file" ]; then
            local test_id=$(basename "$target_file" .json)
            if ! curl -sf "${BASE_URL}/api/v1/test/${test_id}/metrics" >/dev/null 2>&1; then
                rm -f "$target_file"
                cleaned=$((cleaned + 1))
            fi
        fi
    done

    if [ $cleaned -gt 0 ]; then
        log_info "Cleaned up ${cleaned} old Prometheus target file(s) for non-existent tests"
    fi
}


check_port_conflict() {
    local port=8080
    local conflict=false
    local conflicting_processes=""

    while IFS= read -r line; do
        cmd=$(echo "$line" | awk '{print $1}')
        # Allow Docker, kubectl (port-forward), and orchestrator processes
        if [[ "$cmd" != *"docke"* ]] && [[ "$cmd" != *"com.docke"* ]] && \
           [[ "$line" != *"chaos-monkey-orchestrator"* ]] && \
           [[ "$cmd" != *"Docker"* ]] && \
           [[ "$cmd" != *"kubectl"* ]]; then
            conflict=true
            conflicting_processes="$conflicting_processes\n$line"
        fi
    done < <(lsof -i :${port} 2>/dev/null | grep LISTEN || true)

    if [ "$conflict" = true ]; then
        log_warning "Port ${port} is already in use by a non-Docker/Kubernetes process!"
        log_warning "This will conflict with the orchestrator."
        echo ""
        echo "Processes using port ${port}:"
        echo -e "$conflicting_processes"
        echo ""
        log_error "Please stop the conflicting process or use Docker/Kubernetes."
        log_info "To use Docker: docker-compose --profile monitoring up -d"
        log_info "To use Kubernetes: ./scripts/start_k8s.sh"
        log_info "To check: lsof -i :8080"
        return 1
    fi

    # Check for Docker orchestrator
    if docker ps --format '{{.Names}}' 2>/dev/null | grep -qE 'chaos-monkey-orchestrator|choas-monkey-orchestrator'; then
        log_info "Docker orchestrator is running ✓"
        return 0
    fi

    # Check for Kubernetes orchestrator pods (kubectl port-forward)
    if command -v kubectl &> /dev/null; then
        local running_pods=$(timeout 5 kubectl get pods -l app=orchestrator --no-headers 2>/dev/null | grep -c "Running" 2>/dev/null | tr -d '[:space:]')
        running_pods=${running_pods:-0}
        if [ "$running_pods" -gt 0 ] 2>/dev/null; then
            log_info "Kubernetes orchestrator pods running (${running_pods} pods) ✓"
            return 0
        fi
    fi

    # Check if kubectl port-forward is running on 8080 (means K8s is set up)
    if pgrep -f "kubectl port-forward.*8080" > /dev/null 2>&1; then
        log_info "Kubernetes port-forward is active ✓"
        return 0
    fi

    log_warning "No orchestrator detected (Docker or Kubernetes)"
    log_info "Start Docker: docker-compose --profile monitoring up -d orchestrator"
    log_info "Start Kubernetes: ./scripts/start_k8s.sh"
    return 1
}

main() {
    clear

    log_info "Configuration:"
    echo -e "  Participants: ${BOLD}${NUM_PARTICIPANTS}${NC}"
    echo -e "  Test Duration: ${BOLD}${TEST_DURATION_SECONDS}s${NC}"
    echo -e "  Number of Spikes: ${BOLD}${NUM_SPIKES}${NC}"
    echo -e "  Chaos Intensity: ${BOLD}${CHAOS_INTENSITY}${NC}"
    echo -e "  Test ID: ${BOLD}${TEST_ID}${NC}"
    echo ""

    log_info "Running pre-flight checks..."

    if ! command_exists curl; then
        log_error "curl is required but not installed"
        exit 1
    fi

    if ! command_exists jq; then
        log_warning "jq is recommended for better output (install with: brew install jq)"
    fi

    check_docker_memory

    if ! check_port_conflict; then
        log_error "Port conflict detected. Please resolve before continuing."
        exit 1
    fi

    if ! check_api_health; then
        log_error "API server is not healthy at ${BASE_URL}"
        log_error "Start Docker orchestrator: docker-compose --profile monitoring up -d orchestrator"
        log_error "Start Kubernetes orchestrator: ./scripts/start_k8s.sh"
        log_error "Or if running locally: ./chaos-monkey -http :8080"
        exit 1
    fi
    log_success "API server is healthy"

    setup_grafana_dashboard

    cleanup_old_prometheus_targets

    api_create_test $NUM_PARTICIPANTS $TEST_DURATION_SECONDS

    register_prometheus_target "$TEST_ID"

    api_start_test

    local start_time=$(date +%s)
    local end_time=$((start_time + TEST_DURATION_SECONDS))
    local spike_count=0
    local next_spike_time=$((start_time + SPIKE_INTERVAL_SECONDS))
    local last_metrics_update=0

    log_success "Test is running! Injecting chaos..."
    echo ""

    while true; do
        local current_time=$(date +%s)
        local elapsed=$((current_time - start_time))
        local remaining=$((end_time - current_time))

        if [ $current_time -ge $end_time ]; then
            break
        fi

        if [ $current_time -ge $next_spike_time ] && [ $spike_count -lt $NUM_SPIKES ]; then
            spike_count=$((spike_count + 1))
            echo ""
            log_chaos "Injecting spike ${spike_count}/${NUM_SPIKES}..."
            inject_random_spike $spike_count
            next_spike_time=$((current_time + SPIKE_INTERVAL_SECONDS))
        fi

        if [ $((current_time - last_metrics_update)) -ge $METRICS_UPDATE_INTERVAL ]; then
            if ! check_api_health >/dev/null 2>&1; then
                log_error "Orchestrator is not responding - test may have stopped"
                log_error "Restart orchestrator and run the test again"
                break
            fi

            local metrics=$(api_get_metrics)
            if [ -n "$metrics" ] && [ "$metrics" != "null" ]; then
                if echo "$metrics" | grep -q "Test not found"; then
                    log_error "Test ${TEST_ID} not found - test may have stopped on server"
                    log_error "The test may have crashed or been stopped. Check orchestrator logs."
                    break
                elif echo "$metrics" | grep -qE "\"test_id\"|\"aggregate\""; then
                    display_metrics_summary "$metrics"
                else
                    log_warning "Unexpected metrics response format"
                fi
            else
                if [ $((current_time % 10)) -eq 0 ]; then
                    log_warning "Metrics temporarily unavailable (test may be starting or API issue)..."
                fi
            fi
            last_metrics_update=$current_time

            local progress=$((elapsed * 100 / TEST_DURATION_SECONDS))
            show_progress $elapsed $TEST_DURATION_SECONDS
            echo -n " | Remaining: ${remaining}s | Spikes: ${spike_count}/${NUM_SPIKES}"
        fi

        sleep 1
    done

    echo ""
    echo ""
    log_info "Test duration completed. Stopping test..."

    local final_response=$(api_stop_test)

    unregister_prometheus_target "$TEST_ID"
    if [ -n "$final_response" ] && command_exists jq; then
        if echo "$final_response" | jq -e . >/dev/null 2>&1; then
            local final_metrics=$(echo "$final_response" | jq -r '.final_metrics // empty')
            if [ -n "$final_metrics" ] && [ "$final_metrics" != "null" ]; then
                display_metrics_summary "$final_metrics"
            fi
        fi
    fi

    log_success "Chaos test completed!"
    log_info "Total spikes injected: ${spike_count}"
    log_info "Test ID: ${TEST_ID}"
    log_info "View Grafana dashboard at: ${GRAFANA_URL}"
    echo ""
}

cleanup() {
    if [ "${CLEANUP_DONE:-0}" -eq 0 ]; then
        export CLEANUP_DONE=1
        log_info "Cleaning up..."
        unregister_prometheus_target "$TEST_ID" 2>/dev/null || true
    fi
}

handle_signal() {
    log_warning "Received signal - will cleanup when test completes naturally"
}

trap cleanup EXIT
trap handle_signal INT TERM

main "$@"
