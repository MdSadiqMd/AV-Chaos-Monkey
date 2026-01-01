package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/MdSadiqMd/AV-Chaos-Monkey/pkg/utils"
)

type Config struct {
	NumParticipants      int
	TestDurationSeconds  int
	NumSpikes            int
	SpikeIntervalSeconds int
	BaseURL              string
	TestID               string
}

type CreateTestRequest struct {
	TestID             string      `json:"test_id"`
	NumParticipants    int         `json:"num_participants"`
	Video              VideoConfig `json:"video"`
	Audio              AudioConfig `json:"audio"`
	DurationSeconds    int         `json:"duration_seconds"`
	BackendRTPBasePort string      `json:"backend_rtp_base_port"`
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

type SpikeEvent struct {
	SpikeID         string         `json:"spike_id"`
	Type            string         `json:"type"`
	ParticipantIDs  []int          `json:"participant_ids"`
	DurationSeconds int            `json:"duration_seconds"`
	Params          map[string]any `json:"params"`
}

func main() {
	config := parseFlags()

	// Setup signal handling
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	client := utils.NewHTTPClient(config.BaseURL)

	// Pre-flight checks
	logInfo("Running pre-flight checks...")
	if err := client.HealthCheck(); err != nil {
		logError("API server is not healthy at %s", config.BaseURL)
		os.Exit(1)
	}
	logSuccess("API server is healthy")

	// Create test
	logInfo("Creating test with %d participants...", config.NumParticipants)
	if err := createTest(client, config); err != nil {
		logError("Failed to create test: %v", err)
		os.Exit(1)
	}
	logSuccess("Test created: %s", config.TestID)

	// Update target files with test ID
	updateTargetFilesWithTestID(config.TestID)

	// Start test
	logInfo("Starting test...")
	if err := startTest(client, config.TestID); err != nil {
		logError("Failed to start test: %v", err)
		os.Exit(1)
	}
	logSuccess("Test started")

	// Run test loop
	logChaos("Test is running! Injecting chaos...")
	startTime := time.Now()
	endTime := startTime.Add(time.Duration(config.TestDurationSeconds) * time.Second)
	spikeCount := 0
	nextSpikeTime := startTime.Add(time.Duration(config.SpikeIntervalSeconds) * time.Second)

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-sigChan:
			fmt.Println()
			logWarning("Received signal - will cleanup when test completes naturally")
			stopTest(client, config.TestID)
			return
		case <-ticker.C:
			currentTime := time.Now()
			if currentTime.After(endTime) {
				fmt.Println()
				logInfo("Test duration completed. Stopping test...")
				stopTest(client, config.TestID)

				cleanupTargetFiles()

				logSuccess("Chaos test completed!")
				fmt.Printf("Total spikes injected: %d\n", spikeCount)
				fmt.Printf("Test ID: %s\n", config.TestID)
				return
			}

			// Inject spike if it's time
			if currentTime.After(nextSpikeTime) && spikeCount < config.NumSpikes {
				spikeCount++
				logChaos("Injecting spike %d/%d...", spikeCount, config.NumSpikes)
				injectRandomSpike(client, config, spikeCount)
				nextSpikeTime = currentTime.Add(time.Duration(config.SpikeIntervalSeconds) * time.Second)
			}

			// Display metrics with progress bar
			elapsed := currentTime.Sub(startTime)
			displayMetrics(client, config.TestID, elapsed, startTime, endTime, spikeCount, config.NumSpikes, config.NumParticipants)
		}
	}
}

func parseFlags() *Config {
	var (
		numParticipants = flag.Int("participants", 50, "Number of participants")
		testDuration    = flag.Int("duration", 600, "Test duration in seconds")
		numSpikes       = flag.Int("spikes", 70, "Number of spikes to inject")
		spikeInterval   = flag.Int("spike-interval", 5, "Interval between spikes in seconds")
		baseURL         = flag.String("url", "http://localhost:8080", "Base URL of orchestrator")
	)
	flag.Parse()

	if env := os.Getenv("NUM_PARTICIPANTS"); env != "" {
		if n, err := strconv.Atoi(env); err == nil {
			numParticipants = &n
		}
	}
	if env := os.Getenv("TEST_DURATION_SECONDS"); env != "" {
		if n, err := strconv.Atoi(env); err == nil {
			testDuration = &n
		}
	}
	if env := os.Getenv("BASE_URL"); env != "" {
		baseURL = &env
	}

	testID := fmt.Sprintf("chaos_test_%d", time.Now().Unix())

	return &Config{
		NumParticipants:      *numParticipants,
		TestDurationSeconds:  *testDuration,
		NumSpikes:            *numSpikes,
		SpikeIntervalSeconds: *spikeInterval,
		BaseURL:              *baseURL,
		TestID:               testID,
	}
}

