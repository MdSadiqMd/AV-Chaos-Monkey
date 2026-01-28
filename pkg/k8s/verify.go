package k8s

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/MdSadiqMd/AV-Chaos-Monkey/pkg/config"
	"github.com/MdSadiqMd/AV-Chaos-Monkey/pkg/logging"
	"github.com/MdSadiqMd/AV-Chaos-Monkey/pkg/utils"
)

func VerifyPartitionIDs() {
	logging.LogInfo("Verifying partition IDs in pods...")
	kubectlCmd, _ := utils.FindCommand("kubectl")

	cmd := exec.Command(kubectlCmd, "get", "pods", "-l", "app=orchestrator", "-o", "jsonpath={range .items[*]}{.metadata.name}{\"\\n\"}{end}")
	output, err := cmd.Output()
	if err != nil {
		logging.LogWarning("Failed to get pod names: %v", err)
		return
	}

	podNames := strings.Split(strings.TrimSpace(string(output)), "\n")
	logging.LogInfo("Found %d orchestrator pods", len(podNames))

	for _, podName := range podNames {
		podName = strings.TrimSpace(podName)
		if podName == "" {
			continue
		}

		expectedPartition := "0"
		if idx := strings.LastIndex(podName, "-"); idx >= 0 {
			expectedPartition = podName[idx+1:]
		}

		cmd := exec.Command(kubectlCmd, "logs", podName, "--tail=20", "--timestamps=false")
		logs, _ := cmd.Output()
		logStr := string(logs)

		cmd = exec.Command(kubectlCmd, "exec", podName, "--", "wget", "-qO-", "http://localhost:8080/healthz", "2>/dev/null")
		health, _ := cmd.Output()
		healthStr := string(health)

		cmd = exec.Command(kubectlCmd, "exec", podName, "--", "wget", "-qO-", "http://localhost:8080/metrics", "2>/dev/null")
		metrics, _ := cmd.Output()
		metricsStr := string(metrics)

		foundInHealth := strings.Contains(healthStr, fmt.Sprintf("\"partition_id\":%s", expectedPartition))
		foundInLogs := strings.Contains(logStr, fmt.Sprintf("partition %s", expectedPartition)) ||
			strings.Contains(logStr, fmt.Sprintf("ID=%s", expectedPartition)) ||
			strings.Contains(logStr, fmt.Sprintf("Partition config: ID=%s", expectedPartition))
		foundInMetrics := strings.Contains(metricsStr, fmt.Sprintf("partition=\"%s\"", expectedPartition))

		if foundInHealth || foundInLogs || foundInMetrics {
			logging.LogSuccess("Pod %s: Partition ID %s confirmed", podName, expectedPartition)
		} else {
			if strings.Contains(healthStr, "\"partition_id\":0") && expectedPartition != "0" {
				logging.LogError("Pod %s: Health endpoint shows partition_id=0 but expected %s. PARTITION_ID env var is not set!", podName, expectedPartition)
			} else if strings.Contains(metricsStr, "partition=\"0\"") && expectedPartition != "0" {
				logging.LogError("Pod %s: Metrics show partition=0 but expected %s. PARTITION_ID may not be set correctly!", podName, expectedPartition)
			} else if strings.Contains(logStr, "Partition config:") {
				if strings.Contains(logStr, "ID=0") && expectedPartition != "0" {
					logging.LogError("Pod %s: Logs show partition ID=0 but expected %s", podName, expectedPartition)
				}
			} else {
				logging.LogWarning("Pod %s: Could not verify partition ID (expected %s). Health: %s", podName, expectedPartition, healthStr[:min(100, len(healthStr))])
			}
		}
	}
}

