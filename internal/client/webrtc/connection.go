package webrtc

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/MdSadiqMd/AV-Chaos-Monkey/pkg/logging"
	"github.com/pion/webrtc/v3"
)

type SDPResponse struct {
	ParticipantID uint32 `json:"participant_id"`
	SDPOffer      string `json:"sdp_offer"`
}

type SDPAnswerRequest struct {
	SDPAnswer string `json:"sdp_answer"`
}

type ParticipantConnection struct {
	ID             uint32
	PodName        string
	PeerConnection *webrtc.PeerConnection
	VideoPackets   atomic.Int64
	AudioPackets   atomic.Int64
	VideoBytes     atomic.Int64
	AudioBytes     atomic.Int64
	Connected      atomic.Bool
	Done           chan struct{}
	closed         atomic.Bool
}

func (conn *ParticipantConnection) Close() error {
	if conn.closed.Swap(true) {
		return nil
	}
	select {
	case <-conn.Done:
	default:
		close(conn.Done)
	}
	if conn.PeerConnection != nil {
		return conn.PeerConnection.Close()
	}
	return nil
}

type Config struct {
	HTTPClient             *http.Client
	ICEDisconnectedTimeout time.Duration
	ICEFailedTimeout       time.Duration
	ICEKeepaliveInterval   time.Duration
	ICEGatherTimeout       time.Duration
	ICEConnectionTimeout   time.Duration
	MaxRetries             int
	RetryBackoffBase       time.Duration
	TURNConfig             *TURNConfig
}

type TURNConfig struct {
	Host     string
	Username string
	Password string
}

func (tc *TURNConfig) GetWebRTCConfiguration() webrtc.Configuration {
	return webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{URLs: []string{fmt.Sprintf("stun:%s:3478", tc.Host)}},
			{URLs: []string{fmt.Sprintf("turn:%s:3478", tc.Host)}, Username: tc.Username, Credential: tc.Password},
		},
	}
}

type Manager struct {
	config *Config
}

func NewManager(config *Config) *Manager {
	return &Manager{config: config}
}

func (m *Manager) Connect(baseURL, testID string, participantID uint32, podName string, verbose bool) (*ParticipantConnection, error) {
	sdpURL := fmt.Sprintf("%s/api/v1/test/%s/sdp/%d", baseURL, testID, participantID)
	resp, err := m.config.HTTPClient.Get(sdpURL)
	if err != nil {
		return nil, fmt.Errorf("failed to get SDP: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("SDP request failed (%d): %s", resp.StatusCode, string(body))
	}

	var sdpResp SDPResponse
	if err := json.NewDecoder(resp.Body).Decode(&sdpResp); err != nil {
		return nil, fmt.Errorf("failed to decode SDP: %w", err)
	}

	pc, err := m.createPeerConnection()
	if err != nil {
		return nil, err
	}

	conn := &ParticipantConnection{
		ID:             participantID,
		PodName:        podName,
		PeerConnection: pc,
		Done:           make(chan struct{}),
	}

	m.setupTrackHandler(pc, conn, participantID, verbose)

	connected := make(chan bool, 1)
	m.setupConnectionHandler(pc, conn, participantID, verbose, connected)

	if err := m.processSDP(pc, sdpResp, participantID, verbose); err != nil {
		pc.Close()
		return nil, err
	}

	if err := m.sendAnswer(pc, baseURL, testID, participantID); err != nil {
		pc.Close()
		return nil, err
	}

	select {
	case <-connected:
		return conn, nil
	case <-time.After(m.config.ICEConnectionTimeout):
		return conn, nil
	}
}

func (m *Manager) ConnectWithRetry(baseURL, testID string, participantID uint32, podName string, verbose bool) (*ParticipantConnection, error) {
	var conn *ParticipantConnection
	var err error

	for attempt := 1; attempt <= m.config.MaxRetries; attempt++ {
		conn, err = m.Connect(baseURL, testID, participantID, podName, verbose)
		if err == nil {
			return conn, nil
		}
		if attempt < m.config.MaxRetries {
			logging.LogWarning("Retry %d/%d for participant %d: %v", attempt, m.config.MaxRetries, participantID, err)
			time.Sleep(time.Duration(attempt) * m.config.RetryBackoffBase)
		}
	}
	return nil, fmt.Errorf("failed after %d attempts: %w", m.config.MaxRetries, err)
}

func (m *Manager) ConnectBatch(podURL, testID string, participantIDs []uint32, podName string, concurrency int, verbose bool) []*ParticipantConnection {
	if len(participantIDs) == 0 {
		return nil
	}

	connections := make([]*ParticipantConnection, 0, len(participantIDs))
	connectedIDs := make(map[uint32]bool)
	var mu sync.Mutex

	if concurrency < 1 {
		concurrency = 50
	}

	maxWaves := 2
	remaining := participantIDs

	for wave := 1; wave <= maxWaves && len(remaining) > 0; wave++ {
		if wave > 1 {
			logging.LogInfo("Pod %s: Retry wave %d for %d remaining participants", podName, wave, len(remaining))
			time.Sleep(1 * time.Second)
		}

		var wg sync.WaitGroup
		var failed []uint32
		var failedMu sync.Mutex
		sem := make(chan struct{}, concurrency)

		for _, pid := range remaining {
			if connectedIDs[pid] {
				continue
			}
			wg.Add(1)
			sem <- struct{}{}
			go func(participantID uint32) {
				defer wg.Done()
				defer func() { <-sem }()

				conn, err := m.Connect(podURL, testID, participantID, podName, verbose)
				if err != nil {
					if verbose || wave > 1 {
						logging.LogWarning("Pod %s participant %d failed: %v", podName, participantID, err)
					}
					failedMu.Lock()
					failed = append(failed, participantID)
					failedMu.Unlock()
					return
				}
				mu.Lock()
				connections = append(connections, conn)
				connectedIDs[participantID] = true
				mu.Unlock()
			}(pid)
		}
		wg.Wait()
		remaining = failed
	}

	logging.LogSuccess("Pod %s: Connected to %d/%d participants", podName, len(connections), len(participantIDs))
	return connections
}

func (m *Manager) createPeerConnection() (*webrtc.PeerConnection, error) {
	mediaEngine := &webrtc.MediaEngine{}
	if err := mediaEngine.RegisterDefaultCodecs(); err != nil {
		return nil, fmt.Errorf("failed to register codecs: %w", err)
	}

	settingEngine := webrtc.SettingEngine{}
	settingEngine.SetICETimeouts(
		m.config.ICEDisconnectedTimeout,
		m.config.ICEFailedTimeout,
		m.config.ICEKeepaliveInterval,
	)

	api := webrtc.NewAPI(
		webrtc.WithMediaEngine(mediaEngine),
		webrtc.WithSettingEngine(settingEngine),
	)

	return api.NewPeerConnection(m.config.TURNConfig.GetWebRTCConfiguration())
}

func (m *Manager) setupTrackHandler(pc *webrtc.PeerConnection, conn *ParticipantConnection, participantID uint32, verbose bool) {
	pc.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		kind := track.Kind().String()
		if verbose {
			logging.LogInfo("Participant %d: Received %s track", participantID, kind)
		}

		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()

		for {
			select {
			case <-conn.Done:
				return
			case <-ticker.C:
				state := pc.ConnectionState()
				if state == webrtc.PeerConnectionStateClosed || state == webrtc.PeerConnectionStateFailed {
					return
				}
			default:
				pkt, _, err := track.ReadRTP()
				if err != nil {
					return
				}
				if kind == "video" {
					conn.VideoPackets.Add(1)
					conn.VideoBytes.Add(int64(len(pkt.Payload)))
				} else {
					conn.AudioPackets.Add(1)
					conn.AudioBytes.Add(int64(len(pkt.Payload)))
				}
			}
		}
	})
}

