package client

import (
	"net/http"
	"os/exec"
	"strings"
	"time"

	"github.com/MdSadiqMd/AV-Chaos-Monkey/pkg/client/k8s"
	"github.com/MdSadiqMd/AV-Chaos-Monkey/pkg/client/webrtc"
	"github.com/MdSadiqMd/AV-Chaos-Monkey/pkg/utils"
)

// Re-export types for backward compatibility
type (
	ParticipantConnection = webrtc.ParticipantConnection
	PodInfo               = k8s.PodInfo
)

type Config struct {
	HTTPTimeout            time.Duration
	MaxIdleConns           int
	MaxIdleConnsPerHost    int
	IdleConnTimeout        time.Duration
	TURNHost               string
	TURNUsername           string
	TURNPassword           string
	ICEDisconnectedTimeout time.Duration
	ICEFailedTimeout       time.Duration
	ICEKeepaliveInterval   time.Duration
	ICEGatherTimeout       time.Duration
	ICEConnectionTimeout   time.Duration
	ConcurrencyLimit       int
	MaxRetries             int
	RetryBackoffBase       time.Duration
	PortForwardBasePort    int
	UseConnector           bool
	ConnectorBasePort      int
}

func DefaultConfig() *Config {
	return &Config{
		HTTPTimeout:            120 * time.Second,
		MaxIdleConns:           200,
		MaxIdleConnsPerHost:    50,
		IdleConnTimeout:        90 * time.Second,
		TURNHost:               utils.GetEnvOrDefault("TURN_HOST", detectTURNHost()),
		TURNUsername:           utils.GetEnvOrDefault("TURN_USERNAME", "webrtc"),
		TURNPassword:           utils.GetEnvOrDefault("TURN_PASSWORD", "webrtc123"),
		ICEDisconnectedTimeout: 30 * time.Second,
		ICEFailedTimeout:       60 * time.Second,
		ICEKeepaliveInterval:   5 * time.Second,
		ICEGatherTimeout:       300 * time.Millisecond,
		ICEConnectionTimeout:   500 * time.Millisecond,
		ConcurrencyLimit:       50,
		MaxRetries:             0,
		RetryBackoffBase:       0,
		PortForwardBasePort:    18080,
		UseConnector:           false,
		ConnectorBasePort:      19080,
	}
}

func (c *Config) NewHTTPClient() *http.Client {
	return &http.Client{
		Timeout: 3 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:          500,
			MaxIdleConnsPerHost:   50,
			MaxConnsPerHost:       50,
			IdleConnTimeout:       30 * time.Second,
			DisableKeepAlives:     false,
			ResponseHeaderTimeout: 2 * time.Second,
			DisableCompression:    true,
		},
	}
}

func detectTURNHost() string {
	cmd := exec.Command("kubectl", "get", "svc", "coturn-lb", "-o", "jsonpath={.spec.clusterIP}")
	output, err := cmd.Output()
	if err == nil && len(output) > 0 {
		ip := strings.TrimSpace(string(output))
		if ip != "" && ip != "None" {
			return ip
		}
	}
	return "localhost"
}

type ConnectionManager struct {
	config     *Config
	httpClient *http.Client
	webrtcMgr  *webrtc.Manager
}

func NewConnectionManager(config *Config) *ConnectionManager {
	httpClient := config.NewHTTPClient()
	webrtcConfig := &webrtc.Config{
		HTTPClient:             httpClient,
		ICEDisconnectedTimeout: config.ICEDisconnectedTimeout,
		ICEFailedTimeout:       config.ICEFailedTimeout,
		ICEKeepaliveInterval:   config.ICEKeepaliveInterval,
		ICEGatherTimeout:       config.ICEGatherTimeout,
		ICEConnectionTimeout:   config.ICEConnectionTimeout,
		MaxRetries:             config.MaxRetries,
		RetryBackoffBase:       config.RetryBackoffBase,
		TURNConfig: &webrtc.TURNConfig{
			Host:     config.TURNHost,
			Username: config.TURNUsername,
			Password: config.TURNPassword,
		},
	}
	return &ConnectionManager{
		config:     config,
		httpClient: httpClient,
		webrtcMgr:  webrtc.NewManager(webrtcConfig),
	}
}

func (cm *ConnectionManager) ConnectToParticipant(baseURL, testID string, participantID uint32, podName string, verbose bool) (*ParticipantConnection, error) {
	return cm.webrtcMgr.Connect(baseURL, testID, participantID, podName, verbose)
}

func (cm *ConnectionManager) ConnectToParticipantWithRetry(baseURL, testID string, participantID uint32, podName string, verbose bool) (*ParticipantConnection, error) {
	return cm.webrtcMgr.ConnectWithRetry(baseURL, testID, participantID, podName, verbose)
}

func (cm *ConnectionManager) ConnectToPodParticipants(podURL, testID string, pod *PodInfo, maxCount int, verbose bool) []*ParticipantConnection {
	participantIDs := cm.FindParticipantsInPod(podURL, testID, pod.PartitionID, maxCount)
	if len(participantIDs) == 0 {
		return nil
	}
	return cm.webrtcMgr.ConnectBatch(podURL, testID, participantIDs, pod.Name, cm.config.ConcurrencyLimit, verbose)
}

func (cm *ConnectionManager) FindParticipantsInPod(podURL, testID string, partitionID int, maxCount int) []uint32 {
	return findParticipantsInPod(cm.httpClient, podURL, testID, partitionID, maxCount)
}

func (cm *ConnectionManager) FindParticipants(baseURL, testID string, maxCount int) []uint32 {
	return findParticipants(cm.httpClient, baseURL, testID, maxCount)
}

type KubernetesManager struct {
	k8sMgr *k8s.Manager
}

func NewKubernetesManager(config *Config) *KubernetesManager {
	k8sConfig := &k8s.Config{
		PortForwardBasePort: config.PortForwardBasePort,
		ConnectorBasePort:   config.ConnectorBasePort,
		UseConnector:        config.UseConnector,
	}
	mgr := k8s.NewManager(k8sConfig)
	if mgr == nil {
		return nil
	}
	return &KubernetesManager{k8sMgr: mgr}
}

func (km *KubernetesManager) IsAvailable() bool {
	return km != nil && km.k8sMgr != nil && km.k8sMgr.IsAvailable()
}

func (km *KubernetesManager) DiscoverPodsWithScale(participantCount int) ([]*PodInfo, error) {
	return km.k8sMgr.DiscoverPodsWithScale(participantCount)
}

func (km *KubernetesManager) GetPodURL(pod *PodInfo) string {
	return km.k8sMgr.GetPodURL(pod)
}

func (km *KubernetesManager) Cleanup() {
	km.k8sMgr.Cleanup()
}
