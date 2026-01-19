package main

import (
	"fmt"
	"log"
	"math"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/MdSadiqMd/AV-Chaos-Monkey/pkg/client"
)

func init() {
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)
}

type RTPMetrics struct {
	ParticipantID uint32

	// Counters (atomic for thread safety)
	packetsReceived atomic.Int64
	packetsLost     atomic.Int64
	bytesReceived   atomic.Int64

	// Jitter calculation (RFC 3550)
	mu              sync.RWMutex
	lastTimestamp   uint32
	lastArrivalTime time.Time
	transit         int32
	jitter          float64
	jitterSamples   []float64

	// RTT tracking
	rttSamples []time.Duration
	avgRTT     time.Duration

	// Sequence tracking for packet loss
	expectedSeq     uint16
	receivedSeqNums map[uint16]bool
	seqWrap         int
}

func NewRTPMetrics(participantID uint32) *RTPMetrics {
	return &RTPMetrics{
		ParticipantID:   participantID,
		jitterSamples:   make([]float64, 0, 100),
		rttSamples:      make([]time.Duration, 0, 100),
		receivedSeqNums: make(map[uint16]bool),
	}
}

func (m *RTPMetrics) RecordPacketReceived(rtpTimestamp uint32, seqNum uint16, size int) {
	m.packetsReceived.Add(1)
	m.bytesReceived.Add(int64(size))

	m.mu.Lock()
	defer m.mu.Unlock()

	arrivalTime := time.Now()
	m.receivedSeqNums[seqNum] = true

	// Handle sequence wrap-around
	if m.expectedSeq > 0 && seqNum < 1000 && m.expectedSeq > 65000 {
		m.seqWrap++
	}
	m.expectedSeq = seqNum + 1

	// RFC 3550 jitter calculation
	if m.lastTimestamp != 0 {
		arrivalMs := int32(arrivalTime.UnixNano() / 1000000)
		rtpMs := int32(rtpTimestamp / 90)
		transit := arrivalMs - rtpMs
		d := transit - m.transit
		if d < 0 {
			d = -d
		}
		m.jitter = m.jitter + (float64(d)-m.jitter)/16.0
		m.jitterSamples = append(m.jitterSamples, m.jitter)
		if len(m.jitterSamples) > 1000 {
			m.jitterSamples = m.jitterSamples[500:]
		}
		m.transit = transit
	} else {
		m.transit = int32(arrivalTime.UnixNano()/1000000) - int32(rtpTimestamp/90)
	}

	m.lastTimestamp = rtpTimestamp
	m.lastArrivalTime = arrivalTime
}

func (m *RTPMetrics) RecordRTT(rtt time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rttSamples = append(m.rttSamples, rtt)
	if len(m.rttSamples) > 100 {
		m.rttSamples = m.rttSamples[1:]
	}
	var total time.Duration
	for _, r := range m.rttSamples {
		total += r
	}
	m.avgRTT = total / time.Duration(len(m.rttSamples))
}

func (m *RTPMetrics) GetJitter() float64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.jitter
}

func (m *RTPMetrics) GetPacketLoss() float64 {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if len(m.receivedSeqNums) == 0 {
		return 0
	}

	var minSeq, maxSeq uint16
	first := true
	for seq := range m.receivedSeqNums {
		if first {
			minSeq, maxSeq = seq, seq
			first = false
		} else {
			if seq < minSeq {
				minSeq = seq
			}
			if seq > maxSeq {
				maxSeq = seq
			}
		}
	}

	expected := int(maxSeq) - int(minSeq) + 1 + m.seqWrap*65536
	received := len(m.receivedSeqNums)
	if expected <= 0 {
		return 0
	}
	return float64(expected-received) / float64(expected) * 100.0
}

func (m *RTPMetrics) GetRTT() time.Duration {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.avgRTT
}

