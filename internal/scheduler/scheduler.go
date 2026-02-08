package scheduler

import (
	"context"
	"math/rand"
	"sync"
	"time"

	"github.com/MdSadiqMd/AV-Chaos-Monkey/pkg/logging"
)

type Strategy interface {
	Name() string
	// Schedule returns a channel that emits indices at appropriate times
	// The channel closes when all items are scheduled or context is cancelled
	Schedule(ctx context.Context, totalItems int) <-chan int
}

type Config struct {
	// RampUpDuration is the time to gradually start all items
	// If zero, items start as fast as possible with jitter
	RampUpDuration time.Duration

	// JitterPercent adds randomness to timing (0-100)
	// Helps prevent synchronized bursts
	JitterPercent int

	// MaxConcurrent limits simultaneous operations
	// Zero means no limit
	MaxConcurrent int

	// MinInterval is the minimum time between item starts
	MinInterval time.Duration
}

func DefaultConfig() *Config {
	return &Config{
		RampUpDuration: 0, // Immediate start with jitter
		JitterPercent:  20,
		MaxConcurrent:  50,
		MinInterval:    5 * time.Millisecond,
	}
}

type LinearRampStrategy struct {
	config *Config
}

func NewLinearRampStrategy(config *Config) *LinearRampStrategy {
	if config == nil {
		config = DefaultConfig()
	}
	return &LinearRampStrategy{config: config}
}

func (s *LinearRampStrategy) Name() string {
	return "linear_ramp"
}

func (s *LinearRampStrategy) Schedule(ctx context.Context, totalItems int) <-chan int {
	ch := make(chan int, s.config.MaxConcurrent)

	go func() {
		defer close(ch)

		if totalItems == 0 {
			return
		}

		// Calculate interval between starts
		interval := s.config.MinInterval
		if s.config.RampUpDuration > 0 && totalItems > 1 {
			interval = max(s.config.RampUpDuration/time.Duration(totalItems), s.config.MinInterval)
		}

		for i := range totalItems {
			select {
			case <-ctx.Done():
				return
			case ch <- i:
			}

			if i < totalItems-1 {
				// Add jitter to prevent synchronized operations
				jitteredInterval := applyJitter(interval, s.config.JitterPercent)
				select {
				case <-ctx.Done():
					return
				case <-time.After(jitteredInterval):
				}
			}
		}
	}()

	return ch
}

// StaggeredStrategy starts items with random delays within a window
// Best for desynchronizing periodic operations like frame sending
type StaggeredStrategy struct {
	config *Config
	window time.Duration
}

func NewStaggeredStrategy(window time.Duration, config *Config) *StaggeredStrategy {
	if config == nil {
		config = DefaultConfig()
	}
	return &StaggeredStrategy{
		config: config,
		window: window,
	}
}

func (s *StaggeredStrategy) Name() string {
	return "staggered"
}

func (s *StaggeredStrategy) Schedule(ctx context.Context, totalItems int) <-chan int {
	ch := make(chan int, s.config.MaxConcurrent)

	go func() {
		defer close(ch)

		if totalItems == 0 {
			return
		}

		// Generate random delays for each item
		type scheduledItem struct {
			index int
			delay time.Duration
		}

		items := make([]scheduledItem, totalItems)
		for i := range totalItems {
			items[i] = scheduledItem{
				index: i,
				delay: time.Duration(rand.Int63n(int64(s.window))),
			}
		}

		// Sort by delay (simple bubble sort for small N)
		for i := 0; i < len(items)-1; i++ {
			for j := i + 1; j < len(items); j++ {
				if items[j].delay < items[i].delay {
					items[i], items[j] = items[j], items[i]
				}
			}
		}

		// Emit items at their scheduled times
		startTime := time.Now()
		for _, item := range items {
			waitUntil := startTime.Add(item.delay)
			waitDuration := time.Until(waitUntil)
			if waitDuration > 0 {
				select {
				case <-ctx.Done():
					return
				case <-time.After(waitDuration):
				}
			}

			select {
			case <-ctx.Done():
				return
			case ch <- item.index:
			}
		}
	}()

	return ch
}

// Limits concurrent operations with rate limiting
type BurstControlStrategy struct {
	config     *Config
	burstSize  int
	burstDelay time.Duration
}

func NewBurstControlStrategy(burstSize int, burstDelay time.Duration, config *Config) *BurstControlStrategy {
	if config == nil {
		config = DefaultConfig()
	}
	if burstSize <= 0 {
		burstSize = 10
	}
	return &BurstControlStrategy{
		config:     config,
		burstSize:  burstSize,
		burstDelay: burstDelay,
	}
}

func (s *BurstControlStrategy) Name() string {
	return "burst_control"
}

func (s *BurstControlStrategy) Schedule(ctx context.Context, totalItems int) <-chan int {
	ch := make(chan int, s.config.MaxConcurrent)

	go func() {
		defer close(ch)

		for i := range totalItems {
			select {
			case <-ctx.Done():
				return
			case ch <- i:
			}

			// Add jitter within burst
			jitteredInterval := applyJitter(s.config.MinInterval, s.config.JitterPercent)
			time.Sleep(jitteredInterval)

			// Delay between bursts
			if (i+1)%s.burstSize == 0 && i < totalItems-1 {
				select {
				case <-ctx.Done():
					return
				case <-time.After(s.burstDelay):
				}
			}
		}
	}()

	return ch
}

type Executor struct {
	strategy      Strategy
	maxConcurrent int
}

func NewExecutor(strategy Strategy, maxConcurrent int) *Executor {
	if maxConcurrent <= 0 {
		maxConcurrent = 50
	}
	return &Executor{
		strategy:      strategy,
		maxConcurrent: maxConcurrent,
	}
}

type WorkFunc func(index int) error

// Execute runs the work function for each scheduled item
// Returns when all items complete or context is cancelled
func (e *Executor) Execute(ctx context.Context, totalItems int, work WorkFunc) error {
	indices := e.strategy.Schedule(ctx, totalItems)

	var wg sync.WaitGroup
	sem := make(chan struct{}, e.maxConcurrent)
	errCh := make(chan error, 1)

	for idx := range indices {
		select {
		case <-ctx.Done():
			wg.Wait()
			return ctx.Err()
		case err := <-errCh:
			wg.Wait()
			return err
		case sem <- struct{}{}:
		}

		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()

			if err := work(i); err != nil {
				select {
				case errCh <- err:
				default:
				}
			}
		}(idx)
	}

	wg.Wait()
	return nil
}

// ExecuteAsync runs work items without waiting for completion
// Returns immediately after scheduling starts
func (e *Executor) ExecuteAsync(ctx context.Context, totalItems int, work WorkFunc) {
	go func() {
		if err := e.Execute(ctx, totalItems, work); err != nil && err != context.Canceled {
			logging.LogError("Executor error: %v", err)
		}
	}()
}

func applyJitter(d time.Duration, jitterPercent int) time.Duration {
	if jitterPercent <= 0 {
		return d
	}
	if jitterPercent > 100 {
		jitterPercent = 100
	}

	jitterRange := float64(d) * float64(jitterPercent) / 100.0
	jitter := time.Duration(rand.Float64()*jitterRange*2 - jitterRange)
	result := d + jitter
	if result < 0 {
		return 0
	}
	return result
}
