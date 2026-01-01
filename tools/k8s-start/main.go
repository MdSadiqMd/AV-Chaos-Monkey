package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"text/template"
	"time"

	"github.com/MdSadiqMd/AV-Chaos-Monkey/pkg/utils"
)

const defaultClusterName = "av-chaos-monkey"

func main() {
	var (
		replicas        = flag.Int("replicas", 0, "Number of replicas (0 = auto-detect)")
		numParticipants = flag.Int("participants", 0, "Number of participants (0 = auto-calculate)")
		cleanup         = flag.Bool("cleanup", false, "Cleanup Kubernetes resources")
		skipTest        = flag.Bool("skip-test", false, "Skip running the test")
		setup           = flag.Bool("setup", false, "Setup Kubernetes cluster")
		clusterName     = flag.String("name", defaultClusterName, "Kubernetes cluster name")
	)
	flag.Parse()

	if *setup || (flag.NArg() > 0 && flag.Arg(0) == "setup") {
		if err := setupCluster(*clusterName); err != nil {
			fmt.Fprintf(os.Stderr, "❌ Error: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if flag.NArg() > 0 && flag.Arg(0) == "cleanup-cluster" {
		if err := cleanupCluster(*clusterName); err != nil {
			fmt.Fprintf(os.Stderr, "❌ Error: %v\n", err)
			os.Exit(1)
		}
		return
	}

	args := flag.Args()
	if len(args) > 0 && (args[0] == "cleanup" || args[0] == "--cleanup") {
		cleanupResources()
		return
	}

	if *cleanup {
		cleanupResources()
		return
	}

	if env := os.Getenv("REPLICAS"); env != "" {
		if n, err := strconv.Atoi(env); err == nil {
			replicas = &n
		}
	}
	if env := os.Getenv("NUM_PARTICIPANTS"); env != "" {
		if n, err := strconv.Atoi(env); err == nil {
			numParticipants = &n
		}
	}

	projectRoot := getProjectRoot()
	if err := runK8sDeployment(projectRoot, *replicas, *numParticipants, *skipTest); err != nil {
		fmt.Fprintf(os.Stderr, "❌ Error: %v\n", err)
		os.Exit(1)
	}
}

func runK8sDeployment(projectRoot string, userReplicas, userParticipants int, skipTest bool) error {
	if err := checkPrerequisites(); err != nil {
		return err
	}

	systemMem, _ := utils.DetectSystemMemory()
	dockerMem := utils.DetectDockerMemory()
	cpuCores := utils.DetectCPUCores()

	logInfo("System Detection:")
	fmt.Printf("  System RAM:    %s%dGB%s\n", utils.BOLD, systemMem, utils.NC)
	fmt.Printf("  Docker Memory: %s%dGB%s\n", utils.BOLD, dockerMem, utils.NC)
	fmt.Printf("  CPU Cores:     %s%d%s\n", utils.BOLD, cpuCores, utils.NC)
	fmt.Println()

	optimalReplicas := utils.CalculateOptimalReplicas(dockerMem)
	participantsPerPod := utils.CalculateParticipantsPerPod()

	replicas := userReplicas
	if replicas == 0 {
		replicas = optimalReplicas
		logConfig("Auto-detected optimal replicas: %d", replicas)
	} else {
		logConfig("Using user-specified replicas: %d", replicas)
	}

	participants := userParticipants
	if participants == 0 {
		participants = replicas * participantsPerPod
		logConfig("Auto-calculated participants: %d", participants)
	} else {
		logConfig("Using user-specified participants: %d", participants)
	}

	fmt.Println()
	fmt.Printf("%sFinal Configuration:%s\n", utils.BOLD, utils.NC)
	fmt.Printf("  Replicas:          %s%d pods%s\n", utils.CYAN, replicas, utils.NC)
	fmt.Printf("  Participants:      %s%d%s (~%d per pod)\n", utils.CYAN, participants, utils.NC, participants/replicas)
	fmt.Printf("  Memory per pod:    %s2GB%s\n", utils.CYAN, utils.NC)
	fmt.Printf("  Total memory:      %s%dGB%s (pods + monitoring)\n", utils.CYAN, replicas*2+2, utils.NC)
	fmt.Printf("  Throughput:        %s%dx%s\n", utils.CYAN, replicas, utils.NC)
	fmt.Println()

	if err := buildImage(projectRoot); err != nil {
		return err
	}
	if err := updateReplicas(projectRoot, replicas); err != nil {
		return err
	}
	if err := deployK8s(projectRoot); err != nil {
		return err
	}
	if err := waitForPods(replicas); err != nil {
		return err
	}
	// Update Prometheus targets with actual pod IPs
	if err := updatePrometheusTargets(replicas, projectRoot); err != nil {
		logWarning("Failed to update Prometheus targets: %v", err)
	}
	if err := generateTargetFiles(projectRoot); err != nil {
		logWarning("Failed to generate target files: %v", err)
	}

	verifyPartitionIDs()
	checkMetricsPartitions(replicas)
	setupPortForwarding()

	if !skipTest {
		os.Setenv("NUM_PARTICIPANTS", strconv.Itoa(participants))
		os.Setenv("TEST_DURATION_SECONDS", "600")
		os.Setenv("BASE_URL", "http://localhost:8080")

		chaosTestPath := filepath.Join(projectRoot, "tools", "chaos-test", "main.go")
		cmd := exec.Command("go", "run", chaosTestPath)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		cmd.Run()
	}

	return nil
}

func checkPrerequisites() error {
	logInfo("Checking prerequisites...")

	if !utils.CommandExists("kubectl") || !utils.CommandExists("kind") {
		return fmt.Errorf("kubectl/kind not found. Make sure you're in Nix devShell: nix develop")
	}

	if err := utils.EnsureDockerRunning(); err != nil {
		return fmt.Errorf("Docker check failed: %w", err)
	}

	clusterName := os.Getenv("CLUSTER_NAME")
	if clusterName == "" {
		clusterName = defaultClusterName
	}

	kindCmd, _ := utils.FindCommand("kind")
	cmd := exec.Command(kindCmd, "get", "clusters")
	output, err := cmd.Output()
	if err != nil || !strings.Contains(string(output), clusterName) {
		fmt.Println("[INFO] Setting up Kubernetes cluster...")
		if err := setupCluster(clusterName); err != nil {
			return fmt.Errorf("failed to setup cluster: %w", err)
		}
	}

	kubectlCmd, _ := utils.FindCommand("kubectl")
	exec.Command(kubectlCmd, "config", "use-context", fmt.Sprintf("kind-%s", clusterName)).Run()

	logSuccess("Prerequisites OK")
	return nil
}

func buildImage(projectRoot string) error {
	logInfo("Building Docker image...")
	cmd := exec.Command("docker", "build", "-t", "chaos-monkey-orchestrator:latest", ".", "--quiet")
	cmd.Dir = projectRoot
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to build image: %w", err)
	}

	// Load into kind
	clusterName := os.Getenv("CLUSTER_NAME")
	if clusterName == "" {
		clusterName = defaultClusterName
	}
	kindCmd, _ := utils.FindCommand("kind")
	cmd = exec.Command(kindCmd, "load", "docker-image", "chaos-monkey-orchestrator:latest", "--name", clusterName)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Run() // Don't fail if this fails

	logSuccess("Docker image built")
	return nil
}

func updateReplicas(projectRoot string, replicas int) error {
	logInfo("Updating Kubernetes manifests for %d replicas...", replicas)

	k8sDir := filepath.Join(projectRoot, "k8s", "orchestrator")
	orchestratorYAML := filepath.Join(k8sDir, "orchestrator.yaml")

	// Read the original YAML file
	content, err := os.ReadFile(orchestratorYAML)
	if err != nil {
		return fmt.Errorf("failed to read orchestrator.yaml: %w", err)
	}

	// Replace replicas value (match "replicas: 10" or "replicas: 5", etc.)
	replicasPattern := `replicas: \d+`
	replicasReplacement := fmt.Sprintf("replicas: %d", replicas)
	contentStr := string(content)

	// Use regex to replace replicas
	re := regexp.MustCompile(replicasPattern)
	contentStr = re.ReplaceAllString(contentStr, replicasReplacement)

	// Replace TOTAL_PARTITIONS value (match 'value: "10"' or 'value: "5"', etc.)
	totalPartitionsPattern := `(name: TOTAL_PARTITIONS\s+value: )"\d+"`
	totalPartitionsReplacement := fmt.Sprintf("${1}\"%d\"", replicas)
	re = regexp.MustCompile(totalPartitionsPattern)
	contentStr = re.ReplaceAllString(contentStr, totalPartitionsReplacement)

	// Write back to file
	if err := os.WriteFile(orchestratorYAML, []byte(contentStr), 0644); err != nil {
		return fmt.Errorf("failed to write orchestrator.yaml: %w", err)
	}

	logSuccess("Updated manifests for %d replicas", replicas)
	return nil
}

func deployK8s(projectRoot string) error {
	logInfo("Deploying to Kubernetes...")

	k8sDir := filepath.Join(projectRoot, "k8s")
	kubectlCmd, _ := utils.FindCommand("kubectl")

	// Create Grafana dashboard ConfigMap
	logInfo("Creating Grafana dashboard ConfigMap...")
	dashboardPath := filepath.Join(projectRoot, "config", "grafana", "dashboard.json")
	if _, err := os.Stat(dashboardPath); err == nil {
		cmd := exec.Command(kubectlCmd, "create", "configmap", "grafana-dashboard",
			"--from-file=chaos-monkey-dashboard.json="+dashboardPath,
			"--dry-run=client", "-o", "yaml")
		applyCmd := exec.Command(kubectlCmd, "apply", "-f", "-")
		applyCmd.Stdin, _ = cmd.StdoutPipe()
		applyCmd.Stdout = os.Stdout
		applyCmd.Stderr = os.Stderr
		applyCmd.Start()
		cmd.Run()
		applyCmd.Wait()
	}

	// Apply RBAC first (needed for Prometheus to discover pods)
	logInfo("Applying Prometheus RBAC...")
	cmd := exec.Command(kubectlCmd, "apply", "-f", filepath.Join(k8sDir, "monitoring", "prometheus-rbac.yaml"))
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Run()

	// Apply orchestrator
	logInfo("Applying orchestrator StatefulSet...")
	cmd = exec.Command(kubectlCmd, "apply", "-f", filepath.Join(k8sDir, "orchestrator", "orchestrator.yaml"))
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return err
	}

	// Apply monitoring
	logInfo("Applying Prometheus deployment...")
	cmd = exec.Command(kubectlCmd, "apply", "-f", filepath.Join(k8sDir, "monitoring", "prometheus.yaml"))
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Run()

	logInfo("Applying Grafana deployment...")
	cmd = exec.Command(kubectlCmd, "apply", "-f", filepath.Join(k8sDir, "monitoring", "grafana.yaml"))
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Run()

	logInfo("Waiting for pods to be ready...")

	// Wait for orchestrator pods with verbose output
	logInfo("Waiting for orchestrator pods...")
	cmd = exec.Command(kubectlCmd, "wait", "--for=condition=ready", "pod", "-l", "app=orchestrator", "--timeout=120s")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		logWarning("Some orchestrator pods may not be ready yet")
	}

	// Wait for Prometheus
	logInfo("Waiting for Prometheus pod...")
	cmd = exec.Command(kubectlCmd, "wait", "--for=condition=ready", "pod", "-l", "app=prometheus", "--timeout=120s")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		logWarning("Prometheus pod may not be ready yet")
	} else {
		logSuccess("Prometheus pod is ready")
	}

	// Wait for Grafana
	logInfo("Waiting for Grafana pod...")
	cmd = exec.Command(kubectlCmd, "wait", "--for=condition=ready", "pod", "-l", "app=grafana", "--timeout=120s")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		logWarning("Grafana pod may not be ready yet")
	} else {
		logSuccess("Grafana pod is ready")
	}

	logSuccess("Kubernetes deployment ready")
	return nil
}

