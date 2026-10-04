package sandbox

import (
	"context"
	"errors"
	"fmt"

	"github.com/hyscale-lab/aries/internal/app"
	"github.com/hyscale-lab/aries/pkg/config"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/monitor"
	nvidiamonitor "github.com/hyscale-lab/aries/pkg/monitor/nvidia"
	sandlocksandbox "github.com/hyscale-lab/aries/pkg/sandbox/sandlock"
	"github.com/sirupsen/logrus"
)

// NewSandlock constructs a host-process sandbox. The harness stays on its own
// deployment. Task commands do not join that network, so the bridge address is
// the host loopback.
func NewSandlock(cfg config.Config, outputRoot, occurrenceID string, gpuIndices []int, logger *logrus.Logger) (app.SandboxInstance, error) {
	manager, err := sandlocksandbox.New(sandlocksandbox.Options{
		OutputDir:     outputRoot,
		Logger:        logger,
		NetAllow:      cfg.Sandbox.Sandlock.NetAllow,
		FSDenied:      cfg.Sandbox.Sandlock.FSDenied,
		MaxProcesses:  cfg.Sandbox.Sandlock.MaxProcesses,
		MaxOpenFiles:  cfg.Sandbox.Sandlock.MaxOpenFiles,
		MaxCPUPercent: cfg.Sandbox.Sandlock.MaxCPUPercent,
		SeedWorkspace: seedFromImage,
	})
	if err != nil {
		return app.SandboxInstance{}, fmt.Errorf("construct Sandlock sandbox: %w", err)
	}
	source := sandlocksandbox.NewResourceSource(occurrenceID, "sandlock", 0)
	var resources monitor.ResourceSource = source
	if len(gpuIndices) != 0 {
		gpuSource, err := nvidiamonitor.NewSource(nvidiamonitor.Options{TaskID: occurrenceID, GPUIndices: gpuIndices})
		if err != nil {
			return app.SandboxInstance{}, errors.Join(fmt.Errorf("construct NVIDIA resource source: %w", err), manager.Close())
		}
		resources = &combinedResourceSource{container: source, gpu: gpuSource}
	}
	return app.SandboxInstance{
		Sandbox:   manager,
		Resources: resources,
		Close:     manager.Close,
		BridgeListen: func(context.Context) (core.BridgeListen, error) {
			return core.BridgeListen{BindHost: "127.0.0.1", AdvertiseHost: "127.0.0.1"}, nil
		},
	}, nil
}
