// Command aries-claudecode-probe is a throwaway diagnostic tool, NOT part of
// the ARIES CLI surface. It validates the claudecodessh bridge and the
// bash-forwarding wrapper (pkg/harness/claudecode) against a real Docker
// sandbox, in isolation from a real Claude Code invocation. See
// rapport_integration_claude_code.md for how the wire shapes tested here were
// captured.
package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/hyscale-lab/aries/pkg/bridge/claudecodessh"
	"github.com/hyscale-lab/aries/pkg/core"
	dockersandbox "github.com/hyscale-lab/aries/pkg/sandbox/docker"
	"github.com/sirupsen/logrus"
	"golang.org/x/crypto/ssh"
)

// bashWrapperScript MUST match pkg/harness/claudecode's unexported constant of
// the same name — duplicated here only because this diagnostic lives in a
// separate package and the harness does not export it. Keep in sync manually.
const bashWrapperScript = `#!/bin/sh
set -e
if [ "$#" -lt 2 ]; then
  echo "aries claude-code bash wrapper: unsupported invocation: $*" >&2
  exit 1
fi
sep="$(printf '\037')"
payload="bash"
for arg in "$@"; do
  payload="$payload$sep$arg"
done
host="${ARIES_BRIDGE_ADDRESS%:*}"
port="${ARIES_BRIDGE_ADDRESS##*:}"
exec ssh -i "$ARIES_BRIDGE_IDENTITY" -p "$port" \
  -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o BatchMode=yes \
  "$ARIES_BRIDGE_USERNAME@$host" "$payload"
`

func main() {
	if err := run(); err != nil {
		log.Fatalf("probe failed: %v", err)
	}
}

func run() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	outputDir := "/tmp/aries-claudecode-probe-run"
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

	signer, err := loadSigner(endpoint.IdentitySourceFile)
	if err != nil {
		return err
	}

	// The wire format is argv elements joined by \x1f (see bashWrapperScript's
	// doc comment and pkg/bridge/claudecodessh's splitCommand) — plain-space
	// joining was tried and found ambiguous to re-split (§7.6).
	const sep = "\x1f"

	fmt.Println("=== 3a. grammar test: bootstrap env probe (Go SSH client, raw exec) ===")
	if err := runExec(endpoint, signer, "bash"+sep+"-c"+sep+"env"); err != nil {
		return fmt.Errorf("bootstrap env probe failed: %w", err)
	}
	fmt.Println("OK")

	fmt.Println("=== 3b. grammar test: per-call wrapper shape (Go SSH client, raw exec) ===")
	script := "source /tmp/nonexistent-snapshot.sh 2>/dev/null || true && shopt -u extglob 2>/dev/null || true && { \\builtin unalias -- 'unsetenv'; \\builtin unset -f -- 'unsetenv'; } >/dev/null 2>&1 || true && eval 'echo hello-grammar-test' < /dev/null && pwd -P >| /tmp/probe-cwd"
	wrapped := "bash" + sep + "-c" + sep + script
	out, err := runExecCapture(endpoint, signer, wrapped)
	if err != nil {
		return fmt.Errorf("wrapper-shape exec failed: %w", err)
	}
	if !strings.Contains(out, "hello-grammar-test") {
		return fmt.Errorf("wrapper-shape exec did not produce expected output, got: %q", out)
	}
	fmt.Println("OK — output:", strings.TrimSpace(out))

	fmt.Println("=== 4. real bash-wrapper script test (real ssh client, not the Go library) ===")
	wrapperPath, err := stageWrapperScript(outputDir)
	if err != nil {
		return err
	}
	// "env" matches the recognized bootstrap shape (see grammar.go); a plain
	// arbitrary command is correctly refused by design, so this call only
	// tests the wrapper's own mechanics (key loading, host:port parsing,
	// quoting) — not the grammar match itself, already covered by phase 3b.
	cmd := exec.Command(wrapperPath, "-c", "env")
	cmd.Env = append(os.Environ(),
		"ARIES_BRIDGE_ADDRESS="+endpoint.Address,
		"ARIES_BRIDGE_USERNAME="+endpoint.Username,
		"ARIES_BRIDGE_IDENTITY="+endpoint.IdentitySourceFile,
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()
	fmt.Println("--- wrapper stdout ---")
	fmt.Println(stdout.String())
	fmt.Println("--- wrapper stderr ---")
	fmt.Println(stderr.String())
	if runErr != nil {
		fmt.Println("=== RESULT: FAILED — wrapper script exited with error:", runErr, "===")
		return nil
	}
	if strings.Contains(stdout.String(), "PATH=") {
		fmt.Println("=== RESULT: OK — real bash wrapper + real ssh client correctly forwarded to the sandbox ===")
	} else {
		fmt.Println("=== RESULT: FAILED — expected output not observed ===")
	}

	// Phase 5: does edit_file's old_string/new_string (base64-encoded and
	// \x1f-joined into ONE SSH exec "command" string — see
	// pkg/bridge/claudecodessh/fileops.go and cmd/aries-claudecode-mcpfiles's
	// runFileOp) have a real size limit? Unlike write_file (content streamed
	// over the channel's stdin, piped straight to `dd` — never touches any
	// argv/exec() boundary), old_string/new_string ride inside the exec
	// request's command string itself. There is no local OS argv/exec() call
	// anywhere in this path either (the client dials with the native Go SSH
	// library, golang.org/x/crypto/ssh, not a subprocess) — so classic Unix
	// ARG_MAX literally cannot apply; the real question is whether the SSH
	// library/protocol imposes its own limit on a single exec request's
	// command string. Tested here directly against the bridge, bypassing
	// Claude Code and the real API entirely — free and fast to iterate.
	fmt.Println("=== 5. fileop size stress test: edit_file with increasingly large old_string/new_string ===")
	if err := fileOpSizeStressTest(endpoint, signer); err != nil {
		return fmt.Errorf("fileop size stress test: %w", err)
	}
	return nil
}