func waitForPods(replicas int) error {
	logInfo("Waiting for all %d orchestrator pods...", replicas)
	kubectlCmd, _ := utils.FindCommand("kubectl")

	maxWait := 60
	waited := 0
	for waited < maxWait {
		cmd := exec.Command(kubectlCmd, "get", "pods", "-l", "app=orchestrator", "--no-headers")
		output, _ := cmd.Output()
		lines := strings.Split(strings.TrimSpace(string(output)), "\n")
		running := 0
		pending := 0
		for _, line := range lines {
			if strings.Contains(line, "Running") {
				running++
			} else if strings.Contains(line, "Pending") || strings.Contains(line, "ContainerCreating") {
				pending++
			}
		}
		if running >= replicas {
			fmt.Println()
			logSuccess("All %d pods ready", replicas)
			// Show pod status
			showPodStatus(kubectlCmd)
			return nil
		}
		if waited == 0 || waited%10 == 0 {
			fmt.Printf("\r  Pods ready: %d/%d (Pending: %d)", running, replicas, pending)
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
	logWarning("Only %d/%d pods ready after %ds", running, replicas, maxWait)
	showPodStatus(kubectlCmd)
	return nil
}

func showPodStatus(kubectlCmd string) {
	cmd := exec.Command(kubectlCmd, "get", "pods", "-l", "app=orchestrator", "-o", "wide")
	output, err := cmd.Output()
	if err == nil {
		fmt.Println()
		fmt.Println("Orchestrator pod status:")
		fmt.Println(string(output))
	}
}

func updatePrometheusTargets(replicas int, projectRoot string) error {
	logInfo("Configuring Prometheus with orchestrator pod IPs...")
	kubectlCmd, _ := utils.FindCommand("kubectl")

	// Wait a bit for all pods to have IPs assigned
	time.Sleep(3 * time.Second)

	// Get all orchestrator pod IPs - retry a few times if needed
	var podIPs []string
	maxRetries := 10
	for retry := range maxRetries {
		cmd := exec.Command(kubectlCmd, "get", "pods", "-l", "app=orchestrator", "-o", "jsonpath={range .items[*]}{.status.podIP}{\"\\n\"}{end}")
		output, err := cmd.Output()
		if err != nil {
			if retry < maxRetries-1 {
				time.Sleep(2 * time.Second)
				continue
			}
			return fmt.Errorf("failed to get pod IPs: %w", err)
		}

		podIPs = []string{}
		for line := range strings.SplitSeq(strings.TrimSpace(string(output)), "\n") {
			line = strings.TrimSpace(line)
			if line != "" && line != "<none>" {
				podIPs = append(podIPs, line)
			}
		}

		if len(podIPs) >= replicas {
			break
		}

		if retry < maxRetries-1 {
			logInfo("Waiting for pod IPs... (got %d/%d)", len(podIPs), replicas)
			time.Sleep(2 * time.Second)
		}
	}

	if len(podIPs) == 0 {
		return fmt.Errorf("no pod IPs found")
	}

	if len(podIPs) < replicas {
		logWarning("Only got %d pod IPs but expected %d replicas", len(podIPs), replicas)
	}

	logInfo("Found %d orchestrator pod IPs: %v", len(podIPs), podIPs)

	// Generate Prometheus config with actual pod IPs
	k8sDir := filepath.Join(projectRoot, "k8s", "monitoring")
	tmpl := `apiVersion: v1
kind: ConfigMap
metadata:
  name: prometheus-config
data:
  prometheus.yml: |
    global:
      scrape_interval: 5s
      evaluation_interval: 5s

    scrape_configs:
      - job_name: 'orchestrator'
        static_configs:
          - targets:
{{range .Targets}}            - '{{.}}:8080'
{{end}}        metrics_path: /metrics

      - job_name: 'prometheus'
        static_configs:
          - targets: ['localhost:9090']
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: prometheus
  labels:
    app: prometheus
spec:
  replicas: 1
  selector:
    matchLabels:
      app: prometheus
  template:
    metadata:
      labels:
        app: prometheus
    spec:
      serviceAccountName: prometheus
      containers:
      - name: prometheus
        image: prom/prometheus:latest
        ports:
        - containerPort: 9090
        volumeMounts:
        - name: config
          mountPath: /etc/prometheus
        args:
        - '--config.file=/etc/prometheus/prometheus.yml'
        - '--storage.tsdb.path=/prometheus'
        - '--web.enable-lifecycle'
      volumes:
      - name: config
        configMap:
          name: prometheus-config
---
apiVersion: v1
kind: Service
metadata:
  name: prometheus
spec:
  type: NodePort
  ports:
  - port: 9090
    targetPort: 9090
    nodePort: 30090
  selector:
    app: prometheus
`

	t, err := template.New("prometheus").Parse(tmpl)
	if err != nil {
		return err
	}

	f, err := os.Create(filepath.Join(k8sDir, "prometheus.yaml"))
	if err != nil {
		return err
	}
	defer f.Close()

	if err := t.Execute(f, map[string]any{"Targets": podIPs}); err != nil {
		return err
	}

	// Apply the updated config
	applyCmd := exec.Command(kubectlCmd, "apply", "-f", filepath.Join(k8sDir, "prometheus.yaml"))
	applyCmd.Stdout = nil
	applyCmd.Stderr = nil
	if err := applyCmd.Run(); err != nil {
		return err
	}

	// Restart Prometheus to pick up new config
	restartCmd := exec.Command(kubectlCmd, "rollout", "restart", "deployment/prometheus")
	restartCmd.Stdout = nil
	restartCmd.Stderr = nil
	restartCmd.Run()

	// Wait for Prometheus to be ready
	waitCmd := exec.Command(kubectlCmd, "wait", "--for=condition=ready", "pod", "-l", "app=prometheus", "--timeout=60s")
	waitCmd.Stdout = nil
	waitCmd.Stderr = nil
	waitCmd.Run()

	logSuccess("Prometheus configured with %d orchestrator targets", len(podIPs))
	return nil
}

func setupPortForwarding() {
	logInfo("Setting up port forwarding...")

	// Kill existing port-forwards
	exec.Command("pkill", "-9", "-f", "kubectl port-forward").Run()
	time.Sleep(2 * time.Second)

	kubectlCmd, _ := utils.FindCommand("kubectl")

	// Start port-forwards in background
	services := []struct {
		service    string
		localPort  string
		remotePort string
	}{
		{"orchestrator", "8080", "8080"},
		{"prometheus", "9091", "9090"},
		{"grafana", "3000", "3000"},
	}

	for _, svc := range services {
		cmd := exec.Command(kubectlCmd, "port-forward", fmt.Sprintf("svc/%s", svc.service), fmt.Sprintf("%s:%s", svc.localPort, svc.remotePort))
		cmd.Start()
		time.Sleep(1 * time.Second)
	}

	logSuccess("Port forwarding active")
	fmt.Printf("  Orchestrator: %shttp://localhost:8080%s\n", utils.CYAN, utils.NC)
	fmt.Printf("  Prometheus:   %shttp://localhost:9091%s\n", utils.CYAN, utils.NC)
	fmt.Printf("  Grafana:      %shttp://localhost:3000%s (admin/admin)\n", utils.CYAN, utils.NC)
}

func cleanupResources() {
	logInfo("Cleaning up Kubernetes resources...")
	projectRoot := getProjectRoot()
	k8sDir := filepath.Join(projectRoot, "k8s")
	kubectlCmd, _ := utils.FindCommand("kubectl")

	exec.Command(kubectlCmd, "delete", "-f", filepath.Join(k8sDir, "orchestrator", "orchestrator.yaml"), "--ignore-not-found=true").Run()
	exec.Command(kubectlCmd, "delete", "-f", filepath.Join(k8sDir, "monitoring", "prometheus.yaml"), "--ignore-not-found=true").Run()
	exec.Command(kubectlCmd, "delete", "-f", filepath.Join(k8sDir, "monitoring", "grafana.yaml"), "--ignore-not-found=true").Run()
	exec.Command("pkill", "-f", "kubectl port-forward").Run()

	// Cleanup Prometheus target files
	if err := cleanupTargetFiles(projectRoot); err == nil {
		logInfo("Cleaned up Prometheus target files")
	}

	logSuccess("Kubernetes resources cleaned up")
}

func generateTargetFiles(projectRoot string) error {
	return generateTargetFilesWithTestID(projectRoot, "")
}

func generateTargetFilesWithTestID(projectRoot string, testID string) error {
	logInfo("Generating Prometheus target files for debugging...")
	kubectlCmd, _ := utils.FindCommand("kubectl")

	// Get all orchestrator pod IPs and names
	cmd := exec.Command(kubectlCmd, "get", "pods", "-l", "app=orchestrator", "-o", "jsonpath={range .items[*]}{.metadata.name}{\"|\"}{.status.podIP}{\"\\n\"}{end}")
	output, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("failed to get pod info: %w", err)
	}

	targetsDir := filepath.Join(projectRoot, "config", "prometheus", "targets")
	os.MkdirAll(targetsDir, 0755)

	// Clear old target files
	files, _ := os.ReadDir(targetsDir)
	for _, f := range files {
		if strings.HasSuffix(f.Name(), ".json") {
			os.Remove(filepath.Join(targetsDir, f.Name()))
		}
	}

	// Create target files for each pod
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

		// Extract partition ID from pod name
		partitionID := "0"
		if idx := strings.LastIndex(podName, "-"); idx >= 0 {
			partitionID = podName[idx+1:]
		}

		targetFile := filepath.Join(targetsDir, fmt.Sprintf("partition-%s.json", partitionID))

		// Build labels JSON
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
			logWarning("Failed to write target file %s: %v", targetFile, err)
			continue
		}
	}

	logSuccess("Generated Prometheus target files in %s", targetsDir)
	return nil
}

