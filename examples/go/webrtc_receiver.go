package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/pion/webrtc/v3"
)

func init() {
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)
}

type SDPResponse struct {
	ParticipantID uint32 `json:"participant_id"`
	SDPOffer      string `json:"sdp_offer"`
}

type SDPAnswerRequest struct {
	SDPAnswer string `json:"sdp_answer"`
}

type ParticipantConnection struct {
	ID             uint32
	PodName        string
	PeerConnection *webrtc.PeerConnection
	VideoPackets   atomic.Int64
	AudioPackets   atomic.Int64
	VideoBytes     atomic.Int64
	AudioBytes     atomic.Int64
	Connected      atomic.Bool
}

type PodInfo struct {
	Name           string
	PartitionID    int
	Participants   []uint32
	PortForwardCmd *exec.Cmd
	LocalPort      int
}

func main() {
	if len(os.Args) < 3 {
		fmt.Println("Usage: go run webrtc_receiver.go <base_url> <test_id> [max_connections]")
		fmt.Println("")
		fmt.Println("Examples:")
		fmt.Println("  go run webrtc_receiver.go http://localhost:8080 chaos_test_123 all")
		fmt.Println("  go run webrtc_receiver.go http://localhost:8080 chaos_test_123 100")
		fmt.Println("  go run webrtc_receiver.go http://localhost:8080 chaos_test_123 1")
		fmt.Println("")
		fmt.Println("For Kubernetes mode, this will automatically:")
		fmt.Println("  1. Discover all orchestrator pods")
		fmt.Println("  2. Port-forward to each pod")
		fmt.Println("  3. Connect to participants in each pod")
		fmt.Println("")
		fmt.Println("Use 'all' for max_connections to connect to all participants (default)")
		fmt.Println("Use '1' for single participant mode (detailed logging)")
		os.Exit(1)
	}

	baseURL := os.Args[1]
	testID := os.Args[2]
	maxConnections := -1 // -1 means all participants
	if len(os.Args) > 3 && os.Args[3] != "all" {
		fmt.Sscanf(os.Args[3], "%d", &maxConnections)
	}

	if maxConnections == -1 {
		log.Printf("Connecting to ALL participants from test %s", testID)
	} else if maxConnections == 1 {
		log.Printf("Single participant mode - connecting to 1 participant from test %s", testID)
	} else {
		log.Printf("Connecting to up to %d participants from test %s", maxConnections, testID)
	}

	// Check if we're in Kubernetes mode
	kubectlCmd, err := exec.LookPath("kubectl")
	kubernetesMode := err == nil

	var pods []*PodInfo
	var cleanupFuncs []func()

	if kubernetesMode {
		log.Printf("Kubernetes mode detected - will use kubectl port-forward")
		pods, cleanupFuncs = setupKubernetesPods(kubectlCmd)
		if len(pods) == 0 {
			log.Fatal("No pods found or port-forward failed")
		}
		defer func() {
			log.Printf("Cleaning up port-forwards...")
			for _, cleanup := range cleanupFuncs {
				cleanup()
			}
		}()
	}

	httpClient := &http.Client{Timeout: 30 * time.Second}

	// Find and connect to participants
	initialCap := 100
	if maxConnections > 0 && maxConnections < initialCap {
		initialCap = maxConnections
	}
	connections := make([]*ParticipantConnection, 0, initialCap)
	var connMu sync.Mutex

	if kubernetesMode {
		// Connect to participants across all pods
		var wg sync.WaitGroup

		connectionsPerPod := maxConnections
		if maxConnections > 0 {
			connectionsPerPod = maxConnections / len(pods)
			if connectionsPerPod == 0 {
				connectionsPerPod = 1
			}
		}

		log.Printf("Connecting to participants across %d pods (max per pod: %s)",
			len(pods),
			func() string {
				if connectionsPerPod < 0 {
					return "all"
				}
				return fmt.Sprintf("%d", connectionsPerPod)
			}())

		for _, pod := range pods {
			wg.Add(1)
			go func(p *PodInfo) {
				defer wg.Done()
				podURL := fmt.Sprintf("http://localhost:%d", p.LocalPort)
				podConns := connectToPodParticipants(httpClient, podURL, testID, p, connectionsPerPod, maxConnections == 1)
				connMu.Lock()
				connections = append(connections, podConns...)
				connMu.Unlock()
				log.Printf("✅ Pod %s: Connected to %d participants", p.Name, len(podConns))
			}(pod)
		}
		wg.Wait()
	} else {
		// Local mode - connect directly
		participantIDs := findParticipants(httpClient, baseURL, testID, maxConnections)
		if len(participantIDs) == 0 {
			log.Fatal("No participants found")
		}

		var wg sync.WaitGroup
		for _, participantID := range participantIDs {
			wg.Add(1)
			go func(pid uint32) {
				defer wg.Done()
				conn, err := connectToParticipant(httpClient, baseURL, testID, pid, "local", maxConnections == 1)
				if err != nil {
					log.Printf("Failed to connect to participant %d: %v", pid, err)
					return
				}
				connMu.Lock()
				connections = append(connections, conn)
				connMu.Unlock()
				log.Printf("✅ Connected to participant %d", pid)
			}(participantID)
			time.Sleep(100 * time.Millisecond)
		}
		wg.Wait()
	}

	log.Printf("Successfully connected to %d participants", len(connections))

	if len(connections) == 0 {
		log.Fatal("No successful connections")
	}

	// Print statistics periodically
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	for {
		select {
		case <-ticker.C:
			printStats(connections)
		case <-sigChan:
			log.Println("\nShutting down...")
			// Cleanup connections
			for _, conn := range connections {
				if conn.PeerConnection != nil {
					conn.PeerConnection.Close()
				}
			}
			printStats(connections)
			return
		}
	}
}