func (m *Manager) setupConnectionHandler(pc *webrtc.PeerConnection, conn *ParticipantConnection, participantID uint32, verbose bool, connected chan bool) {
	pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		if verbose {
			logging.LogInfo("Participant %d: ICE state: %s", participantID, state.String())
		}
		switch state {
		case webrtc.ICEConnectionStateConnected:
			conn.Connected.Store(true)
			select {
			case connected <- true:
			default:
			}
		case webrtc.ICEConnectionStateFailed, webrtc.ICEConnectionStateDisconnected:
			conn.Connected.Store(false)
		}
	})
}

func (m *Manager) processSDP(pc *webrtc.PeerConnection, sdpResp SDPResponse, _ uint32, _ bool) error {
	offer := webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: sdpResp.SDPOffer}
	if err := pc.SetRemoteDescription(offer); err != nil {
		return fmt.Errorf("failed to set remote description: %w", err)
	}

	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		return fmt.Errorf("failed to create answer: %w", err)
	}

	if err := pc.SetLocalDescription(answer); err != nil {
		return fmt.Errorf("failed to set local description: %w", err)
	}

	candidateFound := make(chan struct{}, 1)
	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c != nil {
			select {
			case candidateFound <- struct{}{}:
			default:
			}
		}
	})

	select {
	case <-candidateFound:
		time.Sleep(100 * time.Millisecond)
	case <-time.After(1 * time.Second):
	}

	if pc.LocalDescription() == nil {
		return fmt.Errorf("local description is nil")
	}
	return nil
}

func (m *Manager) sendAnswer(pc *webrtc.PeerConnection, baseURL, testID string, participantID uint32) error {
	finalAnswer := pc.LocalDescription()
	answerReq := SDPAnswerRequest{SDPAnswer: finalAnswer.SDP}
	answerBody, err := json.Marshal(answerReq)
	if err != nil {
		return fmt.Errorf("failed to marshal answer: %w", err)
	}

	answerURL := fmt.Sprintf("%s/api/v1/test/%s/answer/%d", baseURL, testID, participantID)
	answerResp, err := m.config.HTTPClient.Post(answerURL, "application/json", bytes.NewBuffer(answerBody))
	if err != nil {
		return fmt.Errorf("failed to send answer: %w", err)
	}
	defer answerResp.Body.Close()

	if answerResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(answerResp.Body)
		return fmt.Errorf("answer request failed: %d - %s", answerResp.StatusCode, string(body))
	}
	return nil
}
