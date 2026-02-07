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

var startTime time.Time

func init() {
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)
	startTime = time.Now()
}

func main() {
	if len(os.Args) < 3 {
		printUsage()
		os.Exit(1)
	}

	baseURL := os.Args[1]
	testID := os.Args[2]
	maxConnections := parseMaxConnections()

	logConnectionMode(testID, maxConnections)

	cfg := client.DefaultConfig()
	connMgr := client.NewConnectionManager(cfg)

	var connections []*client.ParticipantConnection
	var connMu sync.Mutex

	metricsMap := make(map[uint32]*metrics.RTPMetrics)
	var metricsMu sync.Mutex

	k8sMgr := client.NewKubernetesManager(cfg)
	if k8sMgr != nil && k8sMgr.IsAvailable() {
		participantCount := maxConnections
		if participantCount <= 0 {
			participantCount = 1500 // Default assumption for "all"
		}
		connections = connectKubernetesMode(k8sMgr, connMgr, testID, maxConnections, participantCount, &connMu)
	} else {
		connections = connectLocalMode(connMgr, baseURL, testID, maxConnections, &connMu)
	}

	log.Printf("✓ Successfully connected to %d participants", len(connections))

	if len(connections) == 0 {
		log.Fatal("No successful connections")
	}

	for _, conn := range connections {
		metricsMap[conn.ID] = metrics.NewRTPMetrics(conn.ID)
	}
	runStatsLoop(connections, metricsMap, &metricsMu)
}

func printUsage() {
	fmt.Println("Usage: go run ./examples/go/webrtc_receiver.go <base_url> <test_id> [max_connections]")
	fmt.Println("")
	fmt.Println("Examples:")
	fmt.Println("  go run ./examples/go/webrtc_receiver.go http://localhost:8080 chaos_test_123 all")
	fmt.Println("  go run ./examples/go/webrtc_receiver.go http://localhost:8080 chaos_test_123 100")
	fmt.Println("  go run ./examples/go/webrtc_receiver.go http://localhost:8080 chaos_test_123 1")
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
	pods, err := k8sMgr.DiscoverPodsWithScale(participantCount)
	if err != nil || len(pods) == 0 {
		log.Fatal("No connector pods found or port-forward failed")
	}
	defer k8sMgr.Cleanup()

	connectionsPerPod := maxConnections
	if maxConnections > 0 {
		connectionsPerPod = (maxConnections + len(pods) - 1) / len(pods)
	}

	log.Printf("Distributing connections across %d connector pods (~%d per pod)", len(pods), connectionsPerPod)

	var connections []*client.ParticipantConnection
	var wg sync.WaitGroup

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
	sem := make(chan struct{}, 50)

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

	tickCount := 0
	for {
		select {
		case <-ticker.C:
			tickCount++
			printStatsWithMetrics(connections, metricsMap, metricsMu, tickCount)
		case <-sigChan:
			log.Println("\nShutting down...")
			client.CloseConnections(connections)
			printFinalStats(connections, metricsMap, metricsMu)
			return
		}
	}
}

type participantStat struct {
	id     uint32
	video  int64
	audio  int64
	bytes  int64
	active bool
}