func setupKubernetesPods(kubectlCmd string) ([]*PodInfo, []func()) {
	cmd := exec.Command(kubectlCmd, "get", "pods", "-l", "app=orchestrator", "-o", "jsonpath={range .items[*]}{.metadata.name}{\"\\n\"}{end}")
	output, err := cmd.Output()
	if err != nil {
		log.Printf("Failed to get pods: %v", err)
		return nil, nil
	}

	podNames := []string{}
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			podNames = append(podNames, line)
		}
	}

	if len(podNames) == 0 {
		log.Printf("No orchestrator pods found")
		return nil, nil
	}

	log.Printf("Found %d orchestrator pods", len(podNames))

	pods := make([]*PodInfo, 0, len(podNames))
	cleanupFuncs := make([]func(), 0, len(podNames))
	basePort := 18080

	for i, podName := range podNames {
		localPort := basePort + i

		partitionID := 0
		if parts := strings.Split(podName, "-"); len(parts) > 1 {
			fmt.Sscanf(parts[len(parts)-1], "%d", &partitionID)
		}

		log.Printf("Setting up port-forward for %s (partition %d) on localhost:%d", podName, partitionID, localPort)

		pfCmd := exec.Command(kubectlCmd, "port-forward", fmt.Sprintf("pod/%s", podName), fmt.Sprintf("%d:8080", localPort))
		if err := pfCmd.Start(); err != nil {
			log.Printf("Failed to start port-forward for %s: %v", podName, err)
			continue
		}

		time.Sleep(1 * time.Second)

		testURL := fmt.Sprintf("http://localhost:%d/healthz", localPort)
		resp, err := http.Get(testURL)
		if err != nil || resp.StatusCode != http.StatusOK {
			log.Printf("Port-forward for %s not ready, skipping", podName)
			pfCmd.Process.Kill()
			continue
		}
		resp.Body.Close()

		pod := &PodInfo{
			Name:           podName,
			PartitionID:    partitionID,
			LocalPort:      localPort,
			PortForwardCmd: pfCmd,
		}
		pods = append(pods, pod)

		cleanupFuncs = append(cleanupFuncs, func() {
			if pfCmd.Process != nil {
				pfCmd.Process.Kill()
			}
		})

		log.Printf("✅ Port-forward ready for %s on localhost:%d", podName, localPort)
	}

	return pods, cleanupFuncs
}