func fileOpCommand(op string, args ...string) string {
	parts := make([]string, 0, len(args)+2)
	parts = append(parts, "aries-fileop", op)
	for _, argument := range args {
		parts = append(parts, base64.StdEncoding.EncodeToString([]byte(argument)))
	}
	return strings.Join(parts, "\x1f")
}

// fileOpRun mirrors cmd/aries-claudecode-mcpfiles's runFileOp exactly (same
// command construction, same one-session-per-call model) but as a standalone
// Go SSH client here, so this phase doesn't need to build or invoke that
// binary.
func fileOpRun(endpoint core.ToolEndpoint, signer ssh.Signer, stdin *strings.Reader, op string, args ...string) (stdout, stderr string, exitCode int, err error) {
	client, err := dialClient(endpoint, signer)
	if err != nil {
		return "", "", -1, fmt.Errorf("dial: %w", err)
	}
	defer client.Close()
	session, err := client.NewSession()
	if err != nil {
		return "", "", -1, fmt.Errorf("new session: %w", err)
	}
	defer session.Close()
	var outBuf, errBuf bytes.Buffer
	session.Stdout, session.Stderr = &outBuf, &errBuf
	if stdin != nil {
		session.Stdin = stdin
	}
	runErr := session.Run(fileOpCommand(op, args...))
	if runErr == nil {
		return outBuf.String(), errBuf.String(), 0, nil
	}
	if exitErr, ok := runErr.(*ssh.ExitError); ok {
		return outBuf.String(), errBuf.String(), exitErr.ExitStatus(), nil
	}
	return outBuf.String(), errBuf.String(), -1, runErr
}

