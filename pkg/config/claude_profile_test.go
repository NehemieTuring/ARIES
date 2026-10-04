package config

import (
	"path/filepath"
	"testing"
)

func TestClaudeCodeProfilesLoad(t *testing.T) {
	root := filepath.Join("..", "..", "profiles")
	for _, name := range []string{
		"claudecode-tb2-fix-git-anthropic.json",
		"claudecode-tb2-overfull-hbox-anthropic.json",
	} {
		cfg, err := Load(filepath.Join(root, name))
		if err != nil {
			t.Fatalf("load %s: %v", name, err)
		}
		if cfg.Harness.Type != "claude-code" || cfg.Bridge.Type != "claude-code-ssh" || cfg.Runtime.Backend != "anthropic" {
			t.Fatalf("%s selectors = %s %s %s", name, cfg.Harness.Type, cfg.Bridge.Type, cfg.Runtime.Backend)
		}
		if cfg.Model.WorkspaceID == "" || cfg.Versions.ClaudeCode.Image == "" {
			t.Fatalf("%s missing workspace or image", name)
		}
	}
}
