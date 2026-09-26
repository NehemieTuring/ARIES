package sandlock

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/monitor"
	"github.com/hyscale-lab/aries/pkg/runner"
)

func TestKernelVersionParsesRelease(t *testing.T) {
	major, minor, err := kernelVersion("7.0.0-31-generic")
	if err != nil || major != 7 || minor != 0 {
		t.Fatalf("version = %d.%d, %v", major, minor, err)
	}
	major, minor, err = kernelVersion("6.11.0")
	if err != nil || major != 6 || minor != 11 {
		t.Fatalf("version = %d.%d, %v", major, minor, err)
	}
}

func TestTaskPolicyDeniesNetworkUnlessAllowed(t *testing.T) {
	root := t.TempDir()
	denied, err := taskPolicy(root, "/app", core.Environment{Workdir: "/app"}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(denied.NetAllow) != 0 {
		t.Fatalf("denied network = %#v", denied.NetAllow)
	}
	if !contains(denied.Readable, "/usr") || !contains(denied.Readable, "/bin") || !contains(denied.Writable, "/app") {
		t.Fatalf("policy = %#v", denied)
	}
	if !contains(denied.Denied, "/etc/shadow") || !contains(denied.Denied, "/home") {
		t.Fatalf("denied paths = %#v", denied.Denied)
	}
	allowed, err := taskPolicy(root, "/app", core.Environment{Workdir: "/app", AllowNetwork: true}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(allowed.NetAllow) != 1 || allowed.NetAllow[0] != "*" {
		t.Fatalf("allowed network = %#v", allowed.NetAllow)
	}
	listed, err := taskPolicy(root, "/app", core.Environment{Workdir: "/app", AllowNetwork: true}, Options{NetAllow: []string{"github.com:443"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed.NetAllow) != 1 || listed.NetAllow[0] != "github.com:443" {
		t.Fatalf("allowlist = %#v", listed.NetAllow)
	}
}

func TestTaskPolicyMapsResourcesWithoutClaimingCgroupParity(t *testing.T) {
	policy, err := taskPolicy(t.TempDir(), "/app", core.Environment{
		Workdir: "/app", MemoryMB: 256, StorageMB: 64, CPU: 0.5, GPUs: 1,
	}, Options{MaxProcesses: 8, MaxOpenFiles: 64})
	if err != nil {
		t.Fatal(err)
	}
	if policy.MaxMemory != "256M" || policy.MaxDisk != "64M" || policy.MaxCPU != 50 || policy.MaxProcesses != 8 || policy.MaxOpenFiles != 64 {
		t.Fatalf("limits = %+v", policy)
	}
	if len(policy.GPUDevices) != 1 || policy.GPUDevices[0] != 0 {
		t.Fatalf("gpus = %#v", policy.GPUDevices)
	}
	wide, err := taskPolicy(t.TempDir(), "/app", core.Environment{Workdir: "/app", CPU: 4}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if wide.MaxCPU != 0 || wide.NumCPUs != 4 {
		t.Fatalf("cpu mapping = max %d num %d", wide.MaxCPU, wide.NumCPUs)
	}
	if _, ok := policy.Env["DEBIAN_FRONTEND"]; !ok || policy.Env["HOME"] != "/app" {
		t.Fatalf("env = %#v", policy.Env)
	}
}

func TestManagerRejectsInvalidBackendInputs(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Fatal("missing output directory")
	}
	manager := testManager(t)
	_, err := manager.Start(context.Background(), core.SandboxRequest{RunID: "run", TaskID: "task", Environment: core.Environment{Image: "alpine:3", Workdir: "relative"}})
	if err == nil {
		t.Fatal("relative workdir")
	}
}

func TestSandlockExecStdoutStderrAndExit(t *testing.T) {
	sandbox := startSandbox(t, core.Environment{Image: "example.invalid/task:1", Workdir: "/app"})
	result, err := sandbox.Exec(context.Background(), core.Command{Path: "/bin/sh", Args: []string{"-c", "echo hello-out; echo hello-err >&2; exit 3"}})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 3 || !strings.Contains(result.Stdout, "hello-out") || !strings.Contains(result.Stderr, "hello-err") {
		t.Fatalf("result = %+v", result)
	}
	log, err := os.ReadFile(filepath.Join(sandbox.artifactDir, "exec.jsonl"))
	if err != nil || !strings.Contains(string(log), `"backend":"sandlock"`) || !strings.Contains(string(log), `/bin/sh`) {
		t.Fatalf("exec log %q, %v", log, err)
	}
}

func TestSandlockTimeoutAndCancel(t *testing.T) {
	sandbox := startSandbox(t, core.Environment{Image: "example.invalid/task:1", Workdir: "/app"})
	_, err := sandbox.Exec(context.Background(), core.Command{Path: "/bin/sh", Args: []string{"-c", "while :; do :; done"}, Timeout: 200 * time.Millisecond})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, execErr := sandbox.Exec(ctx, core.Command{Path: "/bin/sh", Args: []string{"-c", "while :; do :; done"}})
		done <- execErr
	}()
	time.Sleep(100 * time.Millisecond)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error = %v", err)
	}
}

func TestSandlockFilesystemPolicy(t *testing.T) {
	sandbox := startSandbox(t, core.Environment{Image: "example.invalid/task:1", Workdir: "/app"})
	secret := filepath.Join(sandbox.root, "secret")
	if err := os.WriteFile(secret, []byte("hidden"), 0o600); err != nil {
		t.Fatal(err)
	}
	sandbox.policy.Denied = append(sandbox.policy.Denied, "/secret")
	denied, err := sandbox.Exec(context.Background(), core.Command{Path: "/bin/cat", Args: []string{"/secret"}})
	if err != nil {
		t.Fatal(err)
	}
	if denied.ExitCode == 0 || strings.Contains(denied.Stdout, "hidden") {
		t.Fatalf("denied read = %+v", denied)
	}
	written, err := sandbox.Exec(context.Background(), core.Command{Path: "/bin/sh", Args: []string{"-c", "printf hello > /app/note"}})
	if err != nil || written.ExitCode != 0 {
		t.Fatalf("write = %+v, %v", written, err)
	}
	host, err := os.ReadFile(filepath.Join(sandbox.root, "app", "note"))
	if err != nil || string(host) != "hello" {
		t.Fatalf("workspace file = %q, %v", host, err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("host"), 0o600); err != nil {
		t.Fatal(err)
	}
	isolated, err := sandbox.Exec(context.Background(), core.Command{Path: "/bin/cat", Args: []string{outside}})
	if err != nil {
		t.Fatal(err)
	}
	if isolated.ExitCode == 0 || strings.Contains(isolated.Stdout, "host") {
		t.Fatalf("host file leaked = %+v", isolated)
	}
}

func TestSandlockNetworkAllowAndDeny(t *testing.T) {
	sandbox := startSandbox(t, core.Environment{Image: "example.invalid/task:1", Workdir: "/app"})
	denied, err := sandbox.Exec(context.Background(), core.Command{Path: "/usr/bin/python3", Args: []string{"-c", "import socket; socket.create_connection(('127.0.0.1', 9), 1)"}})
	if err != nil {
		t.Fatal(err)
	}
	if denied.ExitCode == 0 {
		t.Fatalf("network deny = %+v", denied)
	}
	sandbox.policy.NetAllow = []string{"127.0.0.1:9"}
	allowed, err := sandbox.Exec(context.Background(), core.Command{Path: "/bin/sh", Args: []string{"-c", "echo allowed"}})
	if err != nil || allowed.ExitCode != 0 {
		t.Fatalf("allowlist still runs commands: %+v %v", allowed, err)
	}
}

func TestSandlockUploadDownloadAndCleanup(t *testing.T) {
	manager := testManager(t)
	live := startWithManager(t, manager, core.Environment{Image: "example.invalid/task:1", Workdir: "/app"})
	sandbox := live.(*Sandbox)
	source := filepath.Join(t.TempDir(), "input")
	if err := os.WriteFile(source, []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := sandbox.Upload(context.Background(), source, "/app/input"); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(manager.outputDir, "copied")
	if err := sandbox.Download(context.Background(), "/app/input", destination); err != nil {
		t.Fatal(err)
	}
	copied, err := os.ReadFile(destination)
	if err != nil || string(copied) != "payload" {
		t.Fatalf("copied = %q, %v", copied, err)
	}
	if err := manager.Stop(context.Background(), live); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sandbox.root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("workspace remains: %v", err)
	}
	if err := manager.Stop(context.Background(), live); err != nil {
		t.Fatal(err)
	}
}

func TestResourceSourceIdentifiesSandlock(t *testing.T) {
	source := NewResourceSource("task", "aries-sandlock-1", 128)
	readings, err := source.Sample(context.Background())
	if err != nil || len(readings) != 1 || readings[0].RuntimeName != "sandlock" || readings[0].MemoryLimitBytes != 128<<20 {
		t.Fatalf("readings = %#v, %v", readings, err)
	}
	recorder, err := monitor.New(monitor.Options{
		RunID: "run", TaskIDs: []string{"task"}, OutputDir: t.TempDir(), Source: source, Interval: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := recorder.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	report, err := recorder.Stop(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report["task"].SampleCount < 1 {
		t.Fatalf("recorder report = %#v", report["task"])
	}
}

func TestExecStreamSeparatesOutput(t *testing.T) {
	sandbox := startSandbox(t, core.Environment{Image: "example.invalid/task:1", Workdir: "/app"})
	var stdout, stderr bytes.Buffer
	result, err := sandbox.ExecStream(context.Background(), core.Command{Path: "/bin/sh", Args: []string{"-c", "IFS= read -r line; printf '%s\n' \"$line\"; echo err >&2"}}, strings.NewReader("in\n"), &stdout, &stderr)
	if err != nil || result.ExitCode != 0 || stdout.String() != "in\n" || !strings.Contains(stderr.String(), "err") {
		t.Fatalf("stream = %+v stdout %q stderr %q err %v", result, stdout.String(), stderr.String(), err)
	}
}

func testManager(t *testing.T) *Manager {
	t.Helper()
	manager, err := New(Options{
		OutputDir: t.TempDir(),
		SeedWorkspace: func(_ context.Context, _, root, _ string) error {
			for _, executable := range []string{"/bin/sh", "/usr/bin/python3", "/usr/bin/grep", "/usr/bin/find"} {
				if _, err := os.Stat(executable); err != nil {
					continue
				}
				if err := installHostExecutable(root, executable); err != nil {
					return err
				}
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	return manager
}

func startSandbox(t *testing.T, environment core.Environment) *Sandbox {
	t.Helper()
	live := startWithManager(t, testManager(t), environment)
	t.Cleanup(func() {
		_ = live.(*Sandbox).owner.Stop(context.Background(), live)
	})
	return live.(*Sandbox)
}

func startWithManager(t *testing.T, manager *Manager, environment core.Environment) runner.Sandbox {
	t.Helper()
	live, err := manager.Start(context.Background(), core.SandboxRequest{RunID: "run", TaskID: "task", Environment: environment})
	if err != nil {
		t.Fatal(err)
	}
	return live
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
