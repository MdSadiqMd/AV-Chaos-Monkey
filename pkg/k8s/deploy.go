package k8s

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/MdSadiqMd/AV-Chaos-Monkey/pkg/logging"
	"github.com/MdSadiqMd/AV-Chaos-Monkey/pkg/utils"
)

func BuildImage(projectRoot, clusterName string) error {
	logging.LogInfo("Building Docker image...")
	cmd := exec.Command("docker", "build", "-t", "chaos-monkey-orchestrator:latest", ".", "--quiet")
	cmd.Dir = projectRoot
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to build image: %w", err)
	}

	if clusterName == "" {
		clusterName = utils.GetEnvOrDefault("CLUSTER_NAME", DefaultClusterName)
	}
	kindCmd, _ := utils.FindCommand("kind")
	cmd = exec.Command(kindCmd, "load", "docker-image", "chaos-monkey-orchestrator:latest", "--name", clusterName)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Run()

	logging.LogSuccess("Docker image built")
	return nil
}

func UpdateReplicas(projectRoot string, replicas int) error {
	logging.LogInfo("Updating Kubernetes manifests for %d replicas...", replicas)

	k8sDir := filepath.Join(projectRoot, "k8s", "orchestrator")
	orchestratorYAML := filepath.Join(k8sDir, "orchestrator.yaml")

	content, err := os.ReadFile(orchestratorYAML)
	if err != nil {
		return fmt.Errorf("failed to read orchestrator.yaml: %w", err)
	}

	contentStr := string(content)
	re := regexp.MustCompile(`replicas: \d+`)
	contentStr = re.ReplaceAllString(contentStr, fmt.Sprintf("replicas: %d", replicas))

	re = regexp.MustCompile(`(name: TOTAL_PARTITIONS\s+value: )"\d+"`)
	contentStr = re.ReplaceAllString(contentStr, fmt.Sprintf("${1}\"%d\"", replicas))

	if err := os.WriteFile(orchestratorYAML, []byte(contentStr), 0644); err != nil {
		return fmt.Errorf("failed to write orchestrator.yaml: %w", err)
	}

	logging.LogSuccess("Updated manifests for %d replicas", replicas)
	return nil
}

func UpdateUDPTarget(projectRoot string, udpTargetHost string, udpTargetPort int) error {
	k8sDir := filepath.Join(projectRoot, "k8s", "orchestrator")
	orchestratorYAML := filepath.Join(k8sDir, "orchestrator.yaml")

	content, err := os.ReadFile(orchestratorYAML)
	if err != nil {
		return fmt.Errorf("failed to read orchestrator.yaml: %w", err)
	}

	contentStr := string(content)
	re := regexp.MustCompile(`(name: UDP_TARGET_HOST\s+value: )"[^"]*"(\s*#.*)?`)
	contentStr = re.ReplaceAllString(contentStr, fmt.Sprintf("${1}\"%s\"${2}", udpTargetHost))

	re = regexp.MustCompile(`(name: UDP_TARGET_PORT\s+value: )"[^"]*"(\s*#.*)?`)
	contentStr = re.ReplaceAllString(contentStr, fmt.Sprintf("${1}\"%d\"${2}", udpTargetPort))

	if err := os.WriteFile(orchestratorYAML, []byte(contentStr), 0644); err != nil {
		return fmt.Errorf("failed to write orchestrator.yaml: %w", err)
	}

	logging.LogInfo("Updated UDP target: %s:%d", udpTargetHost, udpTargetPort)
	return nil
}