func createTest(client *utils.HTTPClient, config *Config) error {
	req := CreateTestRequest{
		TestID:          config.TestID,
		NumParticipants: config.NumParticipants,
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
		DurationSeconds:    config.TestDurationSeconds,
		BackendRTPBasePort: "5000",
	}

	// Broadcast test creation to all orchestrator pods using kubectl exec
	// This works from outside the cluster by executing curl inside each pod
	kubectlCmd, err := utils.FindCommand("kubectl")
	if err == nil {
		cmd := exec.Command(kubectlCmd, "get", "pods", "-l", "app=orchestrator", "-o", "jsonpath={range .items[*]}{.metadata.name}{\"\\n\"}{end}")
		output, err := cmd.Output()
		if err == nil {
			podNames := []string{}
			for line := range strings.SplitSeq(strings.TrimSpace(string(output)), "\n") {
				line = strings.TrimSpace(line)
				if line != "" {
					podNames = append(podNames, line)
				}
			}

			reqJSON, err := json.Marshal(req)
			if err != nil {
				return fmt.Errorf("failed to marshal request: %w", err)
			}

			// Send request to all pods using kubectl exec
			var lastErr error
			successCount := 0
			for _, podName := range podNames {
				// Use kubectl exec to run curl inside the pod with JSON from stdin
				// This avoids shell escaping issues
				curlCmd := "curl -s -X POST http://localhost:8080/api/v1/test/create -H 'Content-Type: application/json' -d @-"
				cmd := exec.Command(kubectlCmd, "exec", "-i", podName, "--", "sh", "-c", curlCmd)
				cmd.Stdin = strings.NewReader(string(reqJSON))
				output, err := cmd.Output()
				if err != nil {
					lastErr = err
					logWarning("Failed to create test on pod %s: %v", podName, err)
				} else {
					successCount++
					// Check if response indicates success
					if strings.Contains(string(output), "test_id") || strings.Contains(string(output), config.TestID) {
						// Success
					} else if string(output) != "" {
						logWarning("Pod %s returned unexpected response: %s", podName, string(output))
					}
				}
			}

			if successCount > 0 {
				logInfo("Successfully created test on %d/%d pods", successCount, len(podNames))
				return nil
			}
			if lastErr != nil {
				return fmt.Errorf("failed to create test on any pod: %w", lastErr)
			}
		}
	}

	// Fallback to single pod (original behavior)
	_, err = client.Post("/api/v1/test/create", req)
	return err
}

func startTest(client *utils.HTTPClient, testID string) error {
	// Broadcast start to all orchestrator pods using kubectl exec
	kubectlCmd, err := utils.FindCommand("kubectl")
	if err == nil {
		cmd := exec.Command(kubectlCmd, "get", "pods", "-l", "app=orchestrator", "-o", "jsonpath={range .items[*]}{.metadata.name}{\"\\n\"}{end}")
		output, err := cmd.Output()
		if err == nil {
			podNames := []string{}
			for line := range strings.SplitSeq(strings.TrimSpace(string(output)), "\n") {
				line = strings.TrimSpace(line)
				if line != "" {
					podNames = append(podNames, line)
				}
			}

			// Send request to all pods using kubectl exec
			var lastErr error
			successCount := 0
			for _, podName := range podNames {
				curlCmd := fmt.Sprintf("curl -s -X POST http://localhost:8080/api/v1/test/%s/start -H 'Content-Type: application/json'", testID)
				cmd := exec.Command(kubectlCmd, "exec", podName, "--", "sh", "-c", curlCmd)
				output, err := cmd.Output()
				if err != nil {
					lastErr = err
					logWarning("Failed to start test on pod %s: %v", podName, err)
				} else {
					successCount++
					if string(output) != "" && !strings.Contains(string(output), "started") && !strings.Contains(string(output), "true") {
						logWarning("Pod %s returned unexpected response: %s", podName, string(output))
					}
				}
			}

			if successCount > 0 {
				logInfo("Successfully started test on %d/%d pods", successCount, len(podNames))
				return nil
			}
			if lastErr != nil {
				return fmt.Errorf("failed to start test on any pod: %w", lastErr)
			}
		}
	}

	// Fallback to single pod
	_, err = client.Post(fmt.Sprintf("/api/v1/test/%s/start", testID), nil)
	return err
}

func stopTest(client *utils.HTTPClient, testID string) {
	client.Post(fmt.Sprintf("/api/v1/test/%s/stop", testID), nil)
}

