package spike

import (
	"fmt"
	"log"
	"strconv"
	"sync"
	"time"

	"github.com/MdSadiqMd/AV-Chaos-Monkey/pkg/network"
	pb "github.com/MdSadiqMd/AV-Chaos-Monkey/pkg/protobuf"
)

type SpikeType string

const (
	SpikeRTPPacketLoss  SpikeType = "rtp_packet_loss"
	SpikeSRTPRekey      SpikeType = "srtp_rekey"
	SpikeICERestart     SpikeType = "ice_restart"
	SpikeBitrateReduce  SpikeType = "bitrate_reduce"
	SpikeFrameDrop      SpikeType = "frame_drop"
	SpikeAudioSilence   SpikeType = "audio_silence"
	SpikeNetworkJitter  SpikeType = "network_jitter"
	SpikeBandwidthLimit SpikeType = "bandwidth_limit"
)

type Injector struct {
	mu           sync.RWMutex
	activeSpikes map[string]*ActiveSpike
	degrader     *network.Degrader
}

type ActiveSpike struct {
	Event     *pb.SpikeEvent
	StartTime time.Time
	EndTime   time.Time
	Applied   bool
	Cleanup   func() error
}

func NewInjector() *Injector {
	return &Injector{
		activeSpikes: make(map[string]*ActiveSpike),
		degrader:     network.NewDegrader(),
	}
}

func (inj *Injector) Inject(spike *pb.SpikeEvent) error {
	inj.mu.Lock()
	defer inj.mu.Unlock()

	if _, exists := inj.activeSpikes[spike.SpikeId]; exists {
		return fmt.Errorf("spike %s already active", spike.SpikeId)
	}

	active := &ActiveSpike{
		Event:     spike,
		StartTime: time.Now(),
		EndTime:   time.Now().Add(time.Duration(spike.DurationSeconds) * time.Second),
	}

	var err error
	var cleanup func() error

	switch SpikeType(spike.Type) {
	case SpikeRTPPacketLoss:
		cleanup, err = inj.applyPacketLoss(spike)
	case SpikeNetworkJitter:
		cleanup, err = inj.applyJitter(spike)
	case SpikeBandwidthLimit:
		cleanup, err = inj.applyBandwidthLimit(spike)
	case SpikeBitrateReduce:
		cleanup = func() error { return nil }
	case SpikeFrameDrop:
		cleanup = func() error { return nil }
	case SpikeAudioSilence:
		cleanup = func() error { return nil }
	case SpikeSRTPRekey:
		cleanup = func() error { return nil }
	case SpikeICERestart:
		cleanup = func() error { return nil }
	default:
		return fmt.Errorf("unknown spike type: %s", spike.Type)
	}

	if err != nil {
		return err
	}

	active.Cleanup = cleanup
	active.Applied = true
	inj.activeSpikes[spike.SpikeId] = active

	log.Printf("[Spike] Injected spike %s type=%s duration=%ds",
		spike.SpikeId, spike.Type, spike.DurationSeconds)

	if spike.DurationSeconds > 0 {
		go inj.scheduleCleanup(spike.SpikeId, time.Duration(spike.DurationSeconds)*time.Second)
	}

	return nil
}

func (inj *Injector) scheduleCleanup(SpikeId string, duration time.Duration) {
	time.Sleep(duration)
	inj.Remove(SpikeId)
}

func (inj *Injector) Remove(SpikeId string) error {
	inj.mu.Lock()
	defer inj.mu.Unlock()

	active, exists := inj.activeSpikes[SpikeId]
	if !exists {
		return nil
	}
	if active.Cleanup != nil {
		if err := active.Cleanup(); err != nil {
			log.Printf("[Spike] Cleanup error for %s: %v", SpikeId, err)
		}
	}

	delete(inj.activeSpikes, SpikeId)
	log.Printf("[Spike] Removed spike %s", SpikeId)

	return nil
}

func (inj *Injector) RemoveAll() {
	inj.mu.Lock()
	defer inj.mu.Unlock()

	for id, active := range inj.activeSpikes {
		if active.Cleanup != nil {
			active.Cleanup()
		}
		delete(inj.activeSpikes, id)
	}

	// Also remove all network degradation rules
	inj.degrader.RemoveAll()
}

func (inj *Injector) GetActive() []*ActiveSpike {
	inj.mu.RLock()
	defer inj.mu.RUnlock()

	result := make([]*ActiveSpike, 0, len(inj.activeSpikes))
	for _, s := range inj.activeSpikes {
		result = append(result, s)
	}
	return result
}