// Calculates Mean Opinion Score using ITU-T G.107 E-model (1.0-4.5)
func (m *RTPMetrics) GetMOS() float64 {
	packetLoss := m.GetPacketLoss()
	jitter := m.GetJitter()
	rtt := m.GetRTT().Milliseconds()

	r0 := 93.2
	delay := float64(rtt) / 2

	id := 0.024 * delay
	if delay > 177.3 {
		id = 0.024*delay + 0.11*(delay-177.3)*(1.0-math.Exp(-(delay-177.3)/100.0))
	}

	ie := 0.0
	if packetLoss > 0 {
		ie = 30.0 * math.Log(1.0+15.0*packetLoss/100.0)
	}
	if jitter > 10 {
		ie += (jitter - 10) * 0.5
	}

	r := r0 - id - ie
	if r < 0 {
		r = 0
	}
	if r > 100 {
		r = 100
	}

	mos := 1.0 + 0.035*r + r*(r-60)*(100-r)*7e-6
	if mos < 1.0 {
		mos = 1.0
	}
	if mos > 4.5 {
		mos = 4.5
	}
	return mos
}

func (m *RTPMetrics) Snapshot() map[string]any {
	return map[string]any{
		"participant_id":      m.ParticipantID,
		"packets_received":    m.packetsReceived.Load(),
		"bytes_received":      m.bytesReceived.Load(),
		"jitter_ms":           m.GetJitter(),
		"packet_loss_percent": m.GetPacketLoss(),
		"mos_score":           m.GetMOS(),
		"rtt_ms":              m.GetRTT().Milliseconds(),
	}
}

func main() {
	if len(os.Args) < 3 {
		printUsage()
		os.Exit(1)
	}

	baseURL := os.Args[1]
	testID := os.Args[2]
	maxConnections := parseMaxConnections()

	// Always run receiver locally - scale connector pods in cluster for performance
	logConnectionMode(testID, maxConnections)

	cfg := client.DefaultConfig()
	connMgr := client.NewConnectionManager(cfg)

	var connections []*client.ParticipantConnection
	var connMu sync.Mutex

	// Metrics per participant
	metricsMap := make(map[uint32]*RTPMetrics)
	var metricsMu sync.Mutex

	k8sMgr := client.NewKubernetesManager(cfg)
	if k8sMgr != nil && k8sMgr.IsAvailable() {
		// Auto-scale connector pods based on requested connections
		participantCount := maxConnections
		if participantCount <= 0 {
			participantCount = 1500 // Default assumption for "all"
		}
		connections = connectKubernetesMode(k8sMgr, connMgr, testID, maxConnections, participantCount, &connMu)
	} else {
		connections = connectLocalMode(connMgr, baseURL, testID, maxConnections, &connMu)
	}

	log.Printf("Successfully connected to %d participants", len(connections))

	if len(connections) == 0 {
		log.Fatal("No successful connections")
	}

	for _, conn := range connections {
		metricsMap[conn.ID] = NewRTPMetrics(conn.ID)
	}
	runStatsLoop(connections, metricsMap, &metricsMu)
}

func printUsage() {
	fmt.Println("Usage: go run webrtc_receiver.go <base_url> <test_id> [max_connections]")
	fmt.Println("")
	fmt.Println("Examples:")
	fmt.Println("  go run webrtc_receiver.go http://localhost:8080 chaos_test_123 all")
	fmt.Println("  go run webrtc_receiver.go http://localhost:8080 chaos_test_123 100")
	fmt.Println("  go run webrtc_receiver.go http://localhost:8080 chaos_test_123 1")
	fmt.Println("")
	fmt.Println("Environment variables:")
	fmt.Println("  CONNECTOR_REPLICAS - Number of connector pods to scale (default: auto)")
	fmt.Println("  TURN_HOST          - TURN server hostname (default: auto-detect)")
	fmt.Println("  TURN_USERNAME      - TURN username (default: webrtc)")
	fmt.Println("  TURN_PASSWORD      - TURN password (default: webrtc123)")
}

func parseMaxConnections() int {
	maxConnections := -1
	if len(os.Args) > 3 && os.Args[3] != "all" {
		fmt.Sscanf(os.Args[3], "%d", &maxConnections)
	}
	return maxConnections
}

func logConnectionMode(testID string, maxConnections int) {
	if maxConnections == -1 {
		log.Printf("Connecting to ALL participants from test %s", testID)
	} else if maxConnections == 1 {
		log.Printf("Single participant mode - connecting to 1 participant from test %s", testID)
	} else {
		log.Printf("Connecting to up to %d participants from test %s", maxConnections, testID)
	}
}

