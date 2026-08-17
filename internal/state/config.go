package state

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/pelletier/go-toml/v2"
)

// syncIsolatedConfig shares desktop-managed settings and MCP servers with an
// isolated subscription while keeping its credentials and project trust local.
func syncIsolatedConfig(primaryCodexHome, isolatedCodexHome string) error {
	if isolatedCodexHome == "" {
		return errors.New("isolated Codex home is required")
	}
	if err := os.MkdirAll(isolatedCodexHome, 0o700); err != nil {
		return fmt.Errorf("create isolated Codex home: %w", err)
	}
	if err := os.Chmod(isolatedCodexHome, 0o700); err != nil {
		return fmt.Errorf("secure isolated Codex home: %w", err)
	}

	primaryConfig, err := readConfig(filepath.Join(primaryCodexHome, "config.toml"))
	if err != nil {
		return fmt.Errorf("read primary config: %w", err)
	}
	configPath := filepath.Join(isolatedCodexHome, "config.toml")
	isolatedConfig, err := readConfig(configPath)
	if err != nil {
		return fmt.Errorf("read isolated config: %w", err)
	}

	contents, err := isolatedConfigContents(primaryConfig, isolatedConfig)
	if err != nil {
		return err
	}
	if bytes.Equal(isolatedConfig, contents) {
		return nil
	}
	temporaryPath := configPath + ".tmp"
	if err := os.WriteFile(temporaryPath, contents, 0o600); err != nil {
		return fmt.Errorf("write temporary config: %w", err)
	}
	if err := os.Chmod(temporaryPath, 0o600); err != nil {
		return fmt.Errorf("secure temporary config: %w", err)
	}
	if err := os.Rename(temporaryPath, configPath); err != nil {
		return fmt.Errorf("commit config: %w", err)
	}
	return nil
}

func isolatedConfigContents(primaryConfig, isolatedConfig []byte) ([]byte, error) {
	managed := make(map[string]any)
	if len(bytes.TrimSpace(primaryConfig)) > 0 {
		if err := toml.Unmarshal(primaryConfig, &managed); err != nil {
			return nil, fmt.Errorf("parse primary config: %w", err)
		}
	}
	local := make(map[string]any)
	if len(bytes.TrimSpace(isolatedConfig)) > 0 {
		if err := toml.Unmarshal(isolatedConfig, &local); err != nil {
			return nil, fmt.Errorf("parse isolated config: %w", err)
		}
	}

	delete(managed, "projects")
	managed["cli_auth_credentials_store"] = "file"
	managed["mcp_oauth_credentials_store"] = "file"
	if projects, ok := local["projects"]; ok {
		managed["projects"] = projects
	}
	contents, err := toml.Marshal(managed)
	if err != nil {
		return nil, fmt.Errorf("encode isolated config: %w", err)
	}
	return contents, nil
}

func readConfig(path string) ([]byte, error) {
	contents, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return contents, err
}

func samePath(left, right string) bool {
	if left == "" || right == "" {
		return false
	}
	leftAbsolute, leftErr := filepath.Abs(left)
	rightAbsolute, rightErr := filepath.Abs(right)
	if leftErr != nil || rightErr != nil {
		return filepath.Clean(left) == filepath.Clean(right)
	}
	return filepath.Clean(leftAbsolute) == filepath.Clean(rightAbsolute)
}
