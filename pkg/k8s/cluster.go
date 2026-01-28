package k8s

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/MdSadiqMd/AV-Chaos-Monkey/pkg/logging"
	"github.com/MdSadiqMd/AV-Chaos-Monkey/pkg/utils"
)

const DefaultClusterName = "av-chaos-monkey"

func SetupCluster(clusterName string) error {
	if err := utils.EnsureDockerRunning(); err != nil {
		return fmt.Errorf("Docker check failed: %w", err)
	}

	kindCmd, err := utils.FindCommand("kind")
	if err != nil {
		return fmt.Errorf("kind not found. Run: nix develop")
	}

	kubectlCmd, err := utils.FindCommand("kubectl")
	if err != nil {
		return fmt.Errorf("kubectl not found. Run: nix develop")
	}

	cmd := exec.Command(kindCmd, "get", "clusters")
	output, err := cmd.Output()
	if err == nil {
		clusters := strings.SplitSeq(string(output), "\n")
		for cluster := range clusters {
			if strings.TrimSpace(cluster) == clusterName {
				fmt.Printf("✓ Cluster '%s' already exists\n", clusterName)
				exec.Command(kubectlCmd, "config", "use-context", fmt.Sprintf("kind-%s", clusterName)).Run()
				return nil
			}
		}
	}

	fmt.Println("Creating Kubernetes cluster...")
	cmd = exec.Command(kindCmd, "create", "cluster", "--name", clusterName, "--wait", "5m")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to create cluster: %w", err)
	}

	exec.Command(kubectlCmd, "config", "use-context", fmt.Sprintf("kind-%s", clusterName)).Run()
	fmt.Printf("✓ Cluster ready: kind-%s\n", clusterName)
	return nil
}

func CleanupCluster(clusterName string) error {
	kindCmd, err := utils.FindCommand("kind")
	if err != nil {
		return fmt.Errorf("kind not found")
	}

	cmd := exec.Command(kindCmd, "get", "clusters")
	output, err := cmd.Output()
	if err == nil {
		clusters := strings.SplitSeq(string(output), "\n")
		for cluster := range clusters {
			if strings.TrimSpace(cluster) == clusterName {
				cmd = exec.Command(kindCmd, "delete", "cluster", "--name", clusterName)
				cmd.Stdout = os.Stdout
				cmd.Stderr = os.Stderr
				if err := cmd.Run(); err != nil {
					return fmt.Errorf("failed to delete cluster: %w", err)
				}
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

func CheckPrerequisites(clusterName string) error {
	logging.LogInfo("Checking prerequisites...")

	if !utils.CommandExists("kubectl") || !utils.CommandExists("kind") {
		return fmt.Errorf("kubectl/kind not found. Make sure you're in Nix devShell: nix develop")
	}

	if err := utils.EnsureDockerRunning(); err != nil {
		return fmt.Errorf("Docker check failed: %w", err)
	}

	if clusterName == "" {
		clusterName = utils.GetEnvOrDefault("CLUSTER_NAME", DefaultClusterName)
	}

	kindCmd, _ := utils.FindCommand("kind")
	cmd := exec.Command(kindCmd, "get", "clusters")
	output, err := cmd.Output()
	if err != nil || !strings.Contains(string(output), clusterName) {
		logging.LogInfo("Setting up Kubernetes cluster...")
		if err := SetupCluster(clusterName); err != nil {
			return fmt.Errorf("failed to setup cluster: %w", err)
		}
	}

	kubectlCmd, _ := utils.FindCommand("kubectl")
	exec.Command(kubectlCmd, "config", "use-context", fmt.Sprintf("kind-%s", clusterName)).Run()

	logging.LogSuccess("Prerequisites OK")
	return nil
}