func connectToPodParticipants(client *http.Client, podURL, testID string, pod *PodInfo, maxCount int, verbose bool) []*ParticipantConnection {
	participantIDs := findParticipantsInPod(client, podURL, testID, pod.PartitionID, maxCount)
	if len(participantIDs) == 0 {
		log.Printf("No participants found in pod %s", pod.Name)
		return nil
	}

	log.Printf("Found %d participants in pod %s (partition %d)", len(participantIDs), pod.Name, pod.PartitionID)

	connections := make([]*ParticipantConnection, 0, len(participantIDs))
	var wg sync.WaitGroup
	var mu sync.Mutex

	for _, pid := range participantIDs {
		wg.Add(1)
		go func(participantID uint32) {
			defer wg.Done()
			conn, err := connectToParticipant(client, podURL, testID, participantID, pod.Name, verbose)
			if err != nil {
				log.Printf("Failed to connect to participant %d in pod %s: %v", participantID, pod.Name, err)
				return
			}
			mu.Lock()
			connections = append(connections, conn)
			mu.Unlock()
			log.Printf("✅ Connected to participant %d in pod %s", participantID, pod.Name)
		}(pid)
		time.Sleep(200 * time.Millisecond)
	}

	wg.Wait()
	return connections
}

func findParticipantsInPod(client *http.Client, podURL, testID string, partitionID int, maxCount int) []uint32 {
	metricsURL := fmt.Sprintf("%s/api/v1/test/%s/metrics", podURL, testID)
	resp, err := client.Get(metricsURL)
	if err == nil && resp.StatusCode == http.StatusOK {
		defer resp.Body.Close()
		var metrics map[string]interface{}
		if json.NewDecoder(resp.Body).Decode(&metrics) == nil {
			if participants, ok := metrics["participants"].([]interface{}); ok {
				participantIDs := make([]uint32, 0, len(participants))
				for _, p := range participants {
					if pMap, ok := p.(map[string]interface{}); ok {
						if pid, ok := pMap["participant_id"].(float64); ok {
							participantIDs = append(participantIDs, uint32(pid))
							if maxCount > 0 && len(participantIDs) >= maxCount {
								break
							}
						}
					}
				}
				if len(participantIDs) > 0 {
					return participantIDs
				}
			}
		}
	}

	totalPartitions := 10
	healthURL := fmt.Sprintf("%s/healthz", podURL)
	if resp, err := client.Get(healthURL); err == nil && resp.StatusCode == http.StatusOK {
		defer resp.Body.Close()
		var health map[string]interface{}
		if json.NewDecoder(resp.Body).Decode(&health) == nil {
			if tp, ok := health["total_partitions"].(float64); ok {
				totalPartitions = int(tp)
			}
		}
	}

	participantIDs := make([]uint32, 0)
	baseID := uint32(1001)
	searchLimit := 10000
	if maxCount > 0 {
		searchLimit = maxCount * totalPartitions * 2
	}

	log.Printf("Searching for participants in partition %d (totalPartitions=%d)", partitionID, totalPartitions)

	checked := 0
	found := 0

	for i := 0; i < searchLimit; i++ {
		candidateID := baseID + uint32(i)
		if int(candidateID)%totalPartitions != partitionID {
			continue
		}
		checked++

		url := fmt.Sprintf("%s/api/v1/test/%s/sdp/%d", podURL, testID, candidateID)
		resp, err := client.Get(url)
		if err != nil {
			log.Printf("Error checking participant %d: %v", candidateID, err)
			continue
		}

		if resp.StatusCode == http.StatusOK {
			participantIDs = append(participantIDs, candidateID)
			found++
			log.Printf("✓ Found participant %d in partition %d", candidateID, partitionID)
			if maxCount > 0 && len(participantIDs) >= maxCount {
				resp.Body.Close()
				break
			}
		} else if resp.StatusCode != http.StatusNotFound {
			body, _ := io.ReadAll(resp.Body)
			log.Printf("Unexpected status %d for participant %d: %s", resp.StatusCode, candidateID, string(body))
		}
		resp.Body.Close()

		if found == 0 && checked > 100 {
			log.Printf("Checked %d candidates in partition %d, found none. Stopping search.", checked, partitionID)
			break
		}
	}

	log.Printf("Found %d participants for partition %d (checked %d candidates)", len(participantIDs), partitionID, checked)
	return participantIDs
}

