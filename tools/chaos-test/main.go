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

// Global cached pod list - populated at startup, reused for all spike injections
var cachedPodNames []string
var cachedKubectlCmd string
var consecutiveFailures int // Track consecutive spike failures for cache refresh

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

	// Initialize pod cache for spike injection
	initPodCache()

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

	// Use separate ticker for metrics display (every 2 seconds)
	metricsTicker := time.NewTicker(2 * time.Second)
	defer metricsTicker.Stop()

	// Use separate ticker for spike injection (at exact intervals)
	// This ensures spikes are evenly spaced regardless of system load
	spikeTicker := time.NewTicker(time.Duration(config.SpikeIntervalSeconds) * time.Second)
	defer spikeTicker.Stop()

	for {
		select {
		case <-sigChan:
			fmt.Println()
			logWarning("Received signal - will cleanup when test completes naturally")
			stopTest(client, config.TestID)
			return
		case <-spikeTicker.C:
			if spikeCount < config.NumSpikes && time.Now().Before(endTime) {
				spikeCount++
				logChaos("Injecting spike %d/%d...", spikeCount, config.NumSpikes)
				injectRandomSpike(client, config, spikeCount)
			}
		case <-metricsTicker.C:
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
			// Add delay between pods to prevent OOM kills from simultaneous startup
			var lastErr error
			successCount := 0
			failedPods := []string{}
			for i, podName := range podNames {
				curlCmd := fmt.Sprintf("curl -s -X POST http://localhost:8080/api/v1/test/%s/start -H 'Content-Type: application/json'", testID)
				cmd := exec.Command(kubectlCmd, "exec", podName, "--", "sh", "-c", curlCmd)
				output, err := cmd.Output()
				if err != nil {
					// Retry once after a short delay
					time.Sleep(500 * time.Millisecond)
					cmd = exec.Command(kubectlCmd, "exec", podName, "--", "sh", "-c", curlCmd)
					output, err = cmd.Output()
					if err != nil {
						// Check if test is actually running despite the error
						verifyCmd := fmt.Sprintf("curl -s http://localhost:8080/api/v1/test/%s", testID)
						verifyExec := exec.Command(kubectlCmd, "exec", podName, "--", "sh", "-c", verifyCmd)
						verifyOutput, verifyErr := verifyExec.Output()
						if verifyErr == nil && strings.Contains(string(verifyOutput), "\"state\":\"running\"") {
							successCount++
							continue
						}
						lastErr = err
						failedPods = append(failedPods, podName)
						logWarning("Pod %s: verification also failed: %v (original error: %v)", podName, verifyErr, err)
						logWarning("Failed to start test on pod %s: %v", podName, err)
						continue
					}
				}

				if string(output) != "" && !strings.Contains(string(output), "started") && !strings.Contains(string(output), "true") {
					verifyCmd := fmt.Sprintf("curl -s http://localhost:8080/api/v1/test/%s", testID)
					verifyExec := exec.Command(kubectlCmd, "exec", podName, "--", "sh", "-c", verifyCmd)
					verifyOutput, _ := verifyExec.Output()
					if !strings.Contains(string(verifyOutput), "\"state\":\"running\"") {
						logWarning("Pod %s: test not running after error %v (response: %s)", podName, err, string(verifyOutput))
						failedPods = append(failedPods, podName)
						continue
					}
				}

				successCount++

				// Add delay between pods to prevent OOM kills
				// This gives each pod time to start its participants before the next one starts
				if i < len(podNames)-1 {
					time.Sleep(200 * time.Millisecond)
				}
			}

			if len(failedPods) > 0 {
				logWarning("Failed to start test on %d pod(s): %v", len(failedPods), failedPods)
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
	// Broadcast stop to all orchestrator pods using kubectl exec
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

			successCount := 0
			for _, podName := range podNames {
				curlCmd := fmt.Sprintf("curl -s -X POST http://localhost:8080/api/v1/test/%s/stop -H 'Content-Type: application/json'", testID)
				cmd := exec.Command(kubectlCmd, "exec", podName, "--", "sh", "-c", curlCmd)
				if _, err := cmd.Output(); err == nil {
					successCount++
				}
			}
			if successCount > 0 {
				logInfo("Stopped test on %d/%d pods", successCount, len(podNames))
				return
			}
		}
	}
	// Fallback to single pod
	client.Post(fmt.Sprintf("/api/v1/test/%s/stop", testID), nil)
}

