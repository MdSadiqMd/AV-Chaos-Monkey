package main

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/MdSadiqMd/AV-Chaos-Monkey/pkg/client"
	"github.com/MdSadiqMd/AV-Chaos-Monkey/pkg/metrics"
)

func init() {
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)
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
	metricsMap := make(map[uint32]*metrics.RTPMetrics)
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
		metricsMap[conn.ID] = metrics.NewRTPMetrics(conn.ID)
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

func runStatsLoop(connections []*client.ParticipantConnection, metricsMap map[uint32]*metrics.RTPMetrics, metricsMu *sync.Mutex) {
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

func printStatsWithMetrics(connections []*client.ParticipantConnection, metricsMap map[uint32]*metrics.RTPMetrics, metricsMu *sync.Mutex) {
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
