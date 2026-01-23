package scheduler

import (
	"context"
	"math/rand"
	"sync"
	"time"

	"github.com/MdSadiqMd/AV-Chaos-Monkey/pkg/logging"
	pb "github.com/MdSadiqMd/AV-Chaos-Monkey/pkg/protobuf"
)

type SpikeScheduler struct {
	mu           sync.RWMutex
	spikes       []*ScheduledSpike
	ctx          context.Context
	cancel       context.CancelFunc
	injectFunc   SpikeInjectFunc
	testDuration time.Duration
	running      bool
}

type ScheduledSpike struct {
	Event       *pb.SpikeEvent
	ScheduledAt time.Time
	ExecutedAt  time.Time
	Executed    bool
}

type SpikeInjectFunc func(spike *pb.SpikeEvent) error

type SpikeSchedulerConfig struct {
	TestDuration         time.Duration
	MinSpacing           time.Duration
	JitterPercent        int
	DistributionStrategy string // "even", "random", "front_loaded", "back_loaded"
}

func DefaultSpikeSchedulerConfig() *SpikeSchedulerConfig {
	return &SpikeSchedulerConfig{
		TestDuration:         5 * time.Minute,
		MinSpacing:           5 * time.Second,
		JitterPercent:        15,
		DistributionStrategy: "even",
	}
}

func NewSpikeScheduler(config *SpikeSchedulerConfig, injectFunc SpikeInjectFunc) *SpikeScheduler {
	if config == nil {
		config = DefaultSpikeSchedulerConfig()
	}

	ctx, cancel := context.WithCancel(context.Background())
	return &SpikeScheduler{
		spikes:       make([]*ScheduledSpike, 0),
		ctx:          ctx,
		cancel:       cancel,
		injectFunc:   injectFunc,
		testDuration: config.TestDuration,
	}
}

func (ss *SpikeScheduler) ScheduleSpikes(spikes []*pb.SpikeEvent, config *SpikeSchedulerConfig) {
	ss.mu.Lock()
	defer ss.mu.Unlock()

	if len(spikes) == 0 {
		return
	}

	if config == nil {
		config = DefaultSpikeSchedulerConfig()
	}

	// Calculate even distribution
	scheduledSpikes := ss.distributeSpikes(spikes, config)
	ss.spikes = append(ss.spikes, scheduledSpikes...)

	logging.LogInfo("Scheduled %d spikes with %s distribution over %v",
		len(scheduledSpikes), config.DistributionStrategy, config.TestDuration)
}

// distributeSpikes calculates execution times based on strategy
func (ss *SpikeScheduler) distributeSpikes(spikes []*pb.SpikeEvent, config *SpikeSchedulerConfig) []*ScheduledSpike {
	result := make([]*ScheduledSpike, len(spikes))
	now := time.Now()

	switch config.DistributionStrategy {
	case "random":
		result = ss.distributeRandom(spikes, config, now)
	case "front_loaded":
		result = ss.distributeFrontLoaded(spikes, config, now)
	case "back_loaded":
		result = ss.distributeBackLoaded(spikes, config, now)
	default: // "even"
		result = ss.distributeEven(spikes, config, now)
	}

	return result
}

func (ss *SpikeScheduler) distributeEven(spikes []*pb.SpikeEvent, config *SpikeSchedulerConfig, startTime time.Time) []*ScheduledSpike {
	result := make([]*ScheduledSpike, len(spikes))
	interval := max(config.TestDuration/time.Duration(len(spikes)+1), config.MinSpacing)

	for i, spike := range spikes {
		// Base time: evenly distributed
		baseDelay := interval * time.Duration(i+1)

		// Respect minimum offset from spike config
		minOffset := time.Duration(spike.StartOffsetSeconds) * time.Second
		if baseDelay < minOffset {
			baseDelay = minOffset
		}

		// Add jitter
		jitter := applyJitter(interval/4, config.JitterPercent)
		scheduledTime := startTime.Add(baseDelay + jitter)

		result[i] = &ScheduledSpike{
			Event:       spike,
			ScheduledAt: scheduledTime,
		}
	}

	return result
}

func (ss *SpikeScheduler) distributeRandom(spikes []*pb.SpikeEvent, config *SpikeSchedulerConfig, startTime time.Time) []*ScheduledSpike {
	result := make([]*ScheduledSpike, len(spikes))

	for i, spike := range spikes {
		// Random time within test duration
		randomOffset := time.Duration(rand.Int63n(int64(config.TestDuration)))

		// Respect minimum offset
		minOffset := time.Duration(spike.StartOffsetSeconds) * time.Second
		if randomOffset < minOffset {
			randomOffset = minOffset
		}

		result[i] = &ScheduledSpike{
			Event:       spike,
			ScheduledAt: startTime.Add(randomOffset),
		}
	}

	// Sort by scheduled time to ensure proper spacing
	ss.sortAndSpace(result, config.MinSpacing)
	return result
}

