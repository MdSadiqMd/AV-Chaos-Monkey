package server

import (
	"log"
	"os"

	"github.com/MdSadiqMd/AV-Chaos-Monkey/pkg/constants"
	"github.com/MdSadiqMd/AV-Chaos-Monkey/internal/media"
	"github.com/MdSadiqMd/AV-Chaos-Monkey/internal/pool"
	"github.com/MdSadiqMd/AV-Chaos-Monkey/pkg/utils"
)

func initMediaSource() {
	videoPath := os.Getenv("MEDIA_VIDEO_PATH")
	audioPath := os.Getenv("MEDIA_AUDIO_PATH")
	mediaPath := os.Getenv("MEDIA_PATH")
	mediaSource := media.GetGlobalMediaSource()

	var config media.MediaConfig
	if mediaPath != "" {
		log.Printf("[HTTP] Using media file: %s", mediaPath)
		config = media.MediaConfig{VideoPath: mediaPath, AudioPath: mediaPath, VideoEnabled: true, AudioEnabled: true}
	} else if videoPath != "" || audioPath != "" {
		config = media.MediaConfig{VideoPath: videoPath, AudioPath: audioPath, VideoEnabled: videoPath != "", AudioEnabled: audioPath != ""}
		if videoPath != "" {
			log.Printf("[HTTP] Using video file: %s", videoPath)
		}
		if audioPath != "" {
			log.Printf("[HTTP] Using audio file: %s", audioPath)
		}
	} else {
		defaultPaths := []string{"/app/public/rick-roll.mp4", "public/rick-roll.mp4", "./public/rick-roll.mp4"}
		var defaultMedia string
		for _, path := range defaultPaths {
			if _, err := os.Stat(path); err == nil {
				defaultMedia = path
				break
			}
		}
		if defaultMedia != "" {
			log.Printf("[HTTP] Using default media file: %s", defaultMedia)
			config = media.MediaConfig{VideoPath: defaultMedia, AudioPath: defaultMedia, VideoEnabled: true, AudioEnabled: true}
		} else {
			log.Printf("[HTTP] No media files configured, using synthetic frames")
			return
		}
	}

	if err := mediaSource.Initialize(config); err != nil {
		log.Printf("[HTTP] Warning: Failed to initialize media source: %v (using synthetic frames)", err)
	} else {
		mediaSource.SetStreamingFPS(constants.StreamingFPS)
		log.Printf("[HTTP] Media source initialized: video=%v (%d NALs), audio=%v (%d packets), duration=%.1fs at %dfps",
			mediaSource.IsVideoEnabled(), mediaSource.GetVideoCount(),
			mediaSource.IsAudioEnabled(), mediaSource.GetAudioCount(),
			mediaSource.GetMediaDuration().Seconds(), constants.StreamingFPS)
	}
}

func configureUDPTarget(participantPool *pool.ParticipantPool) {
	udpTargetHost := os.Getenv("UDP_TARGET_HOST")
	udpTargetPort := utils.GetEnvInt("UDP_TARGET_PORT", 0)
	inKubernetes := os.Getenv("KUBERNETES_SERVICE_HOST") != ""

	if udpTargetHost != "" && udpTargetPort > 0 {
		participantPool.SetTarget(udpTargetHost, udpTargetPort)
		log.Printf("[HTTP] UDP transmission enabled: target=%s:%d (explicit)", udpTargetHost, udpTargetPort)
	} else if inKubernetes {
		participantPool.SetTarget("udp-relay", 5000)
		log.Printf("[HTTP] UDP transmission enabled: target=udp-relay:5000 (Kubernetes mode)")
	} else if udpTargetPort > 0 {
		participantPool.SetTarget(constants.DefaultTargetHost, udpTargetPort)
		log.Printf("[HTTP] UDP transmission enabled: target=%s:%d (port only)", constants.DefaultTargetHost, udpTargetPort)
	} else {
		participantPool.SetTarget(constants.DefaultTargetHost, constants.DefaultTargetPort)
		log.Printf("[HTTP] UDP transmission enabled: target=%s:%d (default)", constants.DefaultTargetHost, constants.DefaultTargetPort)
	}
}