func findParticipants(client *http.Client, baseURL, testID string, maxCount int) []uint32 {
	metricsURL := fmt.Sprintf("%s/api/v1/test/%s/metrics", baseURL, testID)
	resp, err := client.Get(metricsURL)
	if err == nil && resp.StatusCode == http.StatusOK {
		defer resp.Body.Close()
		var metrics map[string]interface{}
		if json.NewDecoder(resp.Body).Decode(&metrics) == nil {
			if participants, ok := metrics["participants"].([]interface{}); ok {
				participantIDs := make([]uint32, 0, len(participants))
				for _, p := range participants {
					if pMap, ok := p.(map[string]interface{}); ok {
						if pid, ok := pMap["participant_id"].(float64); ok {
							participantIDs = append(participantIDs, uint32(pid))
							if maxCount > 0 && len(participantIDs) >= maxCount {
								break
							}
						}
					}
				}
				if len(participantIDs) > 0 {
					return participantIDs
				}
			}
		}
	}

	searchRange := maxCount * 20
	if maxCount < 0 {
		searchRange = 10000
	}

	participantIDs := make([]uint32, 0)
	for i := 0; i < searchRange; i++ {
		if maxCount > 0 && len(participantIDs) >= maxCount {
			break
		}
		pid := uint32(1001 + i)
		url := fmt.Sprintf("%s/api/v1/test/%s/sdp/%d", baseURL, testID, pid)
		resp, err := client.Get(url)
		if err != nil {
			continue
		}
		if resp.StatusCode == http.StatusOK {
			participantIDs = append(participantIDs, pid)
		}
		resp.Body.Close()
	}
	return participantIDs
}

