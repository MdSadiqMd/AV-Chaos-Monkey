package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/pion/webrtc/v3"
)

func logf(format string, args ...interface{}) {
	timestamp := time.Now().Format("2006-01-02T15:04:05.000")
	log.Printf("[%s] %s", timestamp, fmt.Sprintf(format, args...))
}

func init() {
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)
}

type CreateTestRequest struct {
	NumParticipants int         `json:"num_participants"`
	Video           VideoConfig `json:"video"`
	Audio           AudioConfig `json:"audio"`
	DurationSeconds int         `json:"duration_seconds"`
}

type VideoConfig struct {
	Width       int    `json:"width"`
	Height      int    `json:"height"`
	FPS         int    `json:"fps"`
	BitrateKbps int    `json:"bitrate_kbps"`
	Codec       string `json:"codec"`
}

type AudioConfig struct {
	SampleRate  int    `json:"sample_rate"`
	Channels    int    `json:"channels"`
	BitrateKbps int    `json:"bitrate_kbps"`
	Codec       string `json:"codec"`
}

type CreateTestResponse struct {
	TestID       string `json:"test_id"`
	Participants []struct {
		ParticipantID uint32 `json:"participant_id"`
	} `json:"participants"`
}

type SDPResponse struct {
	ParticipantID uint32 `json:"participant_id"`
	SDPOffer      string `json:"sdp_offer"`
}

type SDPAnswerRequest struct {
	SDPAnswer string `json:"sdp_answer"`
}

