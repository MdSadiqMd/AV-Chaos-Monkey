package k8s

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"github.com/MdSadiqMd/AV-Chaos-Monkey/pkg/logging"
)

type ResourceConfig struct {
	OrchestratorMemRequest string
	OrchestratorMemLimit   string
	OrchestratorCPURequest string
	OrchestratorCPULimit   string
	CoturnReplicas         int
	CoturnMemRequest       string
	CoturnMemLimit         string
	CoturnCPURequest       string
	CoturnCPULimit         string
	PrometheusMemRequest   string
	PrometheusMemLimit     string
	GrafanaMemRequest      string
	GrafanaMemLimit        string
}

func CalculateResourceConfig(dockerMemGB int) ResourceConfig {
	var tier string
	var config ResourceConfig

	switch {
	case dockerMemGB >= 16:
		tier, config = "high", ResourceConfig{
			OrchestratorMemRequest: "2Gi", OrchestratorMemLimit: "4Gi",
			OrchestratorCPURequest: "1", OrchestratorCPULimit: "4",
			CoturnReplicas: 3, CoturnMemRequest: "512Mi", CoturnMemLimit: "1Gi",
			CoturnCPURequest: "500m", CoturnCPULimit: "2000m",
			PrometheusMemRequest: "256Mi", PrometheusMemLimit: "1Gi",
			GrafanaMemRequest: "128Mi", GrafanaMemLimit: "512Mi",
		}
	case dockerMemGB >= 8:
		tier, config = "medium", ResourceConfig{
			OrchestratorMemRequest: "1Gi", OrchestratorMemLimit: "2Gi",
			OrchestratorCPURequest: "500m", OrchestratorCPULimit: "2",
			CoturnReplicas: 2, CoturnMemRequest: "256Mi", CoturnMemLimit: "512Mi",
			CoturnCPURequest: "250m", CoturnCPULimit: "1000m",
			PrometheusMemRequest: "128Mi", PrometheusMemLimit: "512Mi",
			GrafanaMemRequest: "128Mi", GrafanaMemLimit: "256Mi",
		}
	case dockerMemGB >= 4:
		tier, config = "low", ResourceConfig{
			OrchestratorMemRequest: "512Mi", OrchestratorMemLimit: "1Gi",
			OrchestratorCPURequest: "250m", OrchestratorCPULimit: "1",
			CoturnReplicas: 1, CoturnMemRequest: "128Mi", CoturnMemLimit: "256Mi",
			CoturnCPURequest: "100m", CoturnCPULimit: "500m",
			PrometheusMemRequest: "64Mi", PrometheusMemLimit: "256Mi",
			GrafanaMemRequest: "64Mi", GrafanaMemLimit: "128Mi",
		}
	default:
		tier, config = "minimal", ResourceConfig{
			OrchestratorMemRequest: "256Mi", OrchestratorMemLimit: "512Mi",
			OrchestratorCPURequest: "100m", OrchestratorCPULimit: "500m",
			CoturnReplicas: 1, CoturnMemRequest: "64Mi", CoturnMemLimit: "128Mi",
			CoturnCPURequest: "50m", CoturnCPULimit: "250m",
			PrometheusMemRequest: "32Mi", PrometheusMemLimit: "128Mi",
			GrafanaMemRequest: "32Mi", GrafanaMemLimit: "64Mi",
		}
	}

	logging.LogInfo("Resource tier: %s (%dGB Docker memory detected)", tier, dockerMemGB)
	return config
}

func UpdateResourceLimits(projectRoot string, dockerMemGB int) error {
	config := CalculateResourceConfig(dockerMemGB)
	k8sDir := filepath.Join(projectRoot, "k8s")

	updates := []struct {
		name string
		path string
		fn   func(string, ResourceConfig) error
	}{
		{"orchestrator", filepath.Join(k8sDir, "orchestrator", "orchestrator.yaml"), updateOrchestratorResources},
		{"coturn", filepath.Join(k8sDir, "coturn", "coturn.yaml"), updateCoturnResources},
		{"prometheus", filepath.Join(k8sDir, "monitoring", "prometheus.yaml"), updateMonitoringResources(config.PrometheusMemRequest, config.PrometheusMemLimit)},
		{"grafana", filepath.Join(k8sDir, "monitoring", "grafana.yaml"), updateMonitoringResources(config.GrafanaMemRequest, config.GrafanaMemLimit)},
	}

	for _, u := range updates {
		if err := u.fn(u.path, config); err != nil {
			return fmt.Errorf("failed to update %s resources: %w", u.name, err)
		}
	}

	logging.LogSuccess("Resource limits configured (orchestrator: %s/%s, coturn: %dx%s)",
		config.OrchestratorMemRequest, config.OrchestratorMemLimit,
		config.CoturnReplicas, config.CoturnMemLimit)
	return nil
}

func updateOrchestratorResources(path string, config ResourceConfig) error {
	content, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	re := regexp.MustCompile(`(?s)(resources:\s+requests:\s+cpu: )"[^"]*"(\s+memory: )"[^"]*"(\s+limits:\s+cpu: )"[^"]*"(\s+memory: )"[^"]*"`)
	contentStr := re.ReplaceAllString(string(content),
		fmt.Sprintf(`${1}"%s"${2}"%s"${3}"%s"${4}"%s"`,
			config.OrchestratorCPURequest, config.OrchestratorMemRequest,
			config.OrchestratorCPULimit, config.OrchestratorMemLimit))
	return os.WriteFile(path, []byte(contentStr), 0644)
}

func updateCoturnResources(path string, config ResourceConfig) error {
	content, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	contentStr := string(content)

	// Update replicas
	replicasRe := regexp.MustCompile(`(kind: StatefulSet[\s\S]*?spec:\s+serviceName: coturn\s+replicas: )\d+`)
	contentStr = replicasRe.ReplaceAllString(contentStr, fmt.Sprintf("${1}%d", config.CoturnReplicas))

	// Update resources
	resourcesRe := regexp.MustCompile(`(?s)(resources:\s+requests:\s+cpu: )"[^"]*"(\s+memory: )"[^"]*"(\s+limits:\s+cpu: )"[^"]*"(\s+memory: )"[^"]*"`)
	contentStr = resourcesRe.ReplaceAllString(contentStr,
		fmt.Sprintf(`${1}"%s"${2}"%s"${3}"%s"${4}"%s"`,
			config.CoturnCPURequest, config.CoturnMemRequest,
			config.CoturnCPULimit, config.CoturnMemLimit))
	return os.WriteFile(path, []byte(contentStr), 0644)
}

func updateMonitoringResources(memRequest, memLimit string) func(string, ResourceConfig) error {
	return func(path string, _ ResourceConfig) error {
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		contentStr := string(content)
		if !contains(contentStr, "resources:") {
			return nil
		}
		re := regexp.MustCompile(`(?s)(resources:\s+requests:\s+cpu: )"[^"]*"(\s+memory: )"[^"]*"(\s+limits:\s+cpu: )"[^"]*"(\s+memory: )"[^"]*"`)
		contentStr = re.ReplaceAllString(contentStr,
			fmt.Sprintf(`${1}"100m"${2}"%s"${3}"500m"${4}"%s"`, memRequest, memLimit))
		return os.WriteFile(path, []byte(contentStr), 0644)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsAt(s, substr))
}

func containsAt(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
