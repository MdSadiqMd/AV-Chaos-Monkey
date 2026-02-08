package k8s

import (
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/MdSadiqMd/AV-Chaos-Monkey/pkg/logging"
)

type PodInfo struct {
	Name           string
	PartitionID    int
	LocalPort      int
	PortForwardCmd *exec.Cmd
}

type Config struct {
	PortForwardBasePort int
	ConnectorBasePort   int
	UseConnector        bool
	ConnectorService    string
}

type Manager struct {
	config     *Config
	kubectlCmd string
	pods       []*PodInfo
	cleanups   []func()
}

func NewManager(config *Config) *Manager {
	kubectlCmd, err := exec.LookPath("kubectl")
	if err != nil {
		return nil
	}
	return &Manager{
		config:     config,
		kubectlCmd: kubectlCmd,
	}
}

func (m *Manager) IsAvailable() bool {
	return m != nil && m.kubectlCmd != ""
}

func (m *Manager) DiscoverPods() ([]*PodInfo, error) {
	return m.DiscoverPodsWithScale(0)
}

func (m *Manager) DiscoverPodsWithScale(participantCount int) ([]*PodInfo, error) {
	if m.config.UseConnector {
		return m.discoverConnectorPods(participantCount)
	}
	if participantCount > 0 {
		m.autoScaleOrchestrators(participantCount)
	}
	return m.discoverOrchestratorPods()
}

func (m *Manager) GetPodURL(pod *PodInfo) string {
	return fmt.Sprintf("http://localhost:%d", pod.LocalPort)
}

func (m *Manager) GetPods() []*PodInfo {
	return m.pods
}

func (m *Manager) Cleanup() {
	logging.LogInfo("Cleaning up port-forwards...")
	for _, cleanup := range m.cleanups {
		cleanup()
	}
}

func (m *Manager) autoScaleOrchestrators(participantCount int) {
	cmd := exec.Command(m.kubectlCmd, "get", "statefulset", "orchestrator", "-o", "jsonpath={.spec.replicas}")
	output, err := cmd.Output()
	if err != nil {
		return
	}

	var currentReplicas int
	fmt.Sscanf(strings.TrimSpace(string(output)), "%d", &currentReplicas)

	optimalReplicas := max((participantCount+149)/150, 2)
	optimalReplicas = min(optimalReplicas, 10)

	if currentReplicas >= optimalReplicas {
		return
	}

	logging.LogInfo("Auto-scaling orchestrators: %d → %d pods for %d participants", currentReplicas, optimalReplicas, participantCount)
	scaleCmd := exec.Command(m.kubectlCmd, "scale", "statefulset", "orchestrator", fmt.Sprintf("--replicas=%d", optimalReplicas))
	scaleCmd.Run()
	time.Sleep(5 * time.Second)
}

func (m *Manager) discoverConnectorPods(participantCount int) ([]*PodInfo, error) {
	if participantCount > 0 {
		desiredReplicas := max((participantCount+499)/500, 2)
		desiredReplicas = min(desiredReplicas, 10)
		m.scaleConnectors(desiredReplicas)
	}

	cmd := exec.Command(m.kubectlCmd, "get", "pods", "-l", "app=webrtc-connector", "--field-selector=status.phase=Running", "-o", "jsonpath={range .items[*]}{.metadata.name}{\"\\n\"}{end}")
	output, err := cmd.Output()
	if err != nil {
		logging.LogWarning("No connector pods found, falling back to orchestrator pods")
		return m.discoverOrchestratorPods()
	}

	podNames := parsePodNames(output)
	if len(podNames) == 0 {
		logging.LogWarning("No connector pods found, falling back to orchestrator pods")
		return m.discoverOrchestratorPods()
	}

	logging.LogInfo("Found %d connector pods", len(podNames))
	return m.setupPortForwards(podNames, m.config.ConnectorBasePort, "connector")
}

