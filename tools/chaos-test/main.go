package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/MdSadiqMd/AV-Chaos-Monkey/internal/spike"
	"github.com/MdSadiqMd/AV-Chaos-Monkey/pkg/config"
	"github.com/MdSadiqMd/AV-Chaos-Monkey/pkg/k8s"
	"github.com/MdSadiqMd/AV-Chaos-Monkey/pkg/logging"
	"github.com/MdSadiqMd/AV-Chaos-Monkey/pkg/utils"
)

func main() {
	configPath := flag.String("config", "", "Path to JSON config file")
	flag.Parse()

	if *configPath == "" {
		fmt.Println("Usage: chaos-test -config <config.json>")
		fmt.Println("Example: go run ./tools/chaos-test -config config/config.json")
		os.Exit(1)
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		logging.LogError("Failed to load config: %v", err)
		os.Exit(1)
	}

	logging.LogInfo("Config: %d participants, %ds duration, %d spikes @ %ds interval, strategy=%s",
		cfg.NumParticipants, cfg.DurationSeconds, cfg.Spikes.Count, cfg.Spikes.IntervalSeconds, cfg.SpikeDistribution.Strategy)

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	client := utils.NewHTTPClient(cfg.BaseURL)

	logging.LogInfo("Running pre-flight checks...")
	if err := client.HealthCheck(); err != nil {
		logging.LogError("API server not healthy at %s", cfg.BaseURL)
		os.Exit(1)
	}
	logging.LogSuccess("API server is healthy")

	podCache := k8s.GetPodCache()
	podCache.Initialize()

	logging.LogInfo("Creating test with %d participants...", cfg.NumParticipants)
	if err := createTest(client, cfg, podCache); err != nil {
		logging.LogError("Failed to create test: %v", err)
		os.Exit(1)
	}
	logging.LogSuccess("Test created: %s", cfg.TestID)

	utils.UpdateTargetFilesWithTestID(cfg.TestID)

	logging.LogInfo("Starting test...")
	if err := startTest(client, cfg.TestID, podCache); err != nil {
		logging.LogError("Failed to start test: %v", err)
		os.Exit(1)
	}
	logging.LogSuccess("Test started")

	logging.LogChaos("Test is running! Injecting chaos...")
	runTestLoop(client, cfg, podCache, sigChan)
}

func createTest(client *utils.HTTPClient, cfg *config.Config, podCache *k8s.PodCache) error {
	req := config.CreateTestRequest{
		TestID:             cfg.TestID,
		NumParticipants:    cfg.NumParticipants,
		Video:              cfg.Video,
		Audio:              cfg.Audio,
		DurationSeconds:    cfg.DurationSeconds,
		BackendRTPBasePort: "5000",
	}

	if podCache.IsAvailable() {
		successCount, _ := podCache.ExecOnAllPods(fmt.Sprintf(
			"curl -s -X POST http://localhost:8080/api/v1/test/create -H 'Content-Type: application/json' -d '%s'",
			utils.MustJSON(req)))
		if successCount > 0 {
			logging.LogInfo("Successfully created test on %d pods", successCount)
			return nil
		}
	}

	_, err := client.Post("/api/v1/test/create", req)
	return err
}

func startTest(client *utils.HTTPClient, testID string, podCache *k8s.PodCache) error {
	if podCache.IsAvailable() {
		successCount, _ := podCache.ExecOnAllPods(fmt.Sprintf(
			"curl -s -X POST http://localhost:8080/api/v1/test/%s/start -H 'Content-Type: application/json'", testID))
		if successCount > 0 {
			logging.LogInfo("Successfully started test on %d pods", successCount)
			return nil
		}
	}

	_, err := client.Post(fmt.Sprintf("/api/v1/test/%s/start", testID), nil)
	return err
}