func cleanupTargetFiles(projectRoot string) error {
	targetsDir := filepath.Join(projectRoot, "config", "prometheus", "targets")
	files, err := os.ReadDir(targetsDir)
	if err != nil {
		return err
	}

	for _, f := range files {
		if strings.HasSuffix(f.Name(), ".json") {
			os.Remove(filepath.Join(targetsDir, f.Name()))
		}
	}
	return nil
}

func verifyPartitionIDs() {
	logInfo("Verifying partition IDs in pods...")
	kubectlCmd, _ := utils.FindCommand("kubectl")

	// Get all orchestrator pods
	cmd := exec.Command(kubectlCmd, "get", "pods", "-l", "app=orchestrator", "-o", "jsonpath={range .items[*]}{.metadata.name}{\"\\n\"}{end}")
	output, err := cmd.Output()
	if err != nil {
		logWarning("Failed to get pod names: %v", err)
		return
	}

	podNames := strings.Split(strings.TrimSpace(string(output)), "\n")
	logInfo("Found %d orchestrator pods", len(podNames))

	// Check logs for partition ID
	for _, podName := range podNames {
		podName = strings.TrimSpace(podName)
		if podName == "" {
			continue
		}

		// Extract expected partition ID
		expectedPartition := "0"
		if idx := strings.LastIndex(podName, "-"); idx >= 0 {
			expectedPartition = podName[idx+1:]
		}

		// Check pod logs for partition ID (get more lines to catch startup messages)
		cmd := exec.Command(kubectlCmd, "logs", podName, "--tail=20", "--timestamps=false")
		logs, _ := cmd.Output()
		logStr := string(logs)

		// Also check the health endpoint to see partition_id
		cmd = exec.Command(kubectlCmd, "exec", podName, "--", "wget", "-qO-", "http://localhost:8080/healthz", "2>/dev/null")
		health, _ := cmd.Output()
		healthStr := string(health)

		// Check metrics endpoint
		cmd = exec.Command(kubectlCmd, "exec", podName, "--", "wget", "-qO-", "http://localhost:8080/metrics", "2>/dev/null")
		metrics, _ := cmd.Output()
		metricsStr := string(metrics)

		// Check health endpoint for partition_id
		foundInHealth := strings.Contains(healthStr, fmt.Sprintf("\"partition_id\":%s", expectedPartition))

		// Check for partition ID in logs (look for "partition X" or "[HTTP] Partition config: ID=X")
		foundInLogs := strings.Contains(logStr, fmt.Sprintf("partition %s", expectedPartition)) ||
			strings.Contains(logStr, fmt.Sprintf("ID=%s", expectedPartition)) ||
			strings.Contains(logStr, fmt.Sprintf("Partition config: ID=%s", expectedPartition))

		// Check metrics for partition label
		foundInMetrics := strings.Contains(metricsStr, fmt.Sprintf("partition=\"%s\"", expectedPartition))

		if foundInHealth || foundInLogs || foundInMetrics {
			logSuccess("Pod %s: Partition ID %s confirmed", podName, expectedPartition)
		} else {
			// Try to find what partition ID is actually being used
			if strings.Contains(healthStr, "\"partition_id\":0") && expectedPartition != "0" {
				logError("Pod %s: Health endpoint shows partition_id=0 but expected %s. PARTITION_ID env var is not set!", podName, expectedPartition)
			} else if strings.Contains(metricsStr, "partition=\"0\"") && expectedPartition != "0" {
				logError("Pod %s: Metrics show partition=0 but expected %s. PARTITION_ID may not be set correctly!", podName, expectedPartition)
			} else if strings.Contains(logStr, "Partition config:") {
				// Extract actual partition ID from log
				if strings.Contains(logStr, "ID=0") && expectedPartition != "0" {
					logError("Pod %s: Logs show partition ID=0 but expected %s", podName, expectedPartition)
				}
			} else {
				logWarning("Pod %s: Could not verify partition ID (expected %s). Health: %s", podName, expectedPartition, healthStr[:min(100, len(healthStr))])
			}
		}
	}
}

