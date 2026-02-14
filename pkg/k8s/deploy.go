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
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to build image: %w", err)
	}

	if clusterName == "" {
		clusterName = utils.GetEnvOrDefault("CLUSTER_NAME", DefaultClusterName)
	}
	kindCmd, _ := utils.FindCommand("kind")
	cmd = exec.Command(kindCmd, "load", "docker-image", "chaos-monkey-orchestrator:latest", "--name", clusterName)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.Run()

	logging.LogSuccess("Docker image built")
	return nil
}

func UpdateReplicas(projectRoot string, replicas int) error {
	logging.LogInfo("Updating Kubernetes manifests for %d replicas...", replicas)
	path := filepath.Join(projectRoot, "k8s", "orchestrator", "orchestrator.yaml")

	content, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("failed to read orchestrator.yaml: %w", err)
	}

	contentStr := string(content)
	contentStr = regexp.MustCompile(`replicas: \d+`).ReplaceAllString(contentStr, fmt.Sprintf("replicas: %d", replicas))
	contentStr = regexp.MustCompile(`(name: TOTAL_PARTITIONS\s+value: )"\d+"`).ReplaceAllString(contentStr, fmt.Sprintf("${1}\"%d\"", replicas))

	if err := os.WriteFile(path, []byte(contentStr), 0644); err != nil {
		return fmt.Errorf("failed to write orchestrator.yaml: %w", err)
	}
	logging.LogSuccess("Updated manifests for %d replicas", replicas)
	return nil
}

func UpdateUDPTarget(projectRoot string, udpTargetHost string, udpTargetPort int) error {
	path := filepath.Join(projectRoot, "k8s", "orchestrator", "orchestrator.yaml")
	content, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("failed to read orchestrator.yaml: %w", err)
	}

	contentStr := string(content)
	contentStr = regexp.MustCompile(`(name: UDP_TARGET_HOST\s+value: )"[^"]*"(\s*#.*)?`).ReplaceAllString(contentStr, fmt.Sprintf("${1}\"%s\"${2}", udpTargetHost))
	contentStr = regexp.MustCompile(`(name: UDP_TARGET_PORT\s+value: )"[^"]*"(\s*#.*)?`).ReplaceAllString(contentStr, fmt.Sprintf("${1}\"%d\"${2}", udpTargetPort))

	if err := os.WriteFile(path, []byte(contentStr), 0644); err != nil {
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
		cmd := exec.Command(kubectlCmd, "create", "configmap", "grafana-dashboard",
			"--from-file=chaos-monkey-dashboard.json="+dashboardPath, "--dry-run=client", "-o", "yaml")
		applyCmd := exec.Command(kubectlCmd, "apply", "-f", "-")
		applyCmd.Stdin, _ = cmd.StdoutPipe()
		applyCmd.Stdout, applyCmd.Stderr = os.Stdout, os.Stderr
		applyCmd.Start()
		cmd.Run()
		applyCmd.Wait()
	}

	apply := func(name, path string) {
		logging.LogInfo("Applying %s...", name)
		cmd := exec.Command(kubectlCmd, "apply", "-f", path)
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		cmd.Run()
	}

	apply("Prometheus RBAC", filepath.Join(k8sDir, "monitoring", "prometheus-rbac.yaml"))
	apply("coturn TURN server", filepath.Join(k8sDir, "coturn", "coturn.yaml"))

	udpRelayPath := filepath.Join(k8sDir, "udp-relay", "udp-relay.yaml")
	if _, err := os.Stat(udpRelayPath); err == nil {
		exec.Command(kubectlCmd, "delete", "pod", "udp-relay", "--ignore-not-found=true").Run()
		time.Sleep(2 * time.Second)
		apply("UDP relay", udpRelayPath)
	}

	apply("orchestrator StatefulSet", filepath.Join(k8sDir, "orchestrator", "orchestrator.yaml"))
	apply("Prometheus deployment", filepath.Join(k8sDir, "monitoring", "prometheus.yaml"))
	apply("Grafana deployment", filepath.Join(k8sDir, "monitoring", "grafana.yaml"))

	logging.LogInfo("Waiting for pods to be ready...")
	for _, p := range []struct{ label, name string }{
		{"app=orchestrator", "orchestrator"},
		{"app=prometheus", "Prometheus"},
		{"app=grafana", "Grafana"},
	} {
		logging.LogInfo("Waiting for %s pod...", p.name)
		cmd := exec.Command(kubectlCmd, "wait", "--for=condition=ready", "pod", "-l", p.label, "--timeout=120s")
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			logging.LogWarning("%s pod may not be ready yet", p.name)
		} else {
			logging.LogSuccess("%s pod is ready", p.name)
		}
	}

	logging.LogSuccess("Kubernetes deployment ready")
	return nil
}

func WaitForPods(replicas int) error {
	minRequired := max((replicas*8+9)/10, 1)
	logging.LogInfo("Waiting for orchestrator pods (need at least %d/%d)...", minRequired, replicas)
	kubectlCmd, _ := utils.FindCommand("kubectl")

	for waited := 0; waited < 60; waited += 2 {
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
			logging.LogSuccess("%d/%d pods ready (sufficient, %d pending)", running, replicas, pending)
			ShowPodStatus()
			return nil
		}

		fmt.Printf("\r  Pods ready: %d/%d (Pending: %d)", running, replicas, pending)
		time.Sleep(2 * time.Second)
	}
	fmt.Println()
	ShowPodStatus()
	return nil
}

func ShowPodStatus() {
	kubectlCmd, _ := utils.FindCommand("kubectl")
	cmd := exec.Command(kubectlCmd, "get", "pods", "-l", "app=orchestrator", "-o", "wide")
	if output, err := cmd.Output(); err == nil {
		fmt.Println("\nOrchestrator pod status:")
		fmt.Println(string(output))
	}
}