func injectRandomSpike(_ *utils.HTTPClient, config *Config, spikeNum int) {
	spikeID := fmt.Sprintf("chaos_spike_%d_%d", spikeNum, time.Now().Unix())
	duration := rand.Intn(6) + 2 // 2-8 seconds
	participants := selectParticipants(config.NumParticipants)

	// Use weighted random selection for better distribution
	// Packet loss: 40%, Jitter: 20%, Frame drop: 20%, Bitrate reduce: 20%
	spikeType := rand.Intn(100)
	switch {
	case spikeType < 40: // Packet loss (40%)
		injectPacketLossSpike(config.TestID, spikeID, participants, duration)
	case spikeType < 60: // Jitter (20%)
		injectJitterSpike(config.TestID, spikeID, participants, duration)
	case spikeType < 80: // Frame drop (20%)
		injectFrameDropSpike(config.TestID, spikeID, participants, duration)
	default: // Bitrate reduce (20%)
		injectBitrateReduceSpike(config.TestID, spikeID, participants, duration)
	}
}

// Sending a spike to a single orchestrator pod, this spike will be applied to participants on that pod
func broadcastSpike(testID string, spike SpikeEvent) {
	if len(cachedPodNames) == 0 || cachedKubectlCmd == "" {
		logWarning("Pod cache not initialized, spike may not reach pods")
		return
	}

	spikeJSON, err := json.Marshal(spike)
	if err != nil {
		logWarning("Failed to marshal spike: %v", err)
		return
	}

	podName := cachedPodNames[rand.Intn(len(cachedPodNames))]

	curlCmd := fmt.Sprintf("curl -s --max-time 3 -X POST http://localhost:8080/api/v1/test/%s/spike -H 'Content-Type: application/json' -d @-", testID)
	cmd := exec.Command(cachedKubectlCmd, "exec", "-i", podName, "--", "sh", "-c", curlCmd)
	cmd.Stdin = strings.NewReader(string(spikeJSON))

	done := make(chan error, 1)
	go func() {
		_, err := cmd.Output()
		done <- err
	}()

	select {
	case err := <-done:
		if err != nil {
			consecutiveFailures++
			if consecutiveFailures >= 5 {
				logWarning("Multiple spike failures - kubectl may be overloaded")
				consecutiveFailures = 0
			}
		} else {
			consecutiveFailures = 0
		}
	case <-time.After(4 * time.Second):
		if cmd.Process != nil {
			cmd.Process.Kill()
		}
		consecutiveFailures++
	}
}

// Initialize cached pod list at startup
func initPodCache() {
	kubectlCmd, err := utils.FindCommand("kubectl")
	if err != nil {
		logWarning("kubectl not found, spike injection will be limited")
		return
	}
	cachedKubectlCmd = kubectlCmd

	cmd := exec.Command(kubectlCmd, "get", "pods", "-l", "app=orchestrator", "-o", "jsonpath={range .items[*]}{.metadata.name}{\"\\n\"}{end}")
	output, err := cmd.Output()
	if err != nil {
		logWarning("Failed to get pod list: %v", err)
		return
	}

	cachedPodNames = []string{}
	for line := range strings.SplitSeq(strings.TrimSpace(string(output)), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			cachedPodNames = append(cachedPodNames, line)
		}
	}

	if len(cachedPodNames) > 0 {
		logInfo("Cached %d orchestrator pods for spike injection", len(cachedPodNames))
	} else {
		logWarning("No orchestrator pods found")
	}
}

var lastPortForwardAttempt time.Time
var portForwardCooldown = 30 * time.Second // Don't spam restart attempts