func injectRandomSpike(client *utils.HTTPClient, config *Config, spikeNum int) {
	spikeID := fmt.Sprintf("chaos_spike_%d_%d", spikeNum, time.Now().Unix())
	duration := rand.Intn(6) + 2 // 2-8 seconds
	participants := selectParticipants(config.NumParticipants)

	spikeType := rand.Intn(5)
	switch spikeType {
	case 0, 1: // Packet loss (30%)
		injectPacketLossSpike(client, config.TestID, spikeID, participants, duration)
	case 2: // Jitter (25%)
		injectJitterSpike(client, config.TestID, spikeID, participants, duration)
	case 3: // Frame drop (20%)
		injectFrameDropSpike(client, config.TestID, spikeID, participants, duration)
	case 4: // Bitrate reduce (15%)
		injectBitrateReduceSpike(client, config.TestID, spikeID, participants, duration)
	}
}

func selectParticipants(total int) []int {
	r := rand.Intn(100)
	if r < 40 {
		// Single participant
		return []int{rand.Intn(total) + 1001}
	} else if r < 75 {
		// Multiple participants (2-5)
		count := rand.Intn(4) + 2
		participants := make([]int, count)
		for i := range count {
			participants[i] = rand.Intn(total) + 1001
		}
		return participants
	}
	// All participants (empty array)
	return []int{}
}

func injectPacketLossSpike(client *utils.HTTPClient, testID, spikeID string, participants []int, duration int) {
	lossPercent := rand.Intn(25) + 1
	pattern := "random"
	if rand.Intn(2) == 1 {
		pattern = "burst"
	}

	spike := SpikeEvent{
		SpikeID:         spikeID,
		Type:            "rtp_packet_loss",
		ParticipantIDs:  participants,
		DurationSeconds: duration,
		Params: map[string]any{
			"loss_percentage": fmt.Sprintf("%d", lossPercent),
			"pattern":         pattern,
		},
	}

	client.Post(fmt.Sprintf("/api/v1/test/%s/spike", testID), spike)
	logChaos("Injected packet loss spike: %d%% loss, pattern=%s", lossPercent, pattern)
}

func injectJitterSpike(client *utils.HTTPClient, testID, spikeID string, participants []int, duration int) {
	baseLatency := rand.Intn(40) + 10
	jitter := rand.Intn(180) + 20

	spike := SpikeEvent{
		SpikeID:         spikeID,
		Type:            "network_jitter",
		ParticipantIDs:  participants,
		DurationSeconds: duration,
		Params: map[string]any{
			"base_latency_ms":   fmt.Sprintf("%d", baseLatency),
			"jitter_std_dev_ms": fmt.Sprintf("%d", jitter),
		},
	}

	client.Post(fmt.Sprintf("/api/v1/test/%s/spike", testID), spike)
	logChaos("Injected jitter spike: base=%dms, jitter=%dms", baseLatency, jitter)
}

func injectFrameDropSpike(client *utils.HTTPClient, testID, spikeID string, participants []int, duration int) {
	dropPercent := rand.Intn(50) + 10

	spike := SpikeEvent{
		SpikeID:         spikeID,
		Type:            "frame_drop",
		ParticipantIDs:  participants,
		DurationSeconds: duration,
		Params: map[string]any{
			"drop_percentage": fmt.Sprintf("%d", dropPercent),
		},
	}

	client.Post(fmt.Sprintf("/api/v1/test/%s/spike", testID), spike)
	logChaos("Injected frame drop spike: %d%% frames dropped", dropPercent)
}

func injectBitrateReduceSpike(client *utils.HTTPClient, testID, spikeID string, participants []int, duration int) {
	reductionPercent := rand.Intn(50) + 30
	newBitrate := 2500 * (100 - reductionPercent) / 100
	transition := rand.Intn(5) + 1

	spike := SpikeEvent{
		SpikeID:         spikeID,
		Type:            "bitrate_reduce",
		ParticipantIDs:  participants,
		DurationSeconds: duration,
		Params: map[string]any{
			"new_bitrate_kbps":   fmt.Sprintf("%d", newBitrate),
			"transition_seconds": fmt.Sprintf("%d", transition),
		},
	}

	client.Post(fmt.Sprintf("/api/v1/test/%s/spike", testID), spike)
	logChaos("Injected bitrate reduction spike: %dkbps (%d%% reduction)", newBitrate, reductionPercent)
}

