package constants

const (
	DefaultTargetHost = "127.0.0.1"
	DefaultTargetPort = 5002

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

	RTPPayloadType = 96
	RTPClockRate   = 90000

	IceUfragLength    = 4
	IcePasswordLength = 12

	SrtpMasterKeyLength  = 32
	SrtpMasterSaltLength = 14

	MaxSampledParticipants = 100

	UDPRelayTCPPort = 15001
	UDPRelayTCPAddr = "localhost:15001"
	UDPRelayUDPAddr = "localhost:5002"
)