func main() {
	baseURL := "http://localhost:8080"
	var existingTestID string
	if len(os.Args) > 1 {
		baseURL = os.Args[1]
	}
	if len(os.Args) > 2 {
		existingTestID = os.Args[2]
		log.Printf("Using existing test ID: %s", existingTestID)
	}

	transport := &http.Transport{
		MaxIdleConns:       10,
		IdleConnTimeout:    30 * time.Second,
		DisableKeepAlives:  false,
		DisableCompression: false,
	}
	httpClient := &http.Client{
		Timeout:   60 * time.Second,
		Transport: transport,
	}

	var testID string
	var participantID uint32
	var foundParticipantID string

	// Step 1: Create test or use existing
	if existingTestID != "" {
		testID = existingTestID
		log.Printf("Using existing test: %s", testID)

		log.Printf("Checking server connectivity...")
		maxHealthRetries := 3
		var healthErr error
		var healthResp *http.Response
		for retry := 0; retry < maxHealthRetries; retry++ {
			healthClient := &http.Client{
				Timeout: 5 * time.Second,
			}
			healthResp, healthErr = healthClient.Get(fmt.Sprintf("%s/healthz", baseURL))
			if healthErr == nil && healthResp.StatusCode == http.StatusOK {
				healthResp.Body.Close()
				log.Printf("✅ Server is reachable")
				break
			}
			if healthErr != nil {
				if retry < maxHealthRetries-1 {
					log.Printf("⚠️  Attempt %d/%d: Cannot connect to server: %v, retrying in 2s...", retry+1, maxHealthRetries, healthErr)
					time.Sleep(2 * time.Second)
				} else {
					log.Fatalf("❌ Cannot connect to server at %s after %d attempts: %v\n"+
						"This usually means:\n"+
						"  1. Port-forward is not active (run: kubectl port-forward svc/orchestrator 8080:8080)\n"+
						"  2. Port-forward disconnected (restart it: pkill -f 'kubectl port-forward' && kubectl port-forward svc/orchestrator 8080:8080)\n"+
						"  3. Server is not running\n"+
						"  4. Wrong URL\n"+
						"Quick fix: kubectl port-forward svc/orchestrator 8080:8080 &\n"+
						"To check port-forward: ps aux | grep 'kubectl port-forward'\n"+
						"To check pods: kubectl get pods -l app=orchestrator", baseURL, maxHealthRetries, healthErr)
				}
			} else {
				healthResp.Body.Close()
				if healthResp.StatusCode != http.StatusOK {
					log.Fatalf("❌ Server health check failed (status %d)", healthResp.StatusCode)
				}
			}
		}

		// Skip test info check - go straight to finding a participant via SDP endpoint

		// Try to find a participant that exists in the partition we're connected to
		// Port-forward typically connects to orchestrator-0 (partition 0)
		// Participants are assigned: participantID % totalPartitions == partitionID
		// With 10 partitions, partition 0 gets: 1010, 1020, 1030, ... (IDs where ID % 10 == 0)
		// Partition 1 gets: 1001, 1011, 1021, ... (IDs where ID % 10 == 1)
		log.Printf("Trying to find a participant in the connected partition...")
		found := false
		maxRetries := 2

		// Try participants that would be in partition 0 (the default port-forward target)
		// These are participants where (participantID % 10) == 0
		// Starting from 1010 (1010 % 10 = 0), then 1020, 1030, etc.
		for offset := 0; offset < 10; offset++ {
			tryID := uint32(1010 + offset*10) // 1010, 1020, 1030, ... (all in partition 0)
			testURL := fmt.Sprintf("%s/api/v1/test/%s/sdp/%d", baseURL, testID, tryID)

			for retry := 0; retry < maxRetries; retry++ {
				testResp, err := httpClient.Get(testURL)
				if err == nil {
					if testResp.StatusCode == http.StatusOK {
						foundParticipantID = fmt.Sprintf("%d", tryID)
						found = true
						log.Printf("✅ Found participant %s in connected partition", foundParticipantID)
						testResp.Body.Close()
						break
					} else if testResp.StatusCode == http.StatusNotFound {
						testResp.Body.Close()
						break
					} else {
						log.Printf("⚠️  Participant %d returned status %d, trying next...", tryID, testResp.StatusCode)
						testResp.Body.Close()
						break
					}
				} else if retry < maxRetries-1 {
					log.Printf("⚠️  Attempt %d/%d for participant %d failed: %v, retrying...", retry+1, maxRetries, tryID, err)
					time.Sleep(500 * time.Millisecond)
				} else {
					log.Printf("❌ Participant %d failed after %d attempts: %v", tryID, maxRetries, err)
				}
			}
			if found {
				break
			}
			if !found {
				time.Sleep(100 * time.Millisecond)
			}
		}

		// If not found in partition 0, try other partitions (1-9)
		// This handles cases where port-forward connects to a different pod
		if !found {
			log.Printf("No participants found in partition 0, trying other partitions...")
			for partition := 1; partition < 10 && !found; partition++ {
				// Try first participant in each partition
				// Partition N gets participants where (ID % 10) == N
				// So partition 1 gets 1001, 1011, 1021...
				// Partition 2 gets 1002, 1012, 1022...
				tryID := uint32(1001 + partition - 1) // 1001, 1002, 1003, ... for partitions 1-9
				if partition == 0 {
					tryID = 1010 // Partition 0 gets 1010, 1020, etc.
				}
				testURL := fmt.Sprintf("%s/api/v1/test/%s/sdp/%d", baseURL, testID, tryID)

				for retry := 0; retry < maxRetries; retry++ {
					testResp, err := httpClient.Get(testURL)
					if err == nil {
						if testResp.StatusCode == http.StatusOK {
							foundParticipantID = fmt.Sprintf("%d", tryID)
							found = true
							log.Printf("✅ Found participant %s in partition %d", foundParticipantID, partition)
							testResp.Body.Close()
							break
						}
						testResp.Body.Close()
					}
					if retry < maxRetries-1 {
						time.Sleep(500 * time.Millisecond)
					}
				}
			}
		}

		if !found {
			log.Fatalf("Failed to find any participant in any partition after trying all candidates.\n"+
				"This usually means:\n"+
				"  1. Port-forward is not active (run: kubectl port-forward svc/orchestrator 8080:8080)\n"+
				"  2. Test %s does not exist or has no participants\n"+
				"  3. The test was created with different participant IDs\n"+
				"Try: Use the chaos-test tool to create a test with participants across all partitions", testID)
		}

		// Convert foundParticipantID string to uint32 for participantID
		if foundParticipantID != "" {
			if pid, err := strconv.ParseUint(foundParticipantID, 10, 32); err == nil {
				participantID = uint32(pid)
			} else {
				log.Fatalf("Failed to parse participant ID: %s", foundParticipantID)
			}
		}
	} else {
		// Create new test
		// In Kubernetes mode with partitions, create enough participants to ensure
		// at least one is assigned to the partition handling the request
		// Formula: participant_id % total_partitions == partition_id
		// For 10 partitions, participant 1001 % 10 = 1, so it goes to partition 1
		// Participant 1002 % 10 = 2, goes to partition 2, etc.
		// Participant 1000 % 10 = 0, goes to partition 0
		// So we create 10 participants to ensure at least one per partition
		testReq := CreateTestRequest{
			NumParticipants: 10, // Create enough to ensure at least one in each partition
			Video: VideoConfig{
				Width:       1280,
				Height:      720,
				FPS:         30,
				BitrateKbps: 2500,
				Codec:       "h264",
			},
			Audio: AudioConfig{
				SampleRate:  48000,
				Channels:    1,
				BitrateKbps: 128,
				Codec:       "opus",
			},
			DurationSeconds: 600,
		}

		reqBody, _ := json.Marshal(testReq)
		log.Printf("Creating test with %d participants...", testReq.NumParticipants)
		resp, err := httpClient.Post(baseURL+"/api/v1/test/create", "application/json", bytes.NewBuffer(reqBody))
		if err != nil {
			log.Fatalf("Failed to create test: %v\n"+
				"This may be due to:\n"+
				"  1. Server not running or port-forward not active\n"+
				"  2. Network connectivity issues\n"+
				"  3. Request timeout (try increasing timeout or check server logs)", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusCreated {
			body, _ := io.ReadAll(resp.Body)
			log.Fatalf("Test creation failed with status %d: %s", resp.StatusCode, string(body))
		}

		var testResp CreateTestResponse
		if err := json.NewDecoder(resp.Body).Decode(&testResp); err != nil {
			log.Fatalf("Failed to decode response: %v", err)
		}

		if testResp.TestID == "" {
			log.Fatalf("Test creation failed: no test ID in response")
		}

		if len(testResp.Participants) == 0 {
			// In Kubernetes mode with partitions, the participant might be assigned to a different partition
			// Participant assignment: participantID % totalPartitions == partitionID
			// With 10 partitions: 1001 % 10 = 1 (partition 1), 1002 % 10 = 2 (partition 2), etc.
			// Try to find a participant by querying metrics or use a known participant ID
			log.Printf("Warning: No participants in response. This happens in Kubernetes mode when the participant is assigned to a different partition.")
			log.Printf("Attempting to use participant ID 1001 (assigned to partition 1 in 10-partition mode)...")

			// Use participant 1001 as default - it should exist if test was created
			// In 10-partition mode: 1001 % 10 = 1, so it's in partition 1
			// We'll try to get SDP from that participant
			testResp.Participants = []struct {
				ParticipantID uint32 `json:"participant_id"`
			}{{ParticipantID: 1001}}
			log.Printf("Using participant ID 1001. If this fails, ensure the test was created with enough participants.")
		}

		testID = testResp.TestID
		participantID = testResp.Participants[0].ParticipantID
		log.Printf("Created test: %s, Participant ID: %d", testID, participantID)
	}

	// Step 2: Get SDP offer
	// In Kubernetes mode, we need to get SDP from the correct partition
	sdpURL := fmt.Sprintf("%s/api/v1/test/%s/sdp/%d", baseURL, testID, participantID)
	log.Printf("Getting SDP offer from: %s", sdpURL)
	sdpResp, err := httpClient.Get(sdpURL)
	if err != nil {
		log.Fatalf("Failed to get SDP offer: %v", err)
	}
	defer sdpResp.Body.Close()

	if sdpResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(sdpResp.Body)
		log.Fatalf("Failed to get SDP offer (status %d): %s\n"+
			"This may happen if participant %d is in a different partition.\n"+
			"In Kubernetes mode with 10 partitions:\n"+
			"  - Participant 1001 is in partition 1 (1001 %% 10 = 1)\n"+
			"  - Participant 1002 is in partition 2 (1002 %% 10 = 2)\n"+
			"  - etc.\n"+
			"Try accessing the orchestrator pod that has this participant, or create a test with 10+ participants.",
			sdpResp.StatusCode, string(body), participantID)
	}

	var sdpOfferResp SDPResponse
	if err := json.NewDecoder(sdpResp.Body).Decode(&sdpOfferResp); err != nil {
		log.Fatalf("Failed to decode SDP offer: %v", err)
	}

	log.Printf("Received SDP offer")
	if strings.Contains(sdpOfferResp.SDPOffer, "typ relay") {
		log.Printf("✅ SDP offer contains TURN relay candidates")
	} else {
		log.Printf("⚠️  SDP offer does NOT contain TURN relay candidates - this may cause connection failures")
		log.Printf("   This usually means TURN servers are not accessible from Kubernetes pods")
	}

	// Step 3: Create WebRTC peer connection
	log.Printf("🔌 Creating WebRTC peer connection...")

	// Get TURN server from environment or use default (localhost for docker-compose)
	turnHost := os.Getenv("TURN_HOST")
	if turnHost == "" {
		turnHost = "localhost" // Default to localhost for docker-compose
	}
	turnUsername := os.Getenv("TURN_USERNAME")
	if turnUsername == "" {
		turnUsername = "webrtc" // Default username
	}
	turnPassword := os.Getenv("TURN_PASSWORD")
	if turnPassword == "" {
		turnPassword = "webrtc123" // Default password
	}

	config := webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{URLs: []string{"stun:stun.l.google.com:19302"}},
			{URLs: []string{"stun:stun1.l.google.com:19302"}},
			// Local TURN server (coturn in docker-compose)
			{URLs: []string{fmt.Sprintf("turn:%s:3478", turnHost)}, Username: turnUsername, Credential: turnPassword},
			{URLs: []string{fmt.Sprintf("turn:%s:3478?transport=tcp", turnHost)}, Username: turnUsername, Credential: turnPassword},
			// Fallback to public TURN servers if local server is not available
			{URLs: []string{"turn:openrelay.metered.ca:80"}, Username: "openrelayproject", Credential: "openrelayproject"},
			{URLs: []string{"turn:openrelay.metered.ca:443"}, Username: "openrelayproject", Credential: "openrelayproject"},
		},
		ICETransportPolicy: webrtc.ICETransportPolicyAll,
	}

	peerConnection, err := webrtc.NewPeerConnection(config)
	if err != nil {
		log.Fatalf("Failed to create peer connection: %v", err)
	}
	defer peerConnection.Close()

	var packetStatsMu sync.Mutex
	packetStats := make(map[string]int)
	lastLogTime := time.Now()
	var lastLogTimeMu sync.Mutex

	connectedParticipantID := foundParticipantID
	if connectedParticipantID == "" {
		connectedParticipantID = "unknown"
	}

	peerConnection.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		kind := track.Kind().String()
		log.Printf("✅ Received %s track, SSRC: %d", kind, track.SSRC())
		log.Printf("📡 REAL MEDIA STREAM - Receiving %s packets via WebRTC", kind)
		log.Printf("💡 This is a REAL WebRTC connection with actual RTP packets, not a simulation!")

		// Read RTP packets
		packetCount := 0
		for {
			rtpPacket, _, err := track.ReadRTP()
			if err != nil {
				if err == io.EOF {
					log.Printf("Track %s ended (EOF)", kind)
					return
				}
				log.Printf("Error reading RTP: %v", err)
				continue
			}

			packetCount++

			packetStatsMu.Lock()
			packetStats[kind] = packetCount
			packetStatsMu.Unlock()

			// Extract participant ID - use the participant we connected to
			// WebRTC may rewrite SSRC and strip custom extensions, so we use the known participant ID
			participantID := connectedParticipantID
			if packetCount%100 == 0 || packetCount <= 10 {
				log.Printf("📦 [%s] Packet #%d | Participant: %s | Seq: %d | TS: %d | Payload: %d bytes | SSRC: %d",
					kind, packetCount, participantID, rtpPacket.SequenceNumber, rtpPacket.Timestamp,
					len(rtpPacket.Payload), rtpPacket.SSRC)
			}

			lastLogTimeMu.Lock()
			shouldLog := time.Since(lastLogTime) >= 5*time.Second
			if shouldLog {
				lastLogTime = time.Now()
			}
			lastLogTimeMu.Unlock()

			if shouldLog {
				packetStatsMu.Lock()
				totalPackets := 0
				videoCount := packetStats["video"]
				audioCount := packetStats["audio"]
				for _, count := range packetStats {
					totalPackets += count
				}
				packetStatsMu.Unlock()
				log.Printf("📊 Media Statistics: Total packets received: %d (Video: %d, Audio: %d)",
					totalPackets, videoCount, audioCount)
			}
		}
	})

	peerConnection.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		log.Printf("🔌 ICE Connection State: %s", state.String())
		if state == webrtc.ICEConnectionStateFailed {
			log.Printf("❌ ICE connection failed. This may be due to:")
			log.Printf("   1. Network connectivity issues")
			log.Printf("   2. Firewall blocking UDP traffic")
			log.Printf("   3. STUN server unreachable")
			log.Printf("   4. Server-side WebRTC not fully implemented (SDP only)")
		} else if state == webrtc.ICEConnectionStateConnected {
			log.Printf("✅ ICE connection established! Receiving real media packets...")
		}
	})

	peerConnection.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate != nil {
			candidateStr := candidate.String()
			candidateJSON := candidate.ToJSON()

			// Detect candidate type for better logging
			candidateType := "unknown"
			if strings.Contains(candidateJSON.Candidate, "typ host") {
				candidateType = "host"
			} else if strings.Contains(candidateJSON.Candidate, "typ srflx") {
				candidateType = "srflx"
			} else if strings.Contains(candidateJSON.Candidate, "typ relay") {
				candidateType = "relay"
				log.Printf("🧊 ICE Candidate: %s [%s] ✅ TURN relay candidate!", candidateStr, candidateType)
			} else {
				log.Printf("🧊 ICE Candidate: %s [%s]", candidateStr, candidateType)
			}

			// Send ICE candidate to server
			go func() {
				candidateReq := map[string]interface{}{
					"candidate":     candidateJSON.Candidate,
					"sdpMLineIndex": candidateJSON.SDPMLineIndex,
					"sdpMid":        candidateJSON.SDPMid,
				}
				candidateBody, _ := json.Marshal(candidateReq)

				maxRetries := 2
				for retry := 0; retry < maxRetries; retry++ {
					candidateResp, err := httpClient.Post(
						fmt.Sprintf("%s/api/v1/test/%s/ice/%d", baseURL, testID, participantID),
						"application/json",
						bytes.NewBuffer(candidateBody),
					)
					if err == nil {
						candidateResp.Body.Close()
						if candidateType == "relay" {
							log.Printf("✅ Sent TURN relay candidate to server")
						} else {
							log.Printf("✅ Sent ICE candidate to server")
						}
						return
					}
					if retry < maxRetries-1 {
						time.Sleep(500 * time.Millisecond)
					} else {
						log.Printf("⚠️  Failed to send ICE candidate to server after %d attempts: %v (non-critical)", maxRetries, err)
					}
				}
			}()
		} else {
			log.Printf("🧊 ICE gathering complete")
		}
	})

	// Set remote description (SDP offer)
	offer := webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer,
		SDP:  sdpOfferResp.SDPOffer,
	}

	if err := peerConnection.SetRemoteDescription(offer); err != nil {
		log.Fatalf("Failed to set remote description: %v", err)
	}

	// Create answer
	answer, err := peerConnection.CreateAnswer(nil)
	if err != nil {
		log.Fatalf("Failed to create answer: %v", err)
	}

	if err := peerConnection.SetLocalDescription(answer); err != nil {
		log.Fatalf("Failed to set local description: %v", err)
	}

	log.Printf("Created SDP answer")

	// Step 4: Send SDP answer back (with retry logic)
	log.Printf("Sending SDP answer to server...")
	answerReq := SDPAnswerRequest{
		SDPAnswer: answer.SDP,
	}

	answerBody, _ := json.Marshal(answerReq)

	maxRetries := 3
	var answerResp *http.Response
	var answerErr error
	for retry := 0; retry < maxRetries; retry++ {
		answerResp, answerErr = httpClient.Post(
			fmt.Sprintf("%s/api/v1/test/%s/answer/%d", baseURL, testID, participantID),
			"application/json",
			bytes.NewBuffer(answerBody),
		)
		if answerErr == nil && answerResp.StatusCode == http.StatusOK {
			break
		}
		if answerErr != nil {
			if retry < maxRetries-1 {
				log.Printf("⚠️  Attempt %d/%d: Failed to send SDP answer: %v, retrying in 2s...", retry+1, maxRetries, answerErr)
				time.Sleep(2 * time.Second)
			} else {
				log.Fatalf("❌ Failed to send SDP answer after %d attempts: %v", maxRetries, answerErr)
			}
		} else {
			answerResp.Body.Close()
			if answerResp.StatusCode != http.StatusOK {
				log.Printf("⚠️  Server returned status %d, retrying...", answerResp.StatusCode)
				time.Sleep(2 * time.Second)
			}
		}
	}
	if answerErr != nil {
		log.Fatalf("❌ Failed to send SDP answer: %v", answerErr)
	}
	defer answerResp.Body.Close()

	if answerResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(answerResp.Body)
		log.Fatalf("❌ Server returned error (status %d): %s", answerResp.StatusCode, string(body))
	}

	log.Printf("✅ SDP answer sent successfully, waiting for media...")

	// Step 5: Start test (only if we created a new test, not for existing tests)
	if existingTestID == "" {
		startResp, err := httpClient.Post(fmt.Sprintf("%s/api/v1/test/%s/start", baseURL, testID), "application/json", nil)
		if err != nil {
			log.Printf("⚠️  Failed to start test (may already be running): %v", err)
		} else {
			defer startResp.Body.Close()
			log.Printf("Test started, receiving media...")
		}
	} else {
		log.Printf("Using existing test %s, skipping start (test should already be running)", testID)
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	log.Println("Shutting down...")
}
