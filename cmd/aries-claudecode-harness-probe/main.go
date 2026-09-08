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
	"os/exec"
	"strings"
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
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
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

	fmt.Println("=== 2b. building the companion MCP file-tools binary ===")
	mcpFilesBinaryPath, err := buildMCPFilesBinary(outputDir)
	if err != nil {
		return fmt.Errorf("build MCP file-tools binary: %w", err)
	}
	fmt.Println("built:", mcpFilesBinaryPath)

	fmt.Println("=== 3. starting Claude Code harness (aries-claudecode:test-1) ===")
	harness, err := claudecodeharness.New(claudecodeharness.Options{
		Image: "aries-claudecode:test-1", OutputDir: outputDir, Logger: logger,
		MCPFilesBinaryPath: mcpFilesBinaryPath,
	})
	if err != nil {
		return fmt.Errorf("construct harness: %w", err)
	}
	defer harness.Close()

	err = harness.Start(ctx, core.HarnessRequest{
		RunID: "probe-run", TaskID: "probe-task",
		Endpoint: endpoint,
		Model:    core.ModelConfig{Provider: "anthropic", Model: "claude-sonnet-5", APIKeyEnv: "ANTHROPIC_API_KEY", WorkspaceID: os.Getenv("ARIES_ANTHROPIC_WORKSPACE_ID")},
		Timeout:  150 * time.Second, OutputDir: outputDir,
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

	fmt.Println("=== 4. running task (exercises write_file + edit_file, not just Bash) ===")
	instruction := "Create a file named greeting.txt in the current directory containing exactly the text: Hello ARIES (no trailing period or newline beyond one). " +
		"Then edit that file to replace the word 'Hello' with 'Hi'. Then print the final contents of greeting.txt using cat."
	result, err := harness.Run(ctx, instruction)
	fmt.Printf("--- harness result ---\nstatus=%s\nfinal_response=%q\nduration=%s\nerror=%q\nlog_paths=%v\n",
		result.Status, result.FinalResponse, result.Duration, result.Error, result.LogPaths)
	if err != nil {
		fmt.Println("Run() returned error:", err)
	}

	fmt.Println("=== 5. verifying greeting.txt in the sandbox (must be 'Hi ARIES', written+edited via the MCP file tools, not the harness's own filesystem) ===")
	verify, verifyErr := sandbox.Exec(ctx, core.Command{Path: "/bin/cat", Args: []string{"/root/greeting.txt"}})
	if verifyErr != nil {
		fmt.Println("verify exec error:", verifyErr)
	} else {
		fmt.Printf("sandbox greeting.txt contents: %q (exit=%d)\n", verify.Stdout, verify.ExitCode)
	}

	editedCorrectly := verifyErr == nil && verify.ExitCode == 0 && strings.Contains(verify.Stdout, "Hi ARIES")
	if result.Status == core.StatusSucceeded && editedCorrectly {
		fmt.Println("=== RESULT: OK — full harness (Start/Run/Stop) worked end-to-end, write_file+edit_file landed in the sandbox ===")
	} else {
		fmt.Println("=== RESULT: FAILED — see details above and artifacts under", outputDir, "===")
	}
	return nil
}

// buildMCPFilesBinary compiles cmd/aries-claudecode-mcpfiles for linux/amd64
// (matching this host and the pinned aries-claudecode:test-1 image) into
// outputDir, so this probe stays self-contained instead of requiring the
// binary to be pre-built and passed in separately. Must be run with the repo
// root as the working directory (same assumption the rest of this probe and
// cmd/aries-claudecode-probe already make about `go build`/`go run`
// invocation).
func buildMCPFilesBinary(outputDir string) (string, error) {
	binaryPath := outputDir + "/aries-claudecode-mcpfiles"
	cmd := exec.Command("go", "build", "-o", binaryPath, "./cmd/aries-claudecode-mcpfiles")
	cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=0")
	if output, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("go build: %w (output: %s)", err, output)
	}
	return binaryPath, nil
}
