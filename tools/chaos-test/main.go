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

	"github.com/MdSadiqMd/AV-Chaos-Monkey/pkg/config"
	"github.com/MdSadiqMd/AV-Chaos-Monkey/pkg/constants"
	"github.com/MdSadiqMd/AV-Chaos-Monkey/pkg/k8s"
	"github.com/MdSadiqMd/AV-Chaos-Monkey/pkg/logging"
	"github.com/MdSadiqMd/AV-Chaos-Monkey/pkg/utils"
)

func main() {
	configPath := flag.String("config", "", "Path to JSON config file")
	flag.Parse()

	if *configPath == "" {
		fmt.Println("Usage: chaos-test -config <config.json>")
		fmt.Println("Example: go run tools/chaos-test/main.go -config config/config.json")
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

	// Initialize pod cache for spike injection
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
			mustJSON(req)))
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
				injectRandomSpike(cfg, podCache, spikeCount)
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

func injectRandomSpike(cfg *config.Config, podCache *k8s.PodCache, spikeNum int) {
	spikeID := fmt.Sprintf("chaos_spike_%d_%d", spikeNum, time.Now().Unix())
	duration := rand.Intn(cfg.Spikes.DurationSecondsMax-cfg.Spikes.DurationSecondsMin+1) + cfg.Spikes.DurationSecondsMin
	participants := selectParticipants(cfg)

	totalWeight := 0
	for _, st := range cfg.Spikes.Types {
		totalWeight += st.Weight
	}

	r := rand.Intn(totalWeight)
	cumulative := 0
	for spikeType, st := range cfg.Spikes.Types {
		cumulative += st.Weight
		if r < cumulative {
			switch spikeType {
			case "rtp_packet_loss":
				injectPacketLossSpike(cfg, podCache, spikeID, participants, duration)
			case "network_jitter":
				injectJitterSpike(cfg, podCache, spikeID, participants, duration)
			case "frame_drop":
				injectFrameDropSpike(cfg, podCache, spikeID, participants, duration)
			case "bitrate_reduce":
				injectBitrateReduceSpike(cfg, podCache, spikeID, participants, duration)
			}
			return
		}
	}

	injectPacketLossSpike(cfg, podCache, spikeID, participants, duration)
}

func selectParticipants(cfg *config.Config) []int {
	sel := cfg.Spikes.ParticipantSelection
	totalWeight := sel.SingleWeight + sel.MultipleWeight + sel.AllWeight
	r := rand.Intn(totalWeight)

	if r < sel.SingleWeight {
		return []int{rand.Intn(cfg.NumParticipants) + 1001}
	} else if r < sel.SingleWeight+sel.MultipleWeight {
		countRange := sel.MultipleCountMax - sel.MultipleCountMin + 1
		count := rand.Intn(countRange) + sel.MultipleCountMin
		participants := make([]int, count)
		for i := range count {
			participants[i] = rand.Intn(cfg.NumParticipants) + 1001
		}
		return participants
	}
	return []int{} // All participants
}

func injectPacketLossSpike(cfg *config.Config, podCache *k8s.PodCache, spikeID string, participants []int, duration int) {
	params := cfg.Spikes.Types["rtp_packet_loss"].Params
	minLoss := config.GetIntParam(params, "loss_percentage_min", 1)
	maxLoss := config.GetIntParam(params, "loss_percentage_max", 25)
	lossPercent := rand.Intn(maxLoss-minLoss+1) + minLoss

	pattern := "random"
	if patterns, ok := params["patterns"].([]any); ok && len(patterns) > 0 {
		pattern = patterns[rand.Intn(len(patterns))].(string)
	} else if rand.Intn(2) == 1 {
		pattern = "burst"
	}

	spike := config.SpikeEvent{
		SpikeID:         spikeID,
		Type:            "rtp_packet_loss",
		ParticipantIDs:  participants,
		DurationSeconds: duration,
		Params: map[string]any{
			"loss_percentage": fmt.Sprintf("%d", lossPercent),
			"pattern":         pattern,
		},
	}

	podCache.BroadcastSpike(cfg.TestID, spike)
	logging.LogChaos("Injected packet loss spike: %d%% loss, pattern=%s", lossPercent, pattern)
}

