package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveProjectPath(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "sqd-go-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	t.Run("config.yaml", func(t *testing.T) {
		dir := filepath.Join(tmpDir, "yaml")
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatalf("failed to create dir: %v", err)
		}
		configPath := filepath.Join(dir, "config.yaml")
		if err := os.WriteFile(configPath, []byte("name: test"), 0644); err != nil {
			t.Fatalf("failed to write config: %v", err)
		}

		root, resolved, err := ResolveProjectPath(dir)
		if err != nil {
			t.Errorf("ResolveProjectPath() error = %v", err)
			return
		}
		if root != dir {
			t.Errorf("root = %v, want %v", root, dir)
		}
		if resolved != configPath {
			t.Errorf("resolved = %v, want %v", resolved, configPath)
		}
	})

	t.Run("config.yml", func(t *testing.T) {
		dir := filepath.Join(tmpDir, "yml")
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatalf("failed to create dir: %v", err)
		}
		configPath := filepath.Join(dir, "config.yml")
		if err := os.WriteFile(configPath, []byte("name: test"), 0644); err != nil {
			t.Fatalf("failed to write config: %v", err)
		}

		root, resolved, err := ResolveProjectPath(dir)
		if err != nil {
			t.Errorf("ResolveProjectPath() error = %v", err)
			return
		}
		if root != dir {
			t.Errorf("root = %v, want %v", root, dir)
		}
		if resolved != configPath {
			t.Errorf("resolved = %v, want %v", resolved, configPath)
		}
	})

	t.Run("both_prefers_yaml", func(t *testing.T) {
		dir := filepath.Join(tmpDir, "both")
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatalf("failed to create dir: %v", err)
		}
		yamlPath := filepath.Join(dir, "config.yaml")
		if err := os.WriteFile(yamlPath, []byte("name: yaml"), 0644); err != nil {
			t.Fatalf("failed to write config: %v", err)
		}
		ymlPath := filepath.Join(dir, "config.yml")
		if err := os.WriteFile(ymlPath, []byte("name: yml"), 0644); err != nil {
			t.Fatalf("failed to write config: %v", err)
		}

		_, resolved, err := ResolveProjectPath(dir)
		if err != nil {
			t.Errorf("ResolveProjectPath() error = %v", err)
			return
		}
		if resolved != yamlPath {
			t.Errorf("resolved = %v, want %v", resolved, yamlPath)
		}
	})
}
