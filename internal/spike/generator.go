package spike

import (
	"fmt"
	"math/rand"
	"time"

	"github.com/MdSadiqMd/AV-Chaos-Monkey/pkg/config"
	"github.com/MdSadiqMd/AV-Chaos-Monkey/pkg/constants"
	"github.com/MdSadiqMd/AV-Chaos-Monkey/pkg/k8s"
	"github.com/MdSadiqMd/AV-Chaos-Monkey/pkg/logging"
)

func InjectRandomSpike(cfg *config.Config, podCache *k8s.PodCache, spikeNum int) {
	spikeID := fmt.Sprintf("chaos_spike_%d_%d", spikeNum, time.Now().Unix())
	duration := rand.Intn(cfg.Spikes.DurationSecondsMax-cfg.Spikes.DurationSecondsMin+1) + cfg.Spikes.DurationSecondsMin
	participants := SelectParticipants(cfg)

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
				InjectPacketLossSpike(cfg, podCache, spikeID, participants, duration)
			case "network_jitter":
				InjectJitterSpike(cfg, podCache, spikeID, participants, duration)
			case "frame_drop":
				InjectFrameDropSpike(cfg, podCache, spikeID, participants, duration)
			case "bitrate_reduce":
				InjectBitrateReduceSpike(cfg, podCache, spikeID, participants, duration)
			}
			return
		}
	}

	InjectPacketLossSpike(cfg, podCache, spikeID, participants, duration)
}

func SelectParticipants(cfg *config.Config) []int {
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

func InjectPacketLossSpike(cfg *config.Config, podCache *k8s.PodCache, spikeID string, participants []int, duration int) {
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

func InjectJitterSpike(cfg *config.Config, podCache *k8s.PodCache, spikeID string, participants []int, duration int) {
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

func InjectFrameDropSpike(cfg *config.Config, podCache *k8s.PodCache, spikeID string, participants []int, duration int) {
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

func InjectBitrateReduceSpike(cfg *config.Config, podCache *k8s.PodCache, spikeID string, participants []int, duration int) {
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