func DeployK8s(projectRoot string) error {
	logging.LogInfo("Deploying to Kubernetes...")
	k8sDir := filepath.Join(projectRoot, "k8s")
	kubectlCmd, _ := utils.FindCommand("kubectl")

	logging.LogInfo("Creating Grafana dashboard ConfigMap...")
	dashboardPath := filepath.Join(projectRoot, "config", "grafana", "dashboard.json")
	if _, err := os.Stat(dashboardPath); err == nil {
		cmd := exec.Command(kubectlCmd, "create", "configmap", "grafana-dashboard", "--from-file=chaos-monkey-dashboard.json="+dashboardPath, "--dry-run=client", "-o", "yaml")
		applyCmd := exec.Command(kubectlCmd, "apply", "-f", "-")
		applyCmd.Stdin, _ = cmd.StdoutPipe()
		applyCmd.Stdout = os.Stdout
		applyCmd.Stderr = os.Stderr
		applyCmd.Start()
		cmd.Run()
		applyCmd.Wait()
	}

	applyManifest := func(name, path string) {
		logging.LogInfo("Applying %s...", name)
		cmd := exec.Command(kubectlCmd, "apply", "-f", path)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		cmd.Run()
	}

	applyManifest("Prometheus RBAC", filepath.Join(k8sDir, "monitoring", "prometheus-rbac.yaml"))
	applyManifest("coturn TURN server", filepath.Join(k8sDir, "coturn", "coturn.yaml"))

	udpRelayPath := filepath.Join(k8sDir, "udp-relay", "udp-relay.yaml")
	if _, err := os.Stat(udpRelayPath); err == nil {
		exec.Command(kubectlCmd, "delete", "pod", "udp-relay", "--ignore-not-found=true").Run()
		time.Sleep(2 * time.Second)
		applyManifest("UDP relay", udpRelayPath)
	}

	logging.LogInfo("Applying orchestrator StatefulSet...")
	cmd := exec.Command(kubectlCmd, "apply", "-f", filepath.Join(k8sDir, "orchestrator", "orchestrator.yaml"))
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return err
	}

	applyManifest("Prometheus deployment", filepath.Join(k8sDir, "monitoring", "prometheus.yaml"))
	applyManifest("Grafana deployment", filepath.Join(k8sDir, "monitoring", "grafana.yaml"))

	logging.LogInfo("Waiting for pods to be ready...")
	waitForPod := func(label, name string) {
		logging.LogInfo("Waiting for %s pod...", name)
		cmd := exec.Command(kubectlCmd, "wait", "--for=condition=ready", "pod", "-l", label, "--timeout=120s")
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			logging.LogWarning("%s pod may not be ready yet", name)
		} else {
			logging.LogSuccess("%s pod is ready", name)
		}
	}

	waitForPod("app=orchestrator", "orchestrator")
	waitForPod("app=prometheus", "Prometheus")
	waitForPod("app=grafana", "Grafana")

	logging.LogSuccess("Kubernetes deployment ready")
	return nil
}

func WaitForPods(replicas int) error {
	logging.LogInfo("Waiting for orchestrator pods (need at least %d/%d)...", (replicas*8+9)/10, replicas)
	kubectlCmd, _ := utils.FindCommand("kubectl")
	minRequired := max((replicas*8+9)/10, 1)

	maxWait := 60
	waited := 0
	for waited < maxWait {
		cmd := exec.Command(kubectlCmd, "get", "pods", "-l", "app=orchestrator", "--no-headers")
		output, _ := cmd.Output()
		lines := strings.Split(strings.TrimSpace(string(output)), "\n")
		running, pending := 0, 0
		for _, line := range lines {
			if strings.Contains(line, "Running") {
				running++
			} else if strings.Contains(line, "Pending") || strings.Contains(line, "ContainerCreating") {
				pending++
			}
		}
		if running >= replicas {
			fmt.Println()
			logging.LogSuccess("All %d pods ready", replicas)
			ShowPodStatus()
			return nil
		}
		if running >= minRequired && pending > 0 && waited >= 20 {
			fmt.Println()
			logging.LogSuccess("%d/%d pods ready (sufficient for operation, %d pending due to resources)", running, replicas, pending)
			ShowPodStatus()
			return nil
		}
		if waited == 0 || waited%10 == 0 {
			fmt.Printf("\r  Pods ready: %d/%d (Pending: %d, need: %d)", running, replicas, pending, minRequired)
		} else {
			fmt.Printf("\r  Pods ready: %d/%d", running, replicas)
		}
		time.Sleep(2 * time.Second)
		waited += 2
	}
	fmt.Println()
	cmd := exec.Command(kubectlCmd, "get", "pods", "-l", "app=orchestrator", "--no-headers")
	output, _ := cmd.Output()
	running := strings.Count(string(output), "Running")
	if running >= minRequired {
		logging.LogSuccess("%d/%d pods ready (sufficient after timeout)", running, replicas)
	} else {
		logging.LogWarning("Only %d/%d pods ready after %ds (need at least %d)", running, replicas, maxWait, minRequired)
	}
	ShowPodStatus()
	return nil
}

func ShowPodStatus() {
	kubectlCmd, _ := utils.FindCommand("kubectl")
	cmd := exec.Command(kubectlCmd, "get", "pods", "-l", "app=orchestrator", "-o", "wide")
	output, err := cmd.Output()
	if err == nil {
		fmt.Println()
		fmt.Println("Orchestrator pod status:")
		fmt.Println(string(output))
	}
}