func (ss *SpikeScheduler) distributeFrontLoaded(spikes []*pb.SpikeEvent, config *SpikeSchedulerConfig, startTime time.Time) []*ScheduledSpike {
	result := make([]*ScheduledSpike, len(spikes))

	// Use first 40% of test duration
	window := config.TestDuration * 40 / 100
	interval := max(window/time.Duration(len(spikes)+1), config.MinSpacing)

	for i, spike := range spikes {
		baseDelay := interval * time.Duration(i+1)
		minOffset := time.Duration(spike.StartOffsetSeconds) * time.Second
		if baseDelay < minOffset {
			baseDelay = minOffset
		}

		jitter := applyJitter(interval/4, config.JitterPercent)
		result[i] = &ScheduledSpike{
			Event:       spike,
			ScheduledAt: startTime.Add(baseDelay + jitter),
		}
	}

	return result
}

func (ss *SpikeScheduler) distributeBackLoaded(spikes []*pb.SpikeEvent, config *SpikeSchedulerConfig, startTime time.Time) []*ScheduledSpike {
	result := make([]*ScheduledSpike, len(spikes))

	// Use last 40% of test duration
	windowStart := config.TestDuration * 60 / 100
	window := config.TestDuration * 40 / 100
	interval := max(window/time.Duration(len(spikes)+1), config.MinSpacing)

	for i, spike := range spikes {
		baseDelay := windowStart + interval*time.Duration(i+1)
		minOffset := time.Duration(spike.StartOffsetSeconds) * time.Second
		if baseDelay < minOffset {
			baseDelay = minOffset
		}

		jitter := applyJitter(interval/4, config.JitterPercent)
		result[i] = &ScheduledSpike{
			Event:       spike,
			ScheduledAt: startTime.Add(baseDelay + jitter),
		}
	}

	return result
}

func (ss *SpikeScheduler) sortAndSpace(spikes []*ScheduledSpike, minSpacing time.Duration) {
	for i := 0; i < len(spikes)-1; i++ {
		for j := i + 1; j < len(spikes); j++ {
			if spikes[j].ScheduledAt.Before(spikes[i].ScheduledAt) {
				spikes[i], spikes[j] = spikes[j], spikes[i]
			}
		}
	}

	// Ensure minimum spacing
	for i := 1; i < len(spikes); i++ {
		minTime := spikes[i-1].ScheduledAt.Add(minSpacing)
		if spikes[i].ScheduledAt.Before(minTime) {
			spikes[i].ScheduledAt = minTime
		}
	}
}

func (ss *SpikeScheduler) Start() {
	ss.mu.Lock()
	if ss.running {
		ss.mu.Unlock()
		return
	}
	ss.running = true
	spikes := make([]*ScheduledSpike, len(ss.spikes))
	copy(spikes, ss.spikes)
	ss.mu.Unlock()

	go ss.runScheduler(spikes)
}

func (ss *SpikeScheduler) runScheduler(spikes []*ScheduledSpike) {
	for _, spike := range spikes {
		// Wait until scheduled time
		waitDuration := time.Until(spike.ScheduledAt)
		if waitDuration > 0 {
			select {
			case <-ss.ctx.Done():
				return
			case <-time.After(waitDuration):
			}
		}

		// Execute spike
		if ss.injectFunc != nil {
			if err := ss.injectFunc(spike.Event); err != nil {
				logging.LogError("Failed to inject spike %s: %v", spike.Event.SpikeId, err)
			} else {
				spike.ExecutedAt = time.Now()
				spike.Executed = true
				logging.LogChaos("Injected spike %s (type=%s) at scheduled time",
					spike.Event.SpikeId, spike.Event.Type)
			}
		}
	}
}

func (ss *SpikeScheduler) Stop() {
	ss.cancel()
	ss.mu.Lock()
	ss.running = false
	ss.mu.Unlock()
}

func (ss *SpikeScheduler) GetScheduledSpikes() []*ScheduledSpike {
	ss.mu.RLock()
	defer ss.mu.RUnlock()

	result := make([]*ScheduledSpike, len(ss.spikes))
	copy(result, ss.spikes)
	return result
}

// If the scheduler is running, the spike will be executed at its scheduled time
func (ss *SpikeScheduler) AddSpike(spike *pb.SpikeEvent) {
	ss.mu.Lock()
	defer ss.mu.Unlock()

	scheduledTime := time.Now().Add(time.Duration(spike.StartOffsetSeconds) * time.Second)
	scheduled := &ScheduledSpike{
		Event:       spike,
		ScheduledAt: scheduledTime,
	}
	ss.spikes = append(ss.spikes, scheduled)

	// If running, schedule execution
	if ss.running {
		go func() {
			waitDuration := time.Until(scheduledTime)
			if waitDuration > 0 {
				select {
				case <-ss.ctx.Done():
					return
				case <-time.After(waitDuration):
				}
			}

			if ss.injectFunc != nil {
				if err := ss.injectFunc(spike); err != nil {
					logging.LogError("Failed to inject spike %s: %v", spike.SpikeId, err)
				} else {
					scheduled.ExecutedAt = time.Now()
					scheduled.Executed = true
				}
			}
		}()
	}
}