func stopTest(client *utils.HTTPClient, testID string, podCache *k8s.PodCache) {
	if podCache.IsAvailable() {
		successCount, _ := podCache.ExecOnAllPods(fmt.Sprintf(
			"curl -s -X POST http://localhost:8080/api/v1/test/%s/stop -H 'Content-Type: application/json'", testID))
		if successCount > 0 {
			logging.LogInfo("Stopped test on %d pods", successCount)
			return
		}
	}
	client.Post(fmt.Sprintf("/api/v1/test/%s/stop", testID), nil)
}

func runTestLoop(client *utils.HTTPClient, cfg *config.Config, podCache *k8s.PodCache, sigChan chan os.Signal) {
	startTime := time.Now()
	endTime := startTime.Add(time.Duration(cfg.DurationSeconds) * time.Second)
	spikeCount := 0

	metricsTicker := time.NewTicker(time.Duration(cfg.Metrics.DisplayIntervalSeconds) * time.Second)
	defer metricsTicker.Stop()

	spikeTicker := time.NewTicker(time.Duration(cfg.Spikes.IntervalSeconds) * time.Second)
	defer spikeTicker.Stop()

	for {
		select {
		case <-sigChan:
			fmt.Println()
			logging.LogWarning("Received signal - stopping test...")
			stopTest(client, cfg.TestID, podCache)
			return

		case <-spikeTicker.C:
			if spikeCount < cfg.Spikes.Count && time.Now().Before(endTime) {
				spikeCount++
				logging.LogChaos("Injecting spike %d/%d...", spikeCount, cfg.Spikes.Count)
				spike.InjectRandomSpike(cfg, podCache, spikeCount)
			}

		case <-metricsTicker.C:
			currentTime := time.Now()
			if currentTime.After(endTime) {
				fmt.Println()
				logging.LogInfo("Test duration completed. Stopping test...")
				stopTest(client, cfg.TestID, podCache)
				utils.CleanupTargetFiles()
				logging.LogSuccess("Chaos test completed!")
				fmt.Printf("Total spikes injected: %d\n", spikeCount)
				fmt.Printf("Test ID: %s\n", cfg.TestID)
				return
			}

			elapsed := currentTime.Sub(startTime)
			displayMetrics(client, cfg, podCache, elapsed, startTime, endTime, spikeCount)
		}
	}
}

func displayMetrics(client *utils.HTTPClient, cfg *config.Config, podCache *k8s.PodCache, elapsed time.Duration, startTime, endTime time.Time, spikeCount int) {
	var data []byte
	var err error

	if podCache.IsAvailable() {
		podNames := podCache.GetPodNames()
		if len(podNames) > 0 {
			podName := podNames[rand.Intn(len(podNames))]
			data, err = podCache.ExecOnPod(podName, fmt.Sprintf(
				"curl -s --max-time 2 http://localhost:8080/api/v1/test/%s/metrics", cfg.TestID))
		}
	}

	if err != nil || len(data) == 0 {
		data, _ = client.Get(fmt.Sprintf("/api/v1/test/%s/metrics", cfg.TestID))
	}

	if len(data) == 0 {
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

	totalFrames := int(utils.GetFloat64(aggregate, "total_frames_sent"))
	totalPackets := int(utils.GetFloat64(aggregate, "total_packets_sent"))
	avgJitter := utils.GetFloat64(aggregate, "avg_jitter_ms")
	avgLoss := utils.GetFloat64(aggregate, "avg_packet_loss")
	remaining := max(int(time.Until(endTime).Seconds()), 0)

	utils.ShowProgress(int(elapsed.Seconds()), int(endTime.Sub(startTime).Seconds()))
	fmt.Printf(" | Remaining: %ds | Spikes: %d/%d", remaining, spikeCount, cfg.Spikes.Count)
	fmt.Println()
	logging.LogMetrics("Frames=%d | Packets=%d | Bitrate=%.0fkbps", totalFrames, totalPackets, utils.GetFloat64(aggregate, "total_bitrate_kbps"))
	logging.LogMetrics("Jitter=%.2fms | Loss=%.2f%% | MOS=%.2f", avgJitter, avgLoss, utils.GetFloat64(aggregate, "avg_mos_score"))
}