func connectToParticipant(client *http.Client, baseURL, testID string, participantID uint32, podName string, verbose bool) (*ParticipantConnection, error) {
	sdpURL := fmt.Sprintf("%s/api/v1/test/%s/sdp/%d", baseURL, testID, participantID)
	resp, err := client.Get(sdpURL)
	if err != nil {
		return nil, fmt.Errorf("failed to get SDP: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("SDP request failed (%d): %s", resp.StatusCode, string(body))
	}

	var sdpResp SDPResponse
	if err := json.NewDecoder(resp.Body).Decode(&sdpResp); err != nil {
		return nil, fmt.Errorf("failed to decode SDP: %w", err)
	}

	config := webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{URLs: []string{"stun:stun.l.google.com:19302"}},
			{URLs: []string{"stun:stun1.l.google.com:19302"}},
		},
	}

	mediaEngine := &webrtc.MediaEngine{}
	if err := mediaEngine.RegisterDefaultCodecs(); err != nil {
		return nil, fmt.Errorf("failed to register codecs: %w", err)
	}

	settingEngine := webrtc.SettingEngine{}
	settingEngine.SetICETimeouts(10*time.Second, 30*time.Second, 5*time.Second)

	api := webrtc.NewAPI(
		webrtc.WithMediaEngine(mediaEngine),
		webrtc.WithSettingEngine(settingEngine),
	)
	pc, err := api.NewPeerConnection(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create peer connection: %w", err)
	}

	conn := &ParticipantConnection{
		ID:             participantID,
		PodName:        podName,
		PeerConnection: pc,
	}

	pc.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		kind := track.Kind().String()
		if verbose {
			log.Printf("Participant %d: Received %s track", participantID, kind)
		}
		for {
			pkt, _, err := track.ReadRTP()
			if err != nil {
				return
			}
			if kind == "video" {
				conn.VideoPackets.Add(1)
				conn.VideoBytes.Add(int64(len(pkt.Payload)))
				if verbose && conn.VideoPackets.Load()%100 == 0 {
					log.Printf("Participant %d: Video packet %d", participantID, conn.VideoPackets.Load())
				}
			} else {
				conn.AudioPackets.Add(1)
				conn.AudioBytes.Add(int64(len(pkt.Payload)))
			}
		}
	})

	connected := make(chan bool, 1)
	pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		if verbose {
			log.Printf("Participant %d: ICE state: %s", participantID, state.String())
		}
		if state == webrtc.ICEConnectionStateConnected {
			conn.Connected.Store(true)
			select {
			case connected <- true:
			default:
			}
		} else if state == webrtc.ICEConnectionStateFailed || state == webrtc.ICEConnectionStateDisconnected {
			conn.Connected.Store(false)
		}
	})

	offer := webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer,
		SDP:  sdpResp.SDPOffer,
	}

	if verbose {
		hasOfferUfrag := strings.Contains(sdpResp.SDPOffer, "a=ice-ufrag:")
		hasOfferPwd := strings.Contains(sdpResp.SDPOffer, "a=ice-pwd:")
		log.Printf("Participant %d: Offer has ice-ufrag=%v, ice-pwd=%v", participantID, hasOfferUfrag, hasOfferPwd)
	}

	if err := pc.SetRemoteDescription(offer); err != nil {
		pc.Close()
		return nil, fmt.Errorf("failed to set remote description: %w", err)
	}

	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		pc.Close()
		return nil, fmt.Errorf("failed to create answer: %w", err)
	}

	if err := pc.SetLocalDescription(answer); err != nil {
		pc.Close()
		return nil, fmt.Errorf("failed to set local description: %w", err)
	}

	gatherComplete := webrtc.GatheringCompletePromise(pc)
	select {
	case <-gatherComplete:
		if verbose {
			log.Printf("Participant %d: ICE gathering complete", participantID)
		}
	case <-time.After(10 * time.Second):
		log.Printf("Participant %d: ICE gathering timeout (continuing anyway)", participantID)
	}

	finalAnswer := pc.LocalDescription()
	if finalAnswer == nil {
		pc.Close()
		return nil, fmt.Errorf("local description is nil after ICE gathering")
	}

	hasUfrag := strings.Contains(finalAnswer.SDP, "a=ice-ufrag:")
	hasPwd := strings.Contains(finalAnswer.SDP, "a=ice-pwd:")
	if !hasUfrag || !hasPwd {
		log.Printf("Participant %d: ERROR - SDP missing ICE credentials!", participantID)
		pc.Close()
		return nil, fmt.Errorf("SDP answer missing ICE credentials")
	}

	answerReq := SDPAnswerRequest{SDPAnswer: finalAnswer.SDP}
	answerBody, err := json.Marshal(answerReq)
	if err != nil {
		pc.Close()
		return nil, fmt.Errorf("failed to marshal answer: %w", err)
	}
	answerURL := fmt.Sprintf("%s/api/v1/test/%s/answer/%d", baseURL, testID, participantID)
	answerResp, err := client.Post(answerURL, "application/json", bytes.NewBuffer(answerBody))
	if err != nil {
		pc.Close()
		return nil, fmt.Errorf("failed to send answer: %w", err)
	}
	defer answerResp.Body.Close()

	if answerResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(answerResp.Body)
		pc.Close()
		return nil, fmt.Errorf("answer request failed: %d - %s", answerResp.StatusCode, string(body))
	}

	log.Printf("Participant %d: Answer accepted by server", participantID)

	select {
	case <-connected:
		return conn, nil
	case <-time.After(15 * time.Second):
		log.Printf("⚠️  Participant %d: ICE connection slow (continuing anyway)", participantID)
		return conn, nil
	}
}

