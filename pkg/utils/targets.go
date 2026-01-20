package utils

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/MdSadiqMd/AV-Chaos-Monkey/pkg/config"
)

func UpdateTargetFilesWithTestID(testID string) error {
	projectRoot := config.GetProjectRoot()
	targetsDir := filepath.Join(projectRoot, "config", "prometheus", "targets")

	files, err := os.ReadDir(targetsDir)
	if err != nil {
		return err
	}

	for _, f := range files {
		if !strings.HasSuffix(f.Name(), ".json") {
			continue
		}

		filePath := filepath.Join(targetsDir, f.Name())
		content, err := os.ReadFile(filePath)
		if err != nil {
			continue
		}

		var targets []map[string]any
		if err := json.Unmarshal(content, &targets); err != nil {
			continue
		}

		for _, target := range targets {
			if labels, ok := target["labels"].(map[string]any); ok {
				labels["test_id"] = testID
			}
		}

		updatedJSON, err := json.MarshalIndent(targets, "", "  ")
		if err != nil {
			continue
		}

		os.WriteFile(filePath, updatedJSON, 0644)
	}

	return nil
}

func CleanupTargetFiles() error {
	projectRoot := config.GetProjectRoot()
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