func (m *Manager) discoverOrchestratorPods() ([]*PodInfo, error) {
	cmd := exec.Command(m.kubectlCmd, "get", "pods", "-l", "app=orchestrator", "-o", "jsonpath={range .items[*]}{.metadata.name}{\"\\n\"}{end}")
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("failed to get pods: %w", err)
	}

	podNames := parsePodNames(output)
	if len(podNames) == 0 {
		return nil, fmt.Errorf("no orchestrator pods found")
	}

	logging.LogInfo("Found %d orchestrator pods", len(podNames))
	return m.setupPortForwards(podNames, m.config.PortForwardBasePort, "orchestrator")
}

func (m *Manager) setupPortForwards(podNames []string, basePort int, podType string) ([]*PodInfo, error) {
	m.pods = make([]*PodInfo, 0, len(podNames))
	m.cleanups = make([]func(), 0, len(podNames))

	var wg sync.WaitGroup
	var mu sync.Mutex
	results := make(chan *PodInfo, len(podNames))

	for i, podName := range podNames {
		wg.Add(1)
		go func(idx int, name string) {
			defer wg.Done()

			localPort := basePort + idx
			partitionID := 0
			if parts := strings.Split(name, "-"); len(parts) > 1 {
				fmt.Sscanf(parts[len(parts)-1], "%d", &partitionID)
			}

			pfCmd := exec.Command(m.kubectlCmd, "port-forward", fmt.Sprintf("pod/%s", name), fmt.Sprintf("%d:8080", localPort))
			if err := pfCmd.Start(); err != nil {
				logging.LogError("Failed to start port-forward for %s: %v", name, err)
				return
			}

			if !m.waitForPortForward(localPort, name) {
				pfCmd.Process.Kill()
				return
			}

			pod := &PodInfo{
				Name:           name,
				PartitionID:    partitionID,
				LocalPort:      localPort,
				PortForwardCmd: pfCmd,
			}

			mu.Lock()
			m.cleanups = append(m.cleanups, func() {
				if pfCmd.Process != nil {
					pfCmd.Process.Kill()
				}
			})
			mu.Unlock()

			results <- pod
			logging.LogSuccess("Port-forward ready for %s on localhost:%d", name, localPort)
		}(i, podName)
	}

	go func() {
		wg.Wait()
		close(results)
	}()

	for pod := range results {
		m.pods = append(m.pods, pod)
	}

	if len(m.pods) == 0 {
		return nil, fmt.Errorf("no %s pods available after port-forward setup", podType)
	}
	return m.pods, nil
}

func (m *Manager) waitForPortForward(localPort int, _ string) bool {
	testURL := fmt.Sprintf("http://localhost:%d/healthz", localPort)
	client := &http.Client{Timeout: 1 * time.Second}

	for range 6 {
		time.Sleep(500 * time.Millisecond)
		resp, err := client.Get(testURL)
		if err == nil && resp.StatusCode == http.StatusOK {
			resp.Body.Close()
			return true
		}
		if resp != nil {
			resp.Body.Close()
		}
	}
	return false
}

func (m *Manager) scaleConnectors(replicas int) {
	cmd := exec.Command(m.kubectlCmd, "get", "deployment", "webrtc-connector", "-o", "jsonpath={.spec.replicas}")
	output, err := cmd.Output()
	if err != nil {
		logging.LogWarning("Connector deployment not found")
		return
	}

	var currentReplicas int
	fmt.Sscanf(strings.TrimSpace(string(output)), "%d", &currentReplicas)

	if currentReplicas >= replicas {
		return
	}

	logging.LogInfo("Scaling connector pods: %d → %d replicas", currentReplicas, replicas)
	scaleCmd := exec.Command(m.kubectlCmd, "scale", "deployment", "webrtc-connector", fmt.Sprintf("--replicas=%d", replicas))
	scaleCmd.Run()
	time.Sleep(3 * time.Second)
}

func parsePodNames(output []byte) []string {
	podNames := []string{}
	for line := range strings.SplitSeq(strings.TrimSpace(string(output)), "\n") {
		if strings.TrimSpace(line) != "" {
			podNames = append(podNames, line)
		}
	}
	return podNames
}
