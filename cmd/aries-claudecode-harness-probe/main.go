// Command aries-claudecode-harness-probe is a throwaway diagnostic tool, NOT
// part of the ARIES CLI surface. Unlike cmd/aries-claudecode-probe (which only
// exercises the bridge), this drives the full pkg/harness/claudecode.Manager
// (Start/Run/Stop) against a real Docker sandbox, a real claudecodessh
// bridge, and a real Claude Code CLI invocation inside the
// aries-claudecode:test-1 image (see rapport_integration_claude_code.md for
// how that image was built). It costs real Anthropic API usage.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/hyscale-lab/aries/pkg/bridge/claudecodessh"
	"github.com/hyscale-lab/aries/pkg/core"
	claudecodeharness "github.com/hyscale-lab/aries/pkg/harness/claudecode"
	dockersandbox "github.com/hyscale-lab/aries/pkg/sandbox/docker"
	"github.com/sirupsen/logrus"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("probe failed: %v", err)
	}
}

func run() error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	outputDir := "/tmp/aries-claudecode-harness-probe-run"
	if err := os.MkdirAll(outputDir, 0o700); err != nil {
		return fmt.Errorf("create output dir: %w", err)
	}
	logger := logrus.StandardLogger()

	fmt.Println("=== 1. starting Docker sandbox (ubuntu:24.04) ===")
	sandboxManager, err := dockersandbox.New(dockersandbox.Options{OutputDir: outputDir, Logger: logger})
	if err != nil {
		return fmt.Errorf("construct sandbox manager: %w", err)
	}
	defer sandboxManager.Close()
	sandbox, err := sandboxManager.Start(ctx, core.SandboxRequest{
		RunID: "probe-run", TaskID: "probe-task",
		Environment: core.Environment{Image: "ubuntu:24.04", Workdir: "/root", CPU: 1, MemoryMB: 512, AllowNetwork: true},
	})
	if err != nil {
		return fmt.Errorf("start sandbox: %w", err)
	}
	defer func() {
		fmt.Println("=== cleanup: stopping sandbox ===")
		if err := sandboxManager.Stop(context.Background(), sandbox); err != nil {
			fmt.Println("sandbox stop error:", err)
		}
	}()

	fmt.Println("=== 2. starting claudecodessh bridge ===")
	bridge, err := claudecodessh.New(claudecodessh.Options{OutputDir: outputDir, Logger: logger, OmitRawLog: false})
	if err != nil {
		return fmt.Errorf("construct bridge: %w", err)
	}
	endpoint, err := bridge.Start(ctx, sandbox)
	if err != nil {
		return fmt.Errorf("start bridge: %w", err)
	}
	defer func() {
		fmt.Println("=== cleanup: stopping bridge ===")
		if err := bridge.Stop(context.Background()); err != nil {
			fmt.Println("bridge stop error:", err)
		}
	}()
	fmt.Printf("bridge endpoint: %+v\n", endpoint)

	fmt.Println("=== 3. starting Claude Code harness (aries-claudecode:test-1) ===")
	harness, err := claudecodeharness.New(claudecodeharness.Options{
		Image: "aries-claudecode:test-1", OutputDir: outputDir, Logger: logger,
	})
	if err != nil {
		return fmt.Errorf("construct harness: %w", err)
	}
	defer harness.Close()

	err = harness.Start(ctx, core.HarnessRequest{
		RunID: "probe-run", TaskID: "probe-task",
		Endpoint: endpoint,
		Model:    core.ModelConfig{Provider: "anthropic", Model: "claude-sonnet-5", APIKeyEnv: "ANTHROPIC_API_KEY"},
		Timeout:  90 * time.Second, OutputDir: outputDir,
	})
	if err != nil {
		return fmt.Errorf("start harness: %w", err)
	}
	defer func() {
		fmt.Println("=== cleanup: stopping harness ===")
		if err := harness.Stop(context.Background()); err != nil {
			fmt.Println("harness stop error:", err)
		}
	}()
	fmt.Println("harness started")

	fmt.Println("=== 4. running task ===")
	instruction := "Create a file named hello.txt in the current directory containing exactly the text: ARIES harness test. Then print its contents with cat."
	result, err := harness.Run(ctx, instruction)
	fmt.Printf("--- harness result ---\nstatus=%s\nfinal_response=%q\nduration=%s\nerror=%q\nlog_paths=%v\n",
		result.Status, result.FinalResponse, result.Duration, result.Error, result.LogPaths)
	if err != nil {
		fmt.Println("Run() returned error:", err)
	}

	fmt.Println("=== 5. verifying hello.txt in the sandbox ===")
	verify, verifyErr := sandbox.Exec(ctx, core.Command{Path: "/bin/cat", Args: []string{"/root/hello.txt"}})
	if verifyErr != nil {
		fmt.Println("verify exec error:", verifyErr)
	} else {
		fmt.Printf("sandbox hello.txt contents: %q (exit=%d)\n", verify.Stdout, verify.ExitCode)
	}

	if result.Status == core.StatusSucceeded && verifyErr == nil && verify.ExitCode == 0 {
		fmt.Println("=== RESULT: OK — full harness (Start/Run/Stop) worked end-to-end ===")
	} else {
		fmt.Println("=== RESULT: FAILED — see details above and artifacts under", outputDir, "===")
	}
	return nil
}