func injectJitterSpike(cfg *config.Config, podCache *k8s.PodCache, spikeID string, participants []int, duration int) {
	params := cfg.Spikes.Types["network_jitter"].Params
	minLatency := config.GetIntParam(params, "base_latency_ms_min", 10)
	maxLatency := config.GetIntParam(params, "base_latency_ms_max", 50)
	minJitter := config.GetIntParam(params, "jitter_std_dev_ms_min", 20)
	maxJitter := config.GetIntParam(params, "jitter_std_dev_ms_max", 200)

	baseLatency := rand.Intn(maxLatency-minLatency+1) + minLatency
	jitter := rand.Intn(maxJitter-minJitter+1) + minJitter

	spike := config.SpikeEvent{
		SpikeID:         spikeID,
		Type:            "network_jitter",
		ParticipantIDs:  participants,
		DurationSeconds: duration,
		Params: map[string]any{
			"base_latency_ms":   fmt.Sprintf("%d", baseLatency),
			"jitter_std_dev_ms": fmt.Sprintf("%d", jitter),
		},
	}

	podCache.BroadcastSpike(cfg.TestID, spike)
	logging.LogChaos("Injected jitter spike: base=%dms, jitter=%dms", baseLatency, jitter)
}

func injectFrameDropSpike(cfg *config.Config, podCache *k8s.PodCache, spikeID string, participants []int, duration int) {
	params := cfg.Spikes.Types["frame_drop"].Params
	minDrop := config.GetIntParam(params, "drop_percentage_min", 10)
	maxDrop := config.GetIntParam(params, "drop_percentage_max", 60)
	dropPercent := rand.Intn(maxDrop-minDrop+1) + minDrop

	spike := config.SpikeEvent{
		SpikeID:         spikeID,
		Type:            "frame_drop",
		ParticipantIDs:  participants,
		DurationSeconds: duration,
		Params: map[string]any{
			"drop_percentage": fmt.Sprintf("%d", dropPercent),
		},
	}

	podCache.BroadcastSpike(cfg.TestID, spike)
	logging.LogChaos("Injected frame drop spike: %d%% frames dropped", dropPercent)
}

func injectBitrateReduceSpike(cfg *config.Config, podCache *k8s.PodCache, spikeID string, participants []int, duration int) {
	params := cfg.Spikes.Types["bitrate_reduce"].Params
	minReduction := config.GetIntParam(params, "reduction_percent_min", 30)
	maxReduction := config.GetIntParam(params, "reduction_percent_max", 80)
	minTransition := config.GetIntParam(params, "transition_seconds_min", 1)
	maxTransition := config.GetIntParam(params, "transition_seconds_max", 5)

	reductionPercent := rand.Intn(maxReduction-minReduction+1) + minReduction
	newBitrate := constants.DefaultBitrateKbps * (100 - reductionPercent) / 100
	transition := rand.Intn(maxTransition-minTransition+1) + minTransition

	spike := config.SpikeEvent{
		SpikeID:         spikeID,
		Type:            "bitrate_reduce",
		ParticipantIDs:  participants,
		DurationSeconds: duration,
		Params: map[string]any{
			"new_bitrate_kbps":   fmt.Sprintf("%d", newBitrate),
			"transition_seconds": fmt.Sprintf("%d", transition),
		},
	}

	podCache.BroadcastSpike(cfg.TestID, spike)
	logging.LogChaos("Injected bitrate reduction spike: %dkbps (%d%% reduction)", newBitrate, reductionPercent)
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
	if err := mustUnmarshalJSON(data, &metrics); err != nil {
		return
	}

	aggregate, ok := metrics["aggregate"].(map[string]any)
	if !ok {
		return
	}

	totalFrames := int(getFloat64(aggregate, "total_frames_sent"))
	totalPackets := int(getFloat64(aggregate, "total_packets_sent"))
	avgJitter := getFloat64(aggregate, "avg_jitter_ms")
	avgLoss := getFloat64(aggregate, "avg_packet_loss")
	remaining := max(int(time.Until(endTime).Seconds()), 0)

	utils.ShowProgress(int(elapsed.Seconds()), int(endTime.Sub(startTime).Seconds()))
	fmt.Printf(" | Remaining: %ds | Spikes: %d/%d", remaining, spikeCount, cfg.Spikes.Count)
	fmt.Println()
	logging.LogMetrics("Frames=%d | Packets=%d | Bitrate=%.0fkbps", totalFrames, totalPackets, getFloat64(aggregate, "total_bitrate_kbps"))
	logging.LogMetrics("Jitter=%.2fms | Loss=%.2f%% | MOS=%.2f", avgJitter, avgLoss*100, getFloat64(aggregate, "avg_mos_score"))
}

func getFloat64(m map[string]any, key string) float64 {
	if val, ok := m[key]; ok && val != nil {
		if f, ok := val.(float64); ok {
			return f
		}
	}
	return 0
}

func mustJSON(v any) string {
	data, _ := json.Marshal(v)
	return string(data)
}

func mustUnmarshalJSON(data []byte, v any) error {
	return json.Unmarshal(data, v)
}