func logInfo(format string, args ...any) {
	fmt.Printf("%s[INFO]%s %s\n", utils.BLUE, utils.NC, fmt.Sprintf(format, args...))
}

func logSuccess(format string, args ...any) {
	fmt.Printf("%s[✓]%s %s\n", utils.GREEN, utils.NC, fmt.Sprintf(format, args...))
}

func logWarning(format string, args ...any) {
	fmt.Printf("%s[⚠]%s %s\n", utils.YELLOW, utils.NC, fmt.Sprintf(format, args...))
}

func logError(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "%s[✗]%s %s\n", utils.RED, utils.NC, fmt.Sprintf(format, args...))
}

func logConfig(format string, args ...any) {
	fmt.Printf("%s[CONFIG]%s %s\n", utils.MAGENTA, utils.NC, fmt.Sprintf(format, args...))
}

func checkMetricsPartitions(replicas int) {
	logInfo("Checking health endpoints for partition IDs...")
	kubectlCmd, _ := utils.FindCommand("kubectl")

	// Get all orchestrator pods
	cmd := exec.Command(kubectlCmd, "get", "pods", "-l", "app=orchestrator", "-o", "jsonpath={range .items[*]}{.metadata.name}{\"\\n\"}{end}")
	output, err := cmd.Output()
	if err != nil {
		return
	}

	podNames := strings.Split(strings.TrimSpace(string(output)), "\n")
	partitionsFound := make(map[int]bool)
	partitionMap := make(map[string]int) // pod -> partition

	for _, podName := range podNames {
		podName = strings.TrimSpace(podName)
		if podName == "" {
			continue
		}

		// Check health endpoint for partition_id
		cmd := exec.Command(kubectlCmd, "exec", podName, "--", "wget", "-qO-", "http://localhost:8080/healthz", "2>/dev/null")
		health, _ := cmd.Output()

		// Parse JSON to extract partition_id
		var healthData map[string]any
		if err := json.Unmarshal(health, &healthData); err == nil {
			if pid, ok := healthData["partition_id"].(float64); ok {
				partitionID := int(pid)
				partitionsFound[partitionID] = true
				partitionMap[podName] = partitionID
			}
		}
	}

	// Report findings
	if len(partitionsFound) == 0 {
		logError("No partition IDs found in health endpoints. All pods may be using partition 0!")
	} else if len(partitionsFound) == 1 {
		var pid int
		for p := range partitionsFound {
			pid = p
		}
		if pid == 0 {
			logError("All pods are reporting partition_id=0! PARTITION_ID environment variable is not being set correctly.")
			logInfo("Expected partitions: 0-%d, but all pods show partition 0", replicas-1)
		} else {
			logWarning("Only found partition %d (expected 0-%d)", pid, replicas-1)
		}
	} else if len(partitionsFound) < replicas {
		logWarning("Only found %d unique partitions (expected %d): %v", len(partitionsFound), replicas, partitionsFound)
		for pod, pid := range partitionMap {
			logInfo("  %s -> partition %d", pod, pid)
		}
	} else {
		logSuccess("Found all %d unique partitions: %v", len(partitionsFound), partitionsFound)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func getProjectRoot() string {
	dir, _ := os.Getwd()
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "."
}

func setupCluster(clusterName string) error {
	// Ensure Docker is running
	if err := utils.EnsureDockerRunning(); err != nil {
		return fmt.Errorf("Docker check failed: %w", err)
	}

	// Find kind and kubectl
	kindCmd, err := utils.FindCommand("kind")
	if err != nil {
		return fmt.Errorf("kind not found. Run: nix develop")
	}

	kubectlCmd, err := utils.FindCommand("kubectl")
	if err != nil {
		return fmt.Errorf("kubectl not found. Run: nix develop")
	}

	// Check if cluster exists
	cmd := exec.Command(kindCmd, "get", "clusters")
	output, err := cmd.Output()
	if err == nil {
		clusters := strings.SplitSeq(string(output), "\n")
		for cluster := range clusters {
			if strings.TrimSpace(cluster) == clusterName {
				fmt.Printf("✓ Cluster '%s' already exists\n", clusterName)
				// Set context
				exec.Command(kubectlCmd, "config", "use-context", fmt.Sprintf("kind-%s", clusterName)).Run()
				return nil
			}
		}
	}

	// Create cluster
	fmt.Println("Creating Kubernetes cluster...")
	cmd = exec.Command(kindCmd, "create", "cluster", "--name", clusterName, "--wait", "5m")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to create cluster: %w", err)
	}

	// Set context
	exec.Command(kubectlCmd, "config", "use-context", fmt.Sprintf("kind-%s", clusterName)).Run()

	fmt.Printf("✓ Cluster ready: kind-%s\n", clusterName)
	return nil
}

func cleanupCluster(clusterName string) error {
	kindCmd, err := utils.FindCommand("kind")
	if err != nil {
		return fmt.Errorf("kind not found")
	}

	// Check if cluster exists
	cmd := exec.Command(kindCmd, "get", "clusters")
	output, err := cmd.Output()
	if err == nil {
		clusters := strings.SplitSeq(string(output), "\n")
		for cluster := range clusters {
			if strings.TrimSpace(cluster) == clusterName {
				// Delete cluster
				cmd = exec.Command(kindCmd, "delete", "cluster", "--name", clusterName)
				cmd.Stdout = os.Stdout
				cmd.Stderr = os.Stderr
				if err := cmd.Run(); err != nil {
					return fmt.Errorf("failed to delete cluster: %w", err)
				}

				// Delete context
				kubectlCmd, _ := utils.FindCommand("kubectl")
				exec.Command(kubectlCmd, "config", "delete-context", fmt.Sprintf("kind-%s", clusterName)).Run()

				fmt.Println("✓ Cluster deleted")
				return nil
			}
		}
	}

	fmt.Printf("Cluster '%s' does not exist\n", clusterName)
	return nil
}

func init() {
	// Setup signal handling for cleanup
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigChan
		exec.Command("pkill", "-f", "kubectl port-forward").Run()
		os.Exit(0)
	}()
}
