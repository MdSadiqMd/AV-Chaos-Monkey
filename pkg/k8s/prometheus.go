package k8s

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"
	"time"

	"github.com/MdSadiqMd/AV-Chaos-Monkey/pkg/logging"
	"github.com/MdSadiqMd/AV-Chaos-Monkey/pkg/utils"
)

func UpdatePrometheusTargets(replicas int, projectRoot string) error {
	logging.LogInfo("Configuring Prometheus with orchestrator pod IPs...")
	kubectlCmd, _ := utils.FindCommand("kubectl")
	time.Sleep(3 * time.Second)

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
			logging.LogInfo("Waiting for pod IPs... (got %d/%d)", len(podIPs), replicas)
			time.Sleep(2 * time.Second)
		}
	}

	if len(podIPs) == 0 {
		return fmt.Errorf("no pod IPs found")
	}
	if len(podIPs) < replicas {
		logging.LogWarning("Only got %d pod IPs but expected %d replicas", len(podIPs), replicas)
	}

	logging.LogInfo("Found %d orchestrator pod IPs: %v", len(podIPs), podIPs)

	k8sDir := filepath.Join(projectRoot, "k8s", "monitoring")
	tmpl := PrometheusTemplate()
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

	applyCmd := exec.Command(kubectlCmd, "apply", "-f", filepath.Join(k8sDir, "prometheus.yaml"))
	applyCmd.Stdout = nil
	applyCmd.Stderr = nil
	if err := applyCmd.Run(); err != nil {
		return err
	}

	restartCmd := exec.Command(kubectlCmd, "rollout", "restart", "deployment/prometheus")
	restartCmd.Stdout = nil
	restartCmd.Stderr = nil
	restartCmd.Run()

	waitCmd := exec.Command(kubectlCmd, "wait", "--for=condition=ready", "pod", "-l", "app=prometheus", "--timeout=60s")
	waitCmd.Stdout = nil
	waitCmd.Stderr = nil
	waitCmd.Run()

	logging.LogSuccess("Prometheus configured with %d orchestrator targets", len(podIPs))
	return nil
}

func PrometheusTemplate() string {
	return `apiVersion: v1
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
}
