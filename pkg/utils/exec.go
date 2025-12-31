package utils

import (
	"fmt"
	"os/exec"
	"strings"
)

func FindCommand(name string) (string, error) {
	cmd := exec.Command("command", "-v", name)
	output, err := cmd.Output()
	if err == nil && len(output) > 0 {
		return strings.TrimSpace(string(output)), nil
	}

	cmd = exec.Command("which", name)
	output, err = cmd.Output()
	if err == nil && len(output) > 0 {
		return strings.TrimSpace(string(output)), nil
	}

	return name, nil
}

func CommandExists(name string) bool {
	cmd := exec.Command("command", "-v", name)
	cmd.Stdout = nil
	cmd.Stderr = nil
	return cmd.Run() == nil
}

func RunCommand(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("failed to run %s: %w", name, err)
	}
	return strings.TrimSpace(string(output)), nil
}

func RunCommandSilent(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = nil
	cmd.Stderr = nil
	return cmd.Run()
}
