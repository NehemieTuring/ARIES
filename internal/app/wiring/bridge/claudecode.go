package bridge

import (
	"context"
	"fmt"

	"github.com/hyscale-lab/aries/pkg/bridge/claudecodessh"
	"github.com/hyscale-lab/aries/pkg/config"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/runner"
	"github.com/sirupsen/logrus"
)

// NewClaudeCode constructs the embedded Claude Code SSH bridge.
func NewClaudeCode(cfg config.Config, outputRoot string, resolveListen func(context.Context) (core.BridgeListen, error), logger *logrus.Logger) (runner.ToolBridge, error) {
	bridge, err := claudecodessh.New(claudecodessh.Options{
		OutputDir: outputRoot, Logger: logger, ResolveListen: resolveListen, OmitRawLog: !cfg.Bridge.RetainBridgeRawLog(),
	})
	if err != nil {
		return nil, fmt.Errorf("construct Claude Code SSH bridge: %w", err)
	}
	return bridge, nil
}
