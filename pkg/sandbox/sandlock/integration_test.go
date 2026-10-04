//go:build integration

package sandlock

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/runner"
)

func TestToolCommands(t *testing.T) {
	manager, err := New(Options{
		OutputDir: t.TempDir(),
		SeedWorkspace: func(_ context.Context, _, root, _ string) error {
			for _, executable := range []string{"/bin/sh", "/bin/echo", "/bin/cat", "/usr/bin/grep", "/usr/bin/find", "/usr/bin/git", "/usr/bin/python3", "/usr/bin/node", "/usr/bin/make"} {
				if _, err := os.Stat(executable); err != nil {
					continue
				}
				if err := installHostExecutable(root, executable); err != nil {
					return err
				}
			}
			if info, err := os.Stat("/usr/lib/python3.12"); err == nil && info.IsDir() {
				if err := copyHostTree(root, "/usr/lib/python3.12"); err != nil {
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
	live, err := manager.Start(context.Background(), core.SandboxRequest{
		RunID: "run", TaskID: "tools",
		Environment: core.Environment{Image: "example.invalid/task:1", Workdir: "/app"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Stop(context.Background(), live) })
	commands := []core.Command{
		{Path: "/bin/sh", Args: []string{"-c", "echo hello"}},
		{Path: "/bin/sh", Args: []string{"-c", "printf hello > /app/note && cat /app/note"}},
		{Path: "/bin/sh", Args: []string{"-c", "printf hello > /app/note && grep hello /app/note"}},
		{Path: "/usr/bin/find", Args: []string{"/app"}},
	}
	if _, err := exec.LookPath("git"); err == nil {
		commands = append(commands, core.Command{Path: "/bin/sh", Args: []string{"-c", "git init /app/repo && git -C /app/repo status"}})
	}
	if _, err := exec.LookPath("python3"); err == nil {
		commands = append(commands, core.Command{Path: "/usr/bin/python3", Args: []string{"-c", "print('hello')"}})
	}
	// Node from a home-directory version manager is denied by the /home rule.
	// A system node under /usr is copied with the other executables above.
	if _, err := os.Stat("/usr/bin/node"); err == nil {
		commands = append(commands, core.Command{Path: "/usr/bin/node", Args: []string{"-e", "console.log('hello')"}})
	}
	if _, err := os.Stat("/usr/bin/make"); err == nil {
		commands = append(commands, core.Command{Path: "/bin/sh", Args: []string{"-c", "printf 'all:\\n\\t@echo hello\\n' > /app/Makefile && make -C /app"}})
	}
	for _, command := range commands {
		result, err := live.(runner.Sandbox).Exec(context.Background(), command)
		if err != nil || result.ExitCode != 0 {
			t.Fatalf("%s %v: %+v, %v", command.Path, command.Args, result, err)
		}
		if strings.Contains(command.Args[len(command.Args)-1], "hello") && !strings.Contains(result.Stdout, "hello") && command.Path != "/usr/bin/find" {
			t.Fatalf("%s produced %q", command.Path, result.Stdout)
		}
	}
}

func TestFixGitVerifierCanInstallUv(t *testing.T) {
	if _, err := os.Stat("/var/run/docker.sock"); err != nil {
		t.Skip(err)
	}
	manager, err := New(Options{OutputDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	live, err := manager.Start(context.Background(), core.SandboxRequest{
		RunID: "run", TaskID: "fix-git",
		Environment: core.Environment{
			Image: "alexgshaw/fix-git:20251031", Workdir: "/app/personal-site", AllowNetwork: true,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Stop(context.Background(), live) })
	result, err := live.(runner.Sandbox).Exec(context.Background(), core.Command{
		Path: "/bin/bash",
		Args: []string{"-c", `set -eu
command -v curl
test -x "$HOME/.local/bin/uv"
test -x "$HOME/.local/bin/uvx"
curl -LsSf https://astral.sh/uv/0.9.5/install.sh | sh || true
test -x "$HOME/.local/bin/uv"
test -x "$HOME/.local/bin/uvx"
. "$HOME/.local/bin/env"
command -v uvx
"$HOME/.local/bin/uv" python install 3.13
"$HOME/.local/bin/uv" run --python 3.13 python -c 'print("uv-python-ok")'
"$HOME/.local/bin/uvx" -p 3.13 pytest --version
`},
	})
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("uv install = %+v, %v", result, err)
	}
	t.Logf("stdout=%s stderr=%s", result.Stdout, result.Stderr)
}