func printStats(connections []*ParticipantConnection) {
	if len(connections) == 0 {
		log.Println("\n═══════════════════════════════════════════════════════════")
		log.Println("                    WEBRTC RECEIVER STATS                   ")
		log.Println("═══════════════════════════════════════════════════════════")
		log.Println("No connections to display")
		log.Println("═══════════════════════════════════════════════════════════\n")
		return
	}

	var totalVideo, totalAudio int64
	var totalVideoBytes, totalAudioBytes int64
	activeConns := 0
	connectedConns := 0

	podStats := make(map[string]struct {
		count        int
		connected    int
		videoPackets int64
		audioPackets int64
		videoBytes   int64
		audioBytes   int64
	})

	type participantStat struct {
		ID           uint32
		PodName      string
		VideoPackets int64
		AudioPackets int64
		TotalPackets int64
	}
	participantStats := make([]participantStat, 0, len(connections))

	log.Println("\n═══════════════════════════════════════════════════════════")
	log.Println("                    WEBRTC RECEIVER STATS                   ")
	log.Println("═══════════════════════════════════════════════════════════")

	for _, conn := range connections {
		video := conn.VideoPackets.Load()
		audio := conn.AudioPackets.Load()
		videoBytes := conn.VideoBytes.Load()
		audioBytes := conn.AudioBytes.Load()
		connected := conn.Connected.Load()

		totalVideo += video
		totalAudio += audio
		totalVideoBytes += videoBytes
		totalAudioBytes += audioBytes

		if video > 0 || audio > 0 {
			activeConns++
			participantStats = append(participantStats, participantStat{
				ID:           conn.ID,
				PodName:      conn.PodName,
				VideoPackets: video,
				AudioPackets: audio,
				TotalPackets: video + audio,
			})
		}
		if connected {
			connectedConns++
		}

		stats := podStats[conn.PodName]
		stats.count++
		stats.videoPackets += video
		stats.audioPackets += audio
		stats.videoBytes += videoBytes
		stats.audioBytes += audioBytes
		if connected {
			stats.connected++
		}
		podStats[conn.PodName] = stats
	}

	totalConns := len(connections)
	log.Printf("\n--- Overall Summary ---")
	log.Printf("Total Participants: %d", totalConns)
	if totalConns > 0 {
		log.Printf("  Connected: %d (%.1f%%)", connectedConns, float64(connectedConns)*100/float64(totalConns))
		log.Printf("  Active (receiving data): %d (%.1f%%)", activeConns, float64(activeConns)*100/float64(totalConns))
	}
	log.Printf("")
	log.Printf("Total Packets: %d", totalVideo+totalAudio)
	log.Printf("  Video: %d packets (%.2f MB)", totalVideo, float64(totalVideoBytes)/1024/1024)
	log.Printf("  Audio: %d packets (%.2f MB)", totalAudio, float64(totalAudioBytes)/1024/1024)
	log.Printf("  Total Data: %.2f MB", float64(totalVideoBytes+totalAudioBytes)/1024/1024)

	log.Println("\n--- Per-Pod Summary ---")
	for podName, stats := range podStats {
		log.Printf("Pod %s:", podName)
		log.Printf("  Participants: %d/%d connected", stats.connected, stats.count)
		log.Printf("  Video: %d packets (%.2f MB)", stats.videoPackets, float64(stats.videoBytes)/1024/1024)
		log.Printf("  Audio: %d packets (%.2f MB)", stats.audioPackets, float64(stats.audioBytes)/1024/1024)
	}

	for i := 0; i < len(participantStats) && i < 10; i++ {
		for j := i + 1; j < len(participantStats); j++ {
			if participantStats[j].TotalPackets > participantStats[i].TotalPackets {
				participantStats[i], participantStats[j] = participantStats[j], participantStats[i]
			}
		}
	}

	if len(participantStats) > 0 {
		log.Println("\n--- Top 10 Participants by Packet Count ---")
		for i := 0; i < len(participantStats) && i < 10; i++ {
			p := participantStats[i]
			log.Printf("  Participant %d [%s]: %d total packets (Video: %d, Audio: %d)",
				p.ID, p.PodName, p.TotalPackets, p.VideoPackets, p.AudioPackets)
		}
	}

	log.Println("═══════════════════════════════════════════════════════════\n")
}
