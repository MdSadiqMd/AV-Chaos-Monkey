package constants

const (
	DefaultTargetHost = "127.0.0.1"
	DefaultTargetPort = 5002

	RTCPFeedbackPort = 5003

	MetricsCacheTTL       = 500
	MetricsAggregatorTick = 250

	DefaultBatchSize          = 25
	DefaultBatchDelay         = 50
	LargeBatchSize            = 20
	LargeBatchDelay           = 75
	VeryLargeBatchSize        = 15
	VeryLargeBatchDelay       = 100
	LargeParticipantCount     = 100
	VeryLargeParticipantCount = 200

	DefaultFPS         = 30
	DefaultBitrateKbps = 2500
	DefaultWidth       = 1280
	DefaultHeight      = 720
	MinFrameSize       = 1000
	MaxFrameSize       = 100000
	KeyframeInterval   = 30

	StreamingFPS = 60

	RTPPayloadType     = 96    // H.264 video
	RTPPayloadTypeOpus = 111   // Opus audio
	RTPClockRate       = 90000 // Video clock rate
	RTPClockRateOpus   = 48000 // Opus clock rate
	OpusFrameDuration  = 20    // milliseconds per Opus frame

	IceUfragLength    = 4
	IcePasswordLength = 12

	SrtpMasterKeyLength  = 32
	SrtpMasterSaltLength = 14

	MaxSampledParticipants = 100

	UDPRelayTCPPort = 15001
	UDPRelayTCPAddr = "localhost:15001"
	UDPRelayUDPAddr = "localhost:5002"

	DefaultMediaPath = "public/rick-roll.mp4"
)