// Threshold pinned by binary search against a real bridge
// (rapport_integration_claude_code.md §9.10): edit_file succeeds with
// old_string/new_string at 98,000 bytes each (196,000 combined raw, ~261.3KiB
// once base64-encoded together in the exec command string) and fails at
// 99,000 bytes each (198,000 combined, ~264.0KiB) — consistent with
// golang.org/x/crypto/ssh's hard-coded `maxPacket = 256*1024` (262144 bytes,
// see ssh/cipher.go) once the "aries-fileop"/path/separator overhead is
// added. Not an OS ARG_MAX (this path never calls exec() with these bytes as
// argv — the client dials with the native Go SSH library, no subprocess) but
// an SSH single-packet ceiling, hit here because both strings ride inside
// one exec request's command string rather than being streamed like
// write_file's content is.
func fileOpSizeStressTest(endpoint core.ToolEndpoint, signer ssh.Signer) error {
	const path = "/root/stress-test.txt"
	sizes := []int{1_000, 10_000, 50_000, 90_000, 98_000}
	for _, size := range sizes {
		oldBlob := strings.Repeat("A", size)
		newBlob := strings.Repeat("B", size)

		// Seed the file via write_file (stdin-streamed, already known safe —
		// this step is just setup, not what's under test).
		_, seedStderr, seedExit, err := fileOpRun(endpoint, signer, strings.NewReader(oldBlob), "write_file", path)
		if err != nil {
			return fmt.Errorf("size %d bytes: seed write_file transport error: %w", size, err)
		}
		if seedExit != 0 {
			return fmt.Errorf("size %d bytes: seed write_file exited %d: %s", size, seedExit, seedStderr)
		}

		// The actual question: old_string/new_string embedded in the exec
		// command string itself.
		out, editErr, exitCode, err := fileOpRun(endpoint, signer, nil, "edit_file", path, oldBlob, newBlob)
		if err != nil {
			fmt.Printf("size %d bytes: FAILED at SSH transport level: %v\n", size, err)
			fmt.Println("=== RESULT: limit found — see size above ===")
			return nil
		}
		if exitCode != 0 {
			fmt.Printf("size %d bytes: edit_file rejected, exit=%d, stdout=%q stderr=%q\n", size, exitCode, out, editErr)
			fmt.Println("=== RESULT: limit found — see size above ===")
			return nil
		}
		fmt.Printf("size %d bytes: OK\n", size)
	}
	fmt.Printf("=== RESULT: OK — edit_file handled up to %d bytes without hitting any limit ===\n", sizes[len(sizes)-1])
	return nil
}

func loadSigner(identityPath string) (ssh.Signer, error) {
	pemBytes, err := os.ReadFile(identityPath)
	if err != nil {
		return nil, fmt.Errorf("read identity: %w", err)
	}
	signer, err := ssh.ParsePrivateKey(pemBytes)
	if err != nil {
		return nil, fmt.Errorf("parse identity: %w", err)
	}
	return signer, nil
}

func dialClient(endpoint core.ToolEndpoint, signer ssh.Signer) (*ssh.Client, error) {
	config := &ssh.ClientConfig{
		User: endpoint.Username, Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 5 * time.Second,
	}
	return ssh.Dial("tcp", endpoint.Address, config)
}

func runExec(endpoint core.ToolEndpoint, signer ssh.Signer, command string) error {
	_, err := runExecCapture(endpoint, signer, command)
	return err
}

func runExecCapture(endpoint core.ToolEndpoint, signer ssh.Signer, command string) (string, error) {
	client, err := dialClient(endpoint, signer)
	if err != nil {
		return "", fmt.Errorf("dial: %w", err)
	}
	defer client.Close()
	session, err := client.NewSession()
	if err != nil {
		return "", fmt.Errorf("new session: %w", err)
	}
	defer session.Close()
	var stdout, stderr bytes.Buffer
	session.Stdout, session.Stderr = &stdout, &stderr
	if err := session.Run(command); err != nil {
		if _, ok := err.(*ssh.ExitError); !ok {
			return "", fmt.Errorf("run %q: %w (stderr: %s)", command, err, stderr.String())
		}
	}
	return stdout.String(), nil
}

func stageWrapperScript(outputDir string) (string, error) {
	path := outputDir + "/bash-wrapper.sh"
	if err := os.WriteFile(path, []byte(bashWrapperScript), 0o755); err != nil {
		return "", fmt.Errorf("stage wrapper script: %w", err)
	}
	return path, nil
}