func printStatsWithMetrics(connections []*client.ParticipantConnection, metricsMap map[uint32]*metrics.RTPMetrics, metricsMu *sync.Mutex, tickCount int) {
	if len(connections) == 0 {
		log.Println("No connections to display")
		return
	}

	var totalVideo, totalAudio int64
	var totalVideoBytes, totalAudioBytes int64
	connectedCount := 0
	var topParticipants []participantStat

	for _, conn := range connections {
		video := conn.VideoPackets.Load()
		audio := conn.AudioPackets.Load()
		videoBytes := conn.VideoBytes.Load()
		audioBytes := conn.AudioBytes.Load()
		totalVideo += video
		totalAudio += audio
		totalVideoBytes += videoBytes
		totalAudioBytes += audioBytes
		connected := conn.Connected.Load()
		if connected {
			connectedCount++
		}
		topParticipants = append(topParticipants, participantStat{
			id: conn.ID, video: video, audio: audio,
			bytes: videoBytes + audioBytes, active: connected,
		})
	}

	elapsed := time.Since(startTime)
	totalPackets := totalVideo + totalAudio
	totalBytes := totalVideoBytes + totalAudioBytes

	log.Println()
	log.Printf("Connections: %d/%d active (%.1f%%)", connectedCount, len(connections), float64(connectedCount)*100/float64(len(connections)))
	log.Printf("Total Packets: %d (%.1f pkt/s)", totalPackets, float64(totalPackets)/elapsed.Seconds())
	log.Printf("Total Bytes: %.2f MB (%.2f Mbps)", float64(totalBytes)/1024/1024, float64(totalBytes)*8/elapsed.Seconds()/1000000)
	log.Println()
	log.Println("Media Type Breakdown:")
	if totalPackets > 0 {
		log.Printf("  Video (H.264): %d packets (%.1f%%) - %.2f MB", totalVideo, float64(totalVideo)*100/float64(totalPackets), float64(totalVideoBytes)/1024/1024)
		log.Printf("  Audio (Opus):  %d packets (%.1f%%) - %.2f MB", totalAudio, float64(totalAudio)*100/float64(totalPackets), float64(totalAudioBytes)/1024/1024)
	} else {
		log.Printf("  Video (H.264): %d packets", totalVideo)
		log.Printf("  Audio (Opus):  %d packets", totalAudio)
	}

	// Sort by total packets and show top 5
	for i := 0; i < len(topParticipants); i++ {
		for j := i + 1; j < len(topParticipants); j++ {
			if topParticipants[j].video+topParticipants[j].audio > topParticipants[i].video+topParticipants[i].audio {
				topParticipants[i], topParticipants[j] = topParticipants[j], topParticipants[i]
			}
		}
	}
	log.Println()
	log.Println("Top Participants:")
	showCount := 5
	if len(topParticipants) < showCount {
		showCount = len(topParticipants)
	}
	for i := 0; i < showCount; i++ {
		p := topParticipants[i]
		status := "●"
		if !p.active {
			status = "○"
		}
		log.Printf("  %s Participant %d: %d video, %d audio (%.2f KB)", status, p.id, p.video, p.audio, float64(p.bytes)/1024)
	}
}

func printFinalStats(connections []*client.ParticipantConnection, metricsMap map[uint32]*metrics.RTPMetrics, metricsMu *sync.Mutex) {
	if len(connections) == 0 {
		return
	}

	var totalVideo, totalAudio int64
	var totalVideoBytes, totalAudioBytes int64
	connectedCount := 0
	activeParticipants := 0

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
		if video > 0 || audio > 0 {
			activeParticipants++
		}
	}

	elapsed := time.Since(startTime)
	totalPackets := totalVideo + totalAudio
	totalBytes := totalVideoBytes + totalAudioBytes

	log.Println()
	log.Println("                 FINAL WEBRTC STATISTICS                   ")
	log.Printf("Duration: %v", elapsed.Round(time.Second))
	log.Printf("Total Participants: %d", len(connections))
	log.Printf("  Connected at exit: %d (%.1f%%)", connectedCount, float64(connectedCount)*100/float64(len(connections)))
	log.Printf("  Received packets: %d (%.1f%%)", activeParticipants, float64(activeParticipants)*100/float64(len(connections)))
	log.Println()
	log.Printf("Total Packets: %d (%.1f pkt/s avg)", totalPackets, float64(totalPackets)/elapsed.Seconds())
	log.Printf("Total Bytes: %.2f MB (%.2f Mbps avg)", float64(totalBytes)/1024/1024, float64(totalBytes)*8/elapsed.Seconds()/1000000)
	log.Println()
	log.Println("Media Type Breakdown:")
	if totalPackets > 0 {
		log.Printf("  Video (H.264): %d packets (%.1f%%) - %.2f MB", totalVideo, float64(totalVideo)*100/float64(totalPackets), float64(totalVideoBytes)/1024/1024)
		log.Printf("  Audio (Opus):  %d packets (%.1f%%) - %.2f MB", totalAudio, float64(totalAudio)*100/float64(totalPackets), float64(totalAudioBytes)/1024/1024)
	} else {
		log.Printf("  Video (H.264): %d packets - %.2f MB", totalVideo, float64(totalVideoBytes)/1024/1024)
		log.Printf("  Audio (Opus):  %d packets - %.2f MB", totalAudio, float64(totalAudioBytes)/1024/1024)
	}
	log.Println("Note: Server test is still running. To stop it before reconnecting:")
	log.Println("  curl -X POST http://localhost:8080/api/v1/test/<test_id>/stop")
}
