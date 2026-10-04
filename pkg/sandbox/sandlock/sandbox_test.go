package sandlock

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
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
	if !contains(denied.Readable, "/usr") || !contains(denied.Readable, "/bin") || !contains(denied.Readable, "/") || !contains(denied.Writable, "/app") {
		t.Fatalf("policy = %#v", denied)
	}
	if !contains(denied.Writable, "/usr") || !contains(denied.Writable, "/var") || !contains(denied.Writable, "/etc") {
		t.Fatalf("package paths = %#v", denied.Writable)
	}
	built := denied.sandbox()
	if built.UID != nil || built.GID != nil {
		t.Fatal("task commands stay the invoking user; this host cannot map uid 0")
	}
	if denied.Mounts["/dev"] != "/dev" || denied.Mounts["/proc"] != "" {
		t.Fatalf("host mounts = %#v", denied.Mounts)
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
	if policy.Env["UV_CACHE_DIR"] != "/tmp/uv-cache" || policy.Env["UV_PYTHON_INSTALL_DIR"] != "/tmp/uv-python" || policy.Env["UV_NO_CONFIG"] != "1" || policy.Env["PIP_USER"] != "1" || policy.Env["LD_PRELOAD"] != realpathCompatPath {
		t.Fatalf("cache env = %#v", policy.Env)
	}
	if !contains(policy.Writable, "/tmp/uv-cache") || !contains(policy.Writable, "/logs/verifier") {
		t.Fatalf("cache paths = %#v", policy.Writable)
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

func TestSandlockCanWritePackageDirectories(t *testing.T) {
	sandbox := startSandbox(t, core.Environment{Image: "example.invalid/task:1", Workdir: "/app"})
	for _, dir := range []string{"var/lib/apt/lists/partial", "usr/bin", "etc"} {
		if err := os.MkdirAll(filepath.Join(sandbox.root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(sandbox.root, "etc", "shadow"), []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := sandbox.Exec(context.Background(), core.Command{
		Path: "/bin/sh",
		Args: []string{"-c", "printf ok > /var/lib/apt/lists/partial/probe"},
	})
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("package write = %+v, %v", result, err)
	}
	probe, err := os.ReadFile(filepath.Join(sandbox.root, "var/lib/apt/lists/partial/probe"))
	if err != nil || string(probe) != "ok" {
		t.Fatalf("probe = %q, %v", probe, err)
	}
	shadow, err := sandbox.Exec(context.Background(), core.Command{Path: "/bin/sh", Args: []string{"-c", "read line < /etc/shadow"}})
	if err != nil {
		t.Fatal(err)
	}
	if shadow.ExitCode == 0 || strings.Contains(shadow.Stdout, "secret") {
		t.Fatalf("shadow readable = %+v", shadow)
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

func TestSandlockReadsReferenceOutsideWorkdir(t *testing.T) {
	sandbox := startSandbox(t, core.Environment{Image: "example.invalid/task:1", Workdir: "/app/personal-site"})
	if err := installHostExecutable(sandbox.root, "/bin/cat"); err != nil {
		t.Fatal(err)
	}
	host, err := hostPath(sandbox.root, "/app/resources/patch_files/about.md")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(host), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(host, []byte("reference\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	result, err := sandbox.Exec(context.Background(), core.Command{Path: "/bin/cat", Args: []string{"/app/resources/patch_files/about.md"}})
	if err != nil || result.ExitCode != 0 || result.Stdout != "reference\n" {
		t.Fatalf("reference read = %+v, %v", result, err)
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

func TestRealpathSeesNewDirectory(t *testing.T) {
	sandbox := startSandbox(t, core.Environment{Image: "example.invalid/task:1", Workdir: "/app"})
	for _, executable := range []string{"/bin/mkdir", "/usr/bin/realpath", "/usr/bin/stat", "/usr/bin/readlink"} {
		if err := installHostExecutable(sandbox.root, executable); err != nil {
			t.Fatal(err)
		}
	}
	result, err := sandbox.Exec(context.Background(), core.Command{
		Path: "/bin/sh",
		Args: []string{"-c", `set +e
mkdir -p /app/.tmpprobe
echo '--- readlink /app ---'
readlink /app
echo READLINK:$?
echo '--- stat /app ---'
stat /app >/dev/null
echo STAT:$?
echo '--- realpath /app ---'
realpath /app
echo REALPATH:$?
echo '--- realpath probe ---'
realpath /app/.tmpprobe
echo PROBE:$?
`},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("stdout=%s stderr=%s", result.Stdout, result.Stderr)
	if result.ExitCode != 0 || !strings.Contains(result.Stdout, "REALPATH:0") || !strings.Contains(result.Stdout, "PROBE:0") {
		t.Fatalf("realpath = %+v", result)
	}
}

func TestSandlockCanCreateNestedDirectories(t *testing.T) {
	sandbox := startSandbox(t, core.Environment{Image: "example.invalid/task:1", Workdir: "/app"})
	if err := installHostExecutable(sandbox.root, "/bin/mkdir"); err != nil {
		t.Fatal(err)
	}
	result, err := sandbox.Exec(context.Background(), core.Command{
		Path: "/bin/sh",
		Args: []string{"-c", "mkdir -p /app/.cache/uv \"$UV_CACHE_DIR/child\" \"$UV_PYTHON_INSTALL_DIR/child\" && printf ok > \"$UV_PYTHON_INSTALL_DIR/child/probe\""},
	})
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("nested create = %+v, %v", result, err)
	}
}

func TestMkdirMissingParentIsNotFound(t *testing.T) {
	sandbox := startSandbox(t, core.Environment{Image: "example.invalid/task:1", Workdir: "/app"})
	if err := installHostExecutable(sandbox.root, "/bin/mkdir"); err != nil {
		t.Fatal(err)
	}
	result, err := sandbox.Exec(context.Background(), core.Command{
		Path: "/bin/mkdir",
		Args: []string{"/tmp/uv-cache/missing-parent/child"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode == 0 || !strings.Contains(result.Stderr, "No such file or directory") {
		t.Fatalf("missing parent = %+v", result)
	}
}

func TestSandlockCanExecScript(t *testing.T) {
	sandbox := startSandbox(t, core.Environment{Image: "example.invalid/task:1", Workdir: "/app"})
	host, err := hostPath(sandbox.root, "/tmp/hello.sh")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(host, []byte("#!/bin/sh\necho script-ok\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	result, err := sandbox.Exec(context.Background(), core.Command{
		Path: "/bin/sh",
		Args: []string{"-c", "exec /tmp/hello.sh"},
	})
	if err != nil || result.ExitCode != 0 || !strings.Contains(result.Stdout, "script-ok") {
		t.Fatalf("script = %+v, %v", result, err)
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

func TestExecStreamReturnsWhileStdinStaysOpen(t *testing.T) {
	sandbox := startSandbox(t, core.Environment{Image: "example.invalid/task:1", Workdir: "/app"})
	reader, writer := io.Pipe()
	t.Cleanup(func() { _ = writer.Close() })
	var stdout bytes.Buffer
	done := make(chan error, 1)
	go func() {
		result, err := sandbox.ExecStream(context.Background(), core.Command{
			Path: "/bin/sh", Args: []string{"-c", "printf 'done\\n'"},
		}, reader, &stdout, io.Discard)
		if err != nil || result.ExitCode != 0 || stdout.String() != "done\n" {
			done <- fmt.Errorf("stream = %+v stdout %q err %v", result, stdout.String(), err)
			return
		}
		done <- nil
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("exec stayed open after the process exited because stdin was still open")
	}
	next, err := sandbox.Exec(context.Background(), core.Command{Path: "/bin/sh", Args: []string{"-c", "printf next"}})
	if err != nil || next.ExitCode != 0 || next.Stdout != "next" {
		t.Fatalf("following command = %+v %v", next, err)
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
	if errors.Is(err, ErrUnsupportedKernel) {
		t.Skipf("sandlock unavailable on this host: %v", err)
	}
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
