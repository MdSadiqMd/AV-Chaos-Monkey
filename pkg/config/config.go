package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/MdSadiqMd/AV-Chaos-Monkey/internal/media"
	"github.com/MdSadiqMd/AV-Chaos-Monkey/pkg/constants"
)

type Config struct {
	BaseURL           string                   `json:"base_url"`
	MediaPath         string                   `json:"media_path"`
	NumParticipants   int                      `json:"num_participants"`
	DurationSeconds   int                      `json:"duration_seconds"`
	Spikes            SpikesConfig             `json:"spikes"`
	SpikeDistribution *SpikeDistributionConfig `json:"spike_distribution"`
	Metrics           MetricsConfig            `json:"metrics"`

	// Runtime fields
	TestID string      `json:"-"`
	Video  VideoConfig `json:"-"`
	Audio  AudioConfig `json:"-"`
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

type SpikesConfig struct {
	Count                int                        `json:"count"`
	IntervalSeconds      int                        `json:"interval_seconds"`
	Types                map[string]SpikeTypeConfig `json:"types"`
	DurationSecondsMin   int                        `json:"duration_seconds_min"`
	DurationSecondsMax   int                        `json:"duration_seconds_max"`
	ParticipantSelection ParticipantSelectionConfig `json:"participant_selection"`
}

type SpikeTypeConfig struct {
	Weight int            `json:"weight"`
	Params map[string]any `json:"params"`
}

type ParticipantSelectionConfig struct {
	SingleWeight     int `json:"single_weight"`
	MultipleWeight   int `json:"multiple_weight"`
	AllWeight        int `json:"all_weight"`
	MultipleCountMin int `json:"multiple_count_min"`
	MultipleCountMax int `json:"multiple_count_max"`
}

type SpikeDistributionConfig struct {
	Strategy          string `json:"strategy"` // "legacy", "even", "random", "front_loaded", "back_loaded"
	MinSpacingSeconds int    `json:"min_spacing_seconds"`
	JitterPercent     int    `json:"jitter_percent"`
}

type MetricsConfig struct {
	DisplayIntervalSeconds int `json:"display_interval_seconds"`
}

type SpikeEvent struct {
	SpikeID         string         `json:"spike_id"`
	Type            string         `json:"type"`
	ParticipantIDs  []int          `json:"participant_ids"`
	DurationSeconds int            `json:"duration_seconds"`
	Params          map[string]any `json:"params"`
}

type CreateTestRequest struct {
	TestID             string      `json:"test_id"`
	NumParticipants    int         `json:"num_participants"`
	Video              VideoConfig `json:"video"`
	Audio              AudioConfig `json:"audio"`
	DurationSeconds    int         `json:"duration_seconds"`
	BackendRTPBasePort string      `json:"backend_rtp_base_port"`
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	var config Config
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("failed to parse config JSON: %w", err)
	}

	config.TestID = fmt.Sprintf("chaos_test_%d", time.Now().Unix())
	applyDefaults(&config)

	return &config, nil
}

func LoadFromProjectRoot() (*Config, error) {
	projectRoot := GetProjectRoot()
	configPath := filepath.Join(projectRoot, "config", "config.json")
	return Load(configPath)
}

func applyDefaults(config *Config) {
	if config.BaseURL == "" {
		config.BaseURL = "http://localhost:8080"
	}
	if config.NumParticipants <= 0 {
		config.NumParticipants = 50
	}
	if config.Spikes.Count <= 0 {
		config.Spikes.Count = 70
	}
	if config.Spikes.IntervalSeconds <= 0 {
		config.Spikes.IntervalSeconds = 5
	}
	if config.Spikes.DurationSecondsMin <= 0 {
		config.Spikes.DurationSecondsMin = 2
	}
	if config.Spikes.DurationSecondsMax <= 0 {
		config.Spikes.DurationSecondsMax = 8
	}
	if config.Metrics.DisplayIntervalSeconds <= 0 {
		config.Metrics.DisplayIntervalSeconds = 2
	}
	if config.SpikeDistribution == nil {
		config.SpikeDistribution = &SpikeDistributionConfig{
			Strategy:          "legacy",
			MinSpacingSeconds: 5,
			JitterPercent:     15,
		}
	}

	if len(config.Spikes.Types) == 0 {
		config.Spikes.Types = map[string]SpikeTypeConfig{
			"rtp_packet_loss": {Weight: 40, Params: map[string]any{"loss_percentage_min": 1, "loss_percentage_max": 25, "patterns": []any{"random", "burst"}}},
			"network_jitter":  {Weight: 20, Params: map[string]any{"base_latency_ms_min": 10, "base_latency_ms_max": 50, "jitter_std_dev_ms_min": 20, "jitter_std_dev_ms_max": 200}},
			"frame_drop":      {Weight: 20, Params: map[string]any{"drop_percentage_min": 10, "drop_percentage_max": 60}},
			"bitrate_reduce":  {Weight: 20, Params: map[string]any{"reduction_percent_min": 30, "reduction_percent_max": 80, "transition_seconds_min": 1, "transition_seconds_max": 5}},
		}
	}

	if config.Spikes.ParticipantSelection.SingleWeight == 0 && config.Spikes.ParticipantSelection.MultipleWeight == 0 {
		config.Spikes.ParticipantSelection = ParticipantSelectionConfig{
			SingleWeight:     40,
			MultipleWeight:   35,
			AllWeight:        25,
			MultipleCountMin: 2,
			MultipleCountMax: 5,
		}
	}

	if config.MediaPath != "" {
		if !filepath.IsAbs(config.MediaPath) {
			config.MediaPath = filepath.Join(GetProjectRoot(), config.MediaPath)
		}
		if info, err := media.ProbeMedia(config.MediaPath); err == nil && config.DurationSeconds <= 0 {
			config.DurationSeconds = int(info.Duration)
		}
	}

	if config.DurationSeconds <= 0 {
		config.DurationSeconds = 600
	}

	config.Video = VideoConfig{
		Width:       constants.DefaultWidth,
		Height:      constants.DefaultHeight,
		FPS:         constants.DefaultFPS,
		BitrateKbps: constants.DefaultBitrateKbps,
		Codec:       "h264",
	}
	config.Audio = AudioConfig{
		SampleRate:  48000,
		Channels:    1,
		BitrateKbps: 128,
		Codec:       "opus",
	}
}

func GetIntParam(params map[string]any, key string, defaultVal int) int {
	if val, ok := params[key]; ok {
		switch v := val.(type) {
		case float64:
			return int(v)
		case int:
			return v
		}
	}
	return defaultVal
}

func GetProjectRoot() string {
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
