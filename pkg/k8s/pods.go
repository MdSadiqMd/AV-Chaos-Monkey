package k8s

import (
	"encoding/json"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/MdSadiqMd/AV-Chaos-Monkey/pkg/logging"
	"github.com/MdSadiqMd/AV-Chaos-Monkey/pkg/utils"
)

type PodCache struct {
	mu                   sync.RWMutex
	podNames             []string
	kubectlCmd           string
	consecutiveFailures  int
	lastPortForwardCheck time.Time
	portForwardCooldown  time.Duration
}

var (
	globalPodCache     *PodCache
	globalPodCacheOnce sync.Once
)

func GetPodCache() *PodCache {
	globalPodCacheOnce.Do(func() {
		globalPodCache = &PodCache{
			portForwardCooldown: 30 * time.Second,
		}
	})
	return globalPodCache
}

func (pc *PodCache) Initialize() error {
	kubectlCmd, err := utils.FindCommand("kubectl")
	if err != nil {
		logging.LogWarning("kubectl not found, spike injection will be limited")
		return err
	}

	pc.mu.Lock()
	pc.kubectlCmd = kubectlCmd
	pc.mu.Unlock()

	return pc.Refresh()
}

func (pc *PodCache) Refresh() error {
	pc.mu.Lock()
	defer pc.mu.Unlock()

	if pc.kubectlCmd == "" {
		return nil
	}

	cmd := exec.Command(pc.kubectlCmd, "get", "pods", "-l", "app=orchestrator",
		"-o", "jsonpath={range .items[*]}{.metadata.name}{\"\\n\"}{end}")
	output, err := cmd.Output()
	if err != nil {
		logging.LogWarning("Failed to get pod list: %v", err)
		return err
	}

	pc.podNames = ParsePodNames(string(output))

	if len(pc.podNames) > 0 {
		logging.LogInfo("Cached %d orchestrator pods for spike injection", len(pc.podNames))
	} else {
		logging.LogWarning("No orchestrator pods found")
	}

	return nil
}

func (pc *PodCache) GetPodNames() []string {
	pc.mu.RLock()
	defer pc.mu.RUnlock()
	return pc.podNames
}

func (pc *PodCache) GetKubectlCmd() string {
	pc.mu.RLock()
	defer pc.mu.RUnlock()
	return pc.kubectlCmd
}

func (pc *PodCache) IsAvailable() bool {
	pc.mu.RLock()
	defer pc.mu.RUnlock()
	return pc.kubectlCmd != "" && len(pc.podNames) > 0
}

func (pc *PodCache) BroadcastSpike(testID string, spike any) int {
	pc.mu.RLock()
	podNames := pc.podNames
	kubectlCmd := pc.kubectlCmd
	pc.mu.RUnlock()

	if len(podNames) == 0 || kubectlCmd == "" {
		logging.LogWarning("Pod cache not initialized, spike may not reach pods")
		return 0
	}

	spikeJSON, err := json.Marshal(spike)
	if err != nil {
		logging.LogWarning("Failed to marshal spike: %v", err)
		return 0
	}

	var wg sync.WaitGroup
	successCount := int32(0)

	for _, podName := range podNames {
		wg.Add(1)
		go func(pod string) {
			defer wg.Done()

			curlCmd := "curl -s --max-time 3 -X POST http://localhost:8080/api/v1/test/" + testID + "/spike -H 'Content-Type: application/json' -d @-"
			cmd := exec.Command(kubectlCmd, "exec", "-i", pod, "--", "sh", "-c", curlCmd)
			cmd.Stdin = strings.NewReader(string(spikeJSON))

			done := make(chan error, 1)
			go func() {
				_, err := cmd.Output()
				done <- err
			}()

			select {
			case err := <-done:
				if err == nil {
					atomic.AddInt32(&successCount, 1)
				}
			case <-time.After(4 * time.Second):
				if cmd.Process != nil {
					cmd.Process.Kill()
				}
			}
		}(podName)
	}

	wg.Wait()

	count := int(successCount)
	if count == 0 {
		pc.mu.Lock()
		pc.consecutiveFailures++
		if pc.consecutiveFailures >= 5 {
			logging.LogWarning("Multiple spike failures - kubectl may be overloaded")
			pc.consecutiveFailures = 0
		}
		pc.mu.Unlock()
	} else {
		pc.mu.Lock()
		pc.consecutiveFailures = 0
		pc.mu.Unlock()
	}

	return count
}

func (pc *PodCache) ExecOnPod(podName, command string) ([]byte, error) {
	pc.mu.RLock()
	kubectlCmd := pc.kubectlCmd
	pc.mu.RUnlock()

	if kubectlCmd == "" {
		return nil, nil
	}

	cmd := exec.Command(kubectlCmd, "exec", podName, "--", "sh", "-c", command)
	return cmd.Output()
}

func (pc *PodCache) ExecOnAllPods(command string) (int, error) {
	pc.mu.RLock()
	podNames := pc.podNames
	kubectlCmd := pc.kubectlCmd
	pc.mu.RUnlock()

	if kubectlCmd == "" || len(podNames) == 0 {
		return 0, nil
	}

	successCount := 0
	for _, podName := range podNames {
		cmd := exec.Command(kubectlCmd, "exec", podName, "--", "sh", "-c", command)
		if _, err := cmd.Output(); err == nil {
			successCount++
		}
	}

	return successCount, nil
}

func ParsePodNames(output string) []string {
	podNames := []string{}
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			podNames = append(podNames, line)
		}
	}
	return podNames
}