func displayMetrics(client *utils.HTTPClient, testID string, elapsed time.Duration, startTime, endTime time.Time, spikeCount, numSpikes, numParticipants int) {
	data, err := client.Get(fmt.Sprintf("/api/v1/test/%s/metrics", testID))
	if err != nil {
		return
	}

	var metrics map[string]any
	if err := json.Unmarshal(data, &metrics); err != nil {
		return
	}

	aggregate, ok := metrics["aggregate"].(map[string]any)
	if !ok {
		return
	}

	// Safely extract values with nil checks
	getFloat64 := func(m map[string]any, key string) float64 {
		if val, ok := m[key]; ok && val != nil {
			if f, ok := val.(float64); ok {
				return f
			}
		}
		return 0
	}

	totalFrames := int(getFloat64(aggregate, "total_frames_sent"))
	totalPackets := int(getFloat64(aggregate, "total_packets_sent"))
	avgJitter := getFloat64(aggregate, "avg_jitter_ms")
	avgLoss := getFloat64(aggregate, "avg_packet_loss")

	remaining := max(int(time.Until(endTime).Seconds()), 0)
	fmt.Print("\033[2K\r")

	showProgress(int(elapsed.Seconds()), int(endTime.Sub(startTime).Seconds()))
	fmt.Printf(" | Remaining: %ds | Spikes: %d/%d", remaining, spikeCount, numSpikes)

	state := "running"
	if s, ok := metrics["state"].(string); ok {
		state = s
	}

	fmt.Println()
	logMetrics("State: %s%s%s | Elapsed: %ds | Participants: %d",
		utils.BOLD, state, utils.NC, int(elapsed.Seconds()), numParticipants)
	logMetrics("Frames: %s%d%s | Packets: %s%d%s | Bitrate: %s%.0fkbps%s",
		utils.BOLD, totalFrames, utils.NC,
		utils.BOLD, totalPackets, utils.NC,
		utils.BOLD, getFloat64(aggregate, "total_bitrate_kbps"), utils.NC)
	logMetrics("Avg Jitter: %s%.2fms%s | Avg Loss: %s%.2f%%%s | Avg MOS: %s%.2f%s",
		utils.BOLD, avgJitter, utils.NC,
		utils.BOLD, avgLoss*100, utils.NC,
		utils.BOLD, getFloat64(aggregate, "avg_mos_score"), utils.NC)
}

func showProgress(current, total int) {
	width := 50
	percentage := current * 100 / total
	if total == 0 {
		percentage = 0
	}
	filled := current * width / total
	if total == 0 {
		filled = 0
	}
	empty := width - filled

	fmt.Printf("\r%s[%s%s%s] %s%d%%%s",
		utils.CYAN,
		strings.Repeat("█", filled),
		strings.Repeat("░", empty),
		utils.CYAN,
		utils.BOLD, percentage, utils.NC)
}

func logInfo(format string, args ...any) {
	fmt.Printf("%s[INFO]%s %s\n", utils.BLUE, utils.NC, fmt.Sprintf(format, args...))
}

func logSuccess(format string, args ...any) {
	fmt.Printf("%s[✓]%s %s\n", utils.GREEN, utils.NC, fmt.Sprintf(format, args...))
}

func logWarning(format string, args ...any) {
	fmt.Printf("%s[⚠]%s %s\n", utils.YELLOW, utils.NC, fmt.Sprintf(format, args...))
}

func logError(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "%s[✗]%s %s\n", utils.RED, utils.NC, fmt.Sprintf(format, args...))
}

func logChaos(format string, args ...any) {
	fmt.Printf("%s[CHAOS]%s %s\n", utils.MAGENTA, utils.NC, fmt.Sprintf(format, args...))
}

func updateTargetFilesWithTestID(testID string) {
	projectRoot := getProjectRoot()
	targetsDir := filepath.Join(projectRoot, "config", "prometheus", "targets")

	files, err := os.ReadDir(targetsDir)
	if err != nil {
		return
	}

	for _, f := range files {
		if !strings.HasSuffix(f.Name(), ".json") {
			continue
		}

		filePath := filepath.Join(targetsDir, f.Name())
		content, err := os.ReadFile(filePath)
		if err != nil {
			continue
		}

		var targets []map[string]any
		if err := json.Unmarshal(content, &targets); err != nil {
			continue
		}

		for _, target := range targets {
			if labels, ok := target["labels"].(map[string]any); ok {
				labels["test_id"] = testID
			}
		}

		updatedJSON, err := json.MarshalIndent(targets, "", "  ")
		if err != nil {
			continue
		}

		os.WriteFile(filePath, updatedJSON, 0644)
	}
}

func cleanupTargetFiles() {
	projectRoot := getProjectRoot()
	targetsDir := filepath.Join(projectRoot, "config", "prometheus", "targets")

	files, err := os.ReadDir(targetsDir)
	if err != nil {
		return
	}

	for _, f := range files {
		if strings.HasSuffix(f.Name(), ".json") {
			os.Remove(filepath.Join(targetsDir, f.Name()))
		}
	}
}

func getProjectRoot() string {
	dir, _ := os.Getwd()
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "."
}

func logMetrics(format string, args ...any) {
	fmt.Printf("%s[METRICS]%s %s\n", utils.CYAN, utils.NC, fmt.Sprintf(format, args...))
}