func connectKubernetesMode(k8sMgr *client.KubernetesManager, connMgr *client.ConnectionManager, testID string, maxConnections int, participantCount int, connMu *sync.Mutex) []*client.ParticipantConnection {
	// Auto-scale connector pods based on participant count
	pods, err := k8sMgr.DiscoverPodsWithScale(participantCount)
	if err != nil || len(pods) == 0 {
		log.Fatal("No connector pods found or port-forward failed")
	}
	defer k8sMgr.Cleanup()

	connectionsPerPod := maxConnections
	if maxConnections > 0 {
		connectionsPerPod = (maxConnections + len(pods) - 1) / len(pods) // Round up
	}

	log.Printf("Distributing connections across %d connector pods (~%d per pod)", len(pods), connectionsPerPod)

	var connections []*client.ParticipantConnection
	var wg sync.WaitGroup

	// Connect to all pods concurrently - per-pod concurrency is already limited
	for _, pod := range pods {
		wg.Add(1)
		go func(p *client.PodInfo) {
			defer wg.Done()
			podURL := k8sMgr.GetPodURL(p)
			podConns := connMgr.ConnectToPodParticipants(podURL, testID, p, connectionsPerPod, maxConnections == 1)
			connMu.Lock()
			connections = append(connections, podConns...)
			connMu.Unlock()
			log.Printf("Pod %s: Connected to %d participants", p.Name, len(podConns))
		}(pod)
	}
	wg.Wait()

	return connections
}

func connectLocalMode(connMgr *client.ConnectionManager, baseURL, testID string, maxConnections int, connMu *sync.Mutex) []*client.ParticipantConnection {
	participantIDs := connMgr.FindParticipants(baseURL, testID, maxConnections)
	if len(participantIDs) == 0 {
		log.Fatal("No participants found")
	}

	log.Printf("Found %d participants, connecting concurrently...", len(participantIDs))

	var connections []*client.ParticipantConnection
	var wg sync.WaitGroup
	sem := make(chan struct{}, 50) // Concurrency limit

	for _, participantID := range participantIDs {
		wg.Add(1)
		sem <- struct{}{}
		go func(pid uint32) {
			defer wg.Done()
			defer func() { <-sem }()
			conn, err := connMgr.ConnectToParticipantWithRetry(baseURL, testID, pid, "local", maxConnections == 1)
			if err != nil {
				log.Printf("Failed to connect to participant %d: %v", pid, err)
				return
			}
			connMu.Lock()
			connections = append(connections, conn)
			connMu.Unlock()
		}(participantID)
	}
	wg.Wait()

	return connections
}

func runStatsLoop(connections []*client.ParticipantConnection, metricsMap map[uint32]*RTPMetrics, metricsMu *sync.Mutex) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	for {
		select {
		case <-ticker.C:
			printStatsWithMetrics(connections, metricsMap, metricsMu)
		case <-sigChan:
			log.Println("\nShutting down...")
			client.CloseConnections(connections)
			printStatsWithMetrics(connections, metricsMap, metricsMu)
			return
		}
	}
}

func printStatsWithMetrics(connections []*client.ParticipantConnection, metricsMap map[uint32]*RTPMetrics, metricsMu *sync.Mutex) {
	if len(connections) == 0 {
		log.Println("No connections to display")
		return
	}

	var totalVideo, totalAudio int64
	var totalVideoBytes, totalAudioBytes int64
	connectedCount := 0

	for _, conn := range connections {
		video := conn.VideoPackets.Load()
		audio := conn.AudioPackets.Load()
		totalVideo += video
		totalAudio += audio
		totalVideoBytes += conn.VideoBytes.Load()
		totalAudioBytes += conn.AudioBytes.Load()
		if conn.Connected.Load() {
			connectedCount++
		}
	}

	log.Println("")
	log.Printf("Total Participants: %d", len(connections))
	log.Printf("  Connected: %d (%.1f%%)", connectedCount, float64(connectedCount)*100/float64(len(connections)))
	log.Printf("Total Packets: %d", totalVideo+totalAudio)
	log.Printf("  Video: %d packets (%.2f MB)", totalVideo, float64(totalVideoBytes)/1024/1024)
	log.Printf("  Audio: %d packets (%.2f MB)", totalAudio, float64(totalAudioBytes)/1024/1024)
	log.Println("")
}
