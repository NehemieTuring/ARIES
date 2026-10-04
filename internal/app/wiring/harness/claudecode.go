package harness

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/hyscale-lab/aries/internal/app"
	"github.com/hyscale-lab/aries/pkg/config"
	"github.com/hyscale-lab/aries/pkg/deployment"
	claudecodeharness "github.com/hyscale-lab/aries/pkg/harness/claudecode"
	"github.com/sirupsen/logrus"
)

// NewClaudeCode takes ownership of transport, including closing it on construction failure.
func NewClaudeCode(cfg config.Config, outputRoot string, lookup func(string) ([]byte, bool), logger *logrus.Logger, transport deployment.Deployment) (app.HarnessInstance, error) {
	mcpFilesBinaryPath, err := resolveMCPFilesBinaryPath()
	if err != nil {
		return app.HarnessInstance{}, errors.Join(fmt.Errorf("resolve Claude Code MCP file-tools binary: %w", err), transport.Close())
	}
	manager, err := claudecodeharness.New(claudecodeharness.Options{
		Deployment: transport, Image: cfg.Versions.ClaudeCode.Image, OutputDir: outputRoot, APIKeyLookup: lookup, Logger: logger,
		MCPFilesBinaryPath: mcpFilesBinaryPath,
	})
	if err != nil {
		return app.HarnessInstance{}, errors.Join(fmt.Errorf("construct Claude Code harness: %w", err), transport.Close())
	}
	return app.HarnessInstance{Harness: manager, Close: manager.Close}, nil
}

// resolveMCPFilesBinaryPath locates the compiled cmd/aries-claudecode-mcpfiles
// binary. The harness copies it into the runtime. ARIES_CLAUDE_CODE_MCPFILES_BIN
// overrides the sibling binary next to this process.
func resolveMCPFilesBinaryPath() (string, error) {
	if override := os.Getenv("ARIES_CLAUDE_CODE_MCPFILES_BIN"); override != "" {
		return override, nil
	}
	executable, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve aries executable path: %w", err)
	}
	return filepath.Join(filepath.Dir(executable), "aries-claudecode-mcpfiles"), nil
}