// Checking if port-forward is alive and restarts if needed
func checkAndRestartPortForward(baseURL string) bool {
	client := utils.NewHTTPClient(baseURL)
	if err := client.HealthCheck(); err == nil {
		return true // Port-forward is working
	}

	// Check cooldown - don't spam restart attempts
	if time.Since(lastPortForwardAttempt) < portForwardCooldown {
		return false // Still in cooldown, skip restart attempt
	}
	lastPortForwardAttempt = time.Now()

	// Don't try to restart port-forward - it causes issues
	// Just return false and let the caller use kubectl exec fallback
	logWarning("Port-forward is down - using kubectl exec fallback")
	return false
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

func injectPacketLossSpike(testID, spikeID string, participants []int, duration int) {
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

	broadcastSpike(testID, spike)
	logChaos("Injected packet loss spike: %d%% loss, pattern=%s", lossPercent, pattern)
}

func injectJitterSpike(testID, spikeID string, participants []int, duration int) {
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

	broadcastSpike(testID, spike)
	logChaos("Injected jitter spike: base=%dms, jitter=%dms", baseLatency, jitter)
}

func injectFrameDropSpike(testID, spikeID string, participants []int, duration int) {
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

	broadcastSpike(testID, spike)
	logChaos("Injected frame drop spike: %d%% frames dropped", dropPercent)
}

func injectBitrateReduceSpike(testID, spikeID string, participants []int, duration int) {
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

	broadcastSpike(testID, spike)
	logChaos("Injected bitrate reduction spike: %dkbps (%d%% reduction)", newBitrate, reductionPercent)
}

func displayMetrics(client *utils.HTTPClient, testID string, elapsed time.Duration, startTime, endTime time.Time, spikeCount, numSpikes, numParticipants int) {
	var data []byte
	var err error

	// Use kubectl exec as PRIMARY method - more reliable than port-forward under load
	if len(cachedPodNames) > 0 && cachedKubectlCmd != "" {
		// Pick a random pod to distribute load
		podName := cachedPodNames[rand.Intn(len(cachedPodNames))]
		curlCmd := fmt.Sprintf("curl -s --max-time 2 http://localhost:8080/api/v1/test/%s/metrics", testID)
		execCmd := exec.Command(cachedKubectlCmd, "exec", podName, "--", "sh", "-c", curlCmd)
		data, err = execCmd.Output()
	}

	// Fallback to port-forward if kubectl exec failed
	if err != nil || len(data) == 0 {
		data, err = client.Get(fmt.Sprintf("/api/v1/test/%s/metrics", testID))

		// If port-forward fails, try to restart it (with cooldown)
		if err != nil {
			checkAndRestartPortForward(client.BaseURL)
			// Don't retry immediately - just skip this metrics update
		}
	}

	if err != nil || len(data) == 0 {
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

	state := "running"
	if s, ok := metrics["state"].(string); ok {
		state = s
	}

	showProgress(int(elapsed.Seconds()), int(endTime.Sub(startTime).Seconds()))
	fmt.Printf(" | Remaining: %ds | Spikes: %d/%d", remaining, spikeCount, numSpikes)
	fmt.Println()
	logMetrics("State=%s | Elapsed=%ds | Participants=%d | Frames=%d | Packets=%d",
		state, int(elapsed.Seconds()), numParticipants, totalFrames, totalPackets)
	logMetrics("Bitrate=%.0fkbps | Jitter=%.2fms | Loss=%.2f%% | MOS=%.2f",
		getFloat64(aggregate, "total_bitrate_kbps"), avgJitter, avgLoss*100, getFloat64(aggregate, "avg_mos_score"))
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

func timestamp() string {
	return time.Now().Format("2006-01-02T15:04:05.000")
}

func logInfo(format string, args ...any) {
	fmt.Printf("%s[%s][INFO]%s %s\n", utils.BLUE, timestamp(), utils.NC, fmt.Sprintf(format, args...))
}

func logSuccess(format string, args ...any) {
	fmt.Printf("%s[%s][✓]%s %s\n", utils.GREEN, timestamp(), utils.NC, fmt.Sprintf(format, args...))
}

func logWarning(format string, args ...any) {
	fmt.Printf("%s[%s][⚠]%s %s\n", utils.YELLOW, timestamp(), utils.NC, fmt.Sprintf(format, args...))
}

func logError(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "%s[%s][✗]%s %s\n", utils.RED, timestamp(), utils.NC, fmt.Sprintf(format, args...))
}

func logChaos(format string, args ...any) {
	fmt.Printf("%s[%s][CHAOS]%s %s\n", utils.MAGENTA, timestamp(), utils.NC, fmt.Sprintf(format, args...))
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
	fmt.Printf("%s[%s][METRICS]%s %s\n", utils.CYAN, timestamp(), utils.NC, fmt.Sprintf(format, args...))
}