// Applies real packet loss using OS network tools
func (inj *Injector) applyPacketLoss(spike *pb.SpikeEvent) (func() error, error) {
	lossPercentStr := spike.Params["loss_percentage"]
	if lossPercentStr == "" {
		lossPercentStr = "5"
	}

	lossPct, err := strconv.Atoi(lossPercentStr)
	if err != nil {
		return nil, fmt.Errorf("invalid loss_percentage: %v", err)
	}

	port := 0
	if portStr, ok := spike.Params["port"]; ok {
		port, _ = strconv.Atoi(portStr)
	}

	// Applies packet loss using OS network tools
	if err := inj.degrader.ApplyPacketLoss(lossPct, port); err != nil {
		log.Printf("[Spike] Warning: Failed to apply real packet loss: %v (falling back to application-level)", err)
		// Fall back to application-level packet loss (handled in pool/participant.go
		return func() error { return nil }, nil
	}

	log.Printf("[Spike] Applied real %d%% packet loss on port %d", lossPct, port)

	cleanup := func() error {
		log.Printf("[Spike] Removing packet loss")
		return inj.degrader.RemoveAll()
	}

	return cleanup, nil
}

// Applies real network jitter using OS network tools
func (inj *Injector) applyJitter(spike *pb.SpikeEvent) (func() error, error) {
	baseLatencyStr := spike.Params["base_latency_ms"]
	if baseLatencyStr == "" {
		baseLatencyStr = "20"
	}

	jitterStr := spike.Params["jitter_std_dev_ms"]
	if jitterStr == "" {
		jitterStr = "50"
	}

	baseLatency, _ := strconv.Atoi(baseLatencyStr)
	jitter, _ := strconv.Atoi(jitterStr)

	port := 0
	if portStr, ok := spike.Params["port"]; ok {
		port, _ = strconv.Atoi(portStr)
	}

	// Applies latency/jitter using OS network tools
	if err := inj.degrader.ApplyLatency(baseLatency, jitter, port); err != nil {
		log.Printf("[Spike] Warning: Failed to apply real jitter: %v (network jitter will not be applied)", err)
		return func() error { return nil }, nil
	}

	log.Printf("[Spike] Applied real %dms latency with %dms jitter on port %d", baseLatency, jitter, port)

	cleanup := func() error {
		log.Printf("[Spike] Removing jitter")
		return inj.degrader.RemoveAll()
	}

	return cleanup, nil
}

// Applies real bandwidth limiting
func (inj *Injector) applyBandwidthLimit(spike *pb.SpikeEvent) (func() error, error) {
	kbpsStr := spike.Params["bandwidth_kbps"]
	if kbpsStr == "" {
		kbpsStr = "1000"
	}

	kbps, err := strconv.Atoi(kbpsStr)
	if err != nil {
		return nil, fmt.Errorf("invalid bandwidth_kbps: %v", err)
	}

	port := 0
	if portStr, ok := spike.Params["port"]; ok {
		port, _ = strconv.Atoi(portStr)
	}

	if err := inj.degrader.ApplyBandwidthLimit(kbps, port); err != nil {
		log.Printf("[Spike] Warning: Failed to apply bandwidth limit: %v", err)
		return func() error { return nil }, nil
	}

	log.Printf("[Spike] Applied %d kbps bandwidth limit on port %d", kbps, port)

	cleanup := func() error {
		log.Printf("[Spike] Removing bandwidth limit")
		return inj.degrader.RemoveAll()
	}

	return cleanup, nil
}

// Validates a spike event
func ValidateSpikeEvent(spike *pb.SpikeEvent) error {
	if spike.SpikeId == "" {
		return fmt.Errorf("spike_id is required")
	}
	if spike.Type == "" {
		return fmt.Errorf("type is required")
	}

	switch SpikeType(spike.Type) {
	case SpikeRTPPacketLoss:
		if pct, ok := spike.Params["loss_percentage"]; ok {
			v, err := strconv.Atoi(pct)
			if err != nil || v < 0 || v > 100 {
				return fmt.Errorf("loss_percentage must be 0-100")
			}
		}
	case SpikeBitrateReduce:
		if _, ok := spike.Params["new_bitrate_kbps"]; !ok {
			return fmt.Errorf("new_bitrate_kbps is required for bitrate_reduce")
		}
	case SpikeFrameDrop:
		if pct, ok := spike.Params["drop_percentage"]; ok {
			v, err := strconv.Atoi(pct)
			if err != nil || v < 0 || v > 100 {
				return fmt.Errorf("drop_percentage must be 0-100")
			}
		}
	case SpikeBandwidthLimit:
		if kbps, ok := spike.Params["bandwidth_kbps"]; ok {
			v, err := strconv.Atoi(kbps)
			if err != nil || v < 0 {
				return fmt.Errorf("bandwidth_kbps must be positive")
			}
		}
	}

	return nil
}

// Checks if the injector has necessary privileges for network manipulation
func (inj *Injector) CheckPrivileges() error {
	return inj.degrader.CheckPrivileges()
}