func CheckMetricsPartitions(replicas int) {
	logging.LogInfo("Checking health endpoints for partition IDs...")
	kubectlCmd, _ := utils.FindCommand("kubectl")

	cmd := exec.Command(kubectlCmd, "get", "pods", "-l", "app=orchestrator", "-o", "jsonpath={range .items[*]}{.metadata.name}{\"\\n\"}{end}")
	output, err := cmd.Output()
	if err != nil {
		return
	}

	podNames := strings.Split(strings.TrimSpace(string(output)), "\n")
	partitionsFound := make(map[int]bool)
	partitionMap := make(map[string]int)

	for _, podName := range podNames {
		podName = strings.TrimSpace(podName)
		if podName == "" {
			continue
		}

		cmd := exec.Command(kubectlCmd, "exec", podName, "--", "wget", "-qO-", "http://localhost:8080/healthz", "2>/dev/null")
		health, _ := cmd.Output()

		var healthData map[string]any
		if err := json.Unmarshal(health, &healthData); err == nil {
			if pid, ok := healthData["partition_id"].(float64); ok {
				partitionID := int(pid)
				partitionsFound[partitionID] = true
				partitionMap[podName] = partitionID
			}
		}
	}

	if len(partitionsFound) == 0 {
		logging.LogError("No partition IDs found in health endpoints. All pods may be using partition 0!")
	} else if len(partitionsFound) == 1 {
		var pid int
		for p := range partitionsFound {
			pid = p
		}
		if pid == 0 {
			logging.LogError("All pods are reporting partition_id=0! PARTITION_ID environment variable is not being set correctly.")
			logging.LogInfo("Expected partitions: 0-%d, but all pods show partition 0", replicas-1)
		} else {
			logging.LogWarning("Only found partition %d (expected 0-%d)", pid, replicas-1)
		}
	} else if len(partitionsFound) < replicas {
		logging.LogWarning("Only found %d unique partitions (expected %d): %v", len(partitionsFound), replicas, partitionsFound)
		for pod, pid := range partitionMap {
			logging.LogInfo("  %s -> partition %d", pod, pid)
		}
	} else {
		logging.LogSuccess("Found all %d unique partitions: %v", len(partitionsFound), partitionsFound)
	}
}

func CleanupResources() {
	logging.LogInfo("Cleaning up Kubernetes resources...")
	projectRoot := config.GetProjectRoot()
	k8sDir := filepath.Join(projectRoot, "k8s")
	kubectlCmd, _ := utils.FindCommand("kubectl")

	exec.Command(kubectlCmd, "delete", "-f", filepath.Join(k8sDir, "orchestrator", "orchestrator.yaml"), "--ignore-not-found=true").Run()
	exec.Command(kubectlCmd, "delete", "-f", filepath.Join(k8sDir, "monitoring", "prometheus.yaml"), "--ignore-not-found=true").Run()
	exec.Command(kubectlCmd, "delete", "-f", filepath.Join(k8sDir, "monitoring", "grafana.yaml"), "--ignore-not-found=true").Run()
	exec.Command("pkill", "-f", "kubectl port-forward").Run()

	if err := utils.CleanupTargetFiles(); err == nil {
		logging.LogInfo("Cleaned up Prometheus target files")
	}

	logging.LogSuccess("Kubernetes resources cleaned up")
}

func GenerateTargetFiles(projectRoot string) error {
	return GenerateTargetFilesWithTestID(projectRoot, "")
}

func GenerateTargetFilesWithTestID(projectRoot string, testID string) error {
	logging.LogInfo("Generating Prometheus target files for debugging...")
	kubectlCmd, _ := utils.FindCommand("kubectl")

	cmd := exec.Command(kubectlCmd, "get", "pods", "-l", "app=orchestrator", "-o", "jsonpath={range .items[*]}{.metadata.name}{\"|\"}{.status.podIP}{\"\\n\"}{end}")
	output, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("failed to get pod info: %w", err)
	}

	targetsDir := filepath.Join(projectRoot, "config", "prometheus", "targets")
	os.MkdirAll(targetsDir, 0755)
	utils.CleanupTargetFiles()

	lines := strings.SplitSeq(strings.TrimSpace(string(output)), "\n")
	for line := range lines {
		parts := strings.Split(line, "|")
		if len(parts) != 2 {
			continue
		}
		podName := strings.TrimSpace(parts[0])
		podIP := strings.TrimSpace(parts[1])
		if podIP == "" || podIP == "<none>" {
			continue
		}

		partitionID := "0"
		if idx := strings.LastIndex(podName, "-"); idx >= 0 {
			partitionID = podName[idx+1:]
		}

		targetFile := filepath.Join(targetsDir, fmt.Sprintf("partition-%s.json", partitionID))
		labels := fmt.Sprintf(`      "partition": "%s",
      "pod": "%s",
      "job": "chaos-monkey"`, partitionID, podName)

		if testID != "" {
			labels = fmt.Sprintf(`      "partition": "%s",
      "pod": "%s",
      "job": "chaos-monkey",
      "test_id": "%s"`, partitionID, podName, testID)
		}

		targetJSON := fmt.Sprintf(`[
  {
    "targets": ["%s:8080"],
    "labels": {
%s
    }
  }
]
`, podIP, labels)

		if err := os.WriteFile(targetFile, []byte(targetJSON), 0644); err != nil {
			logging.LogWarning("Failed to write target file %s: %v", targetFile, err)
			continue
		}
	}

	logging.LogSuccess("Generated Prometheus target files in %s", targetsDir)
	return nil
}
