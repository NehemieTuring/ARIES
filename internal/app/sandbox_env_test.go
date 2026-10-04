package app

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hyscale-lab/aries/pkg/config"
)

func TestEnvironmentSandboxTypeOverridesProfile(t *testing.T) {
	t.Setenv(sandboxTypeEnv, "sandlock")
	got, err := sandboxTypeSeenByValidator(t, "")
	if err != nil {
		t.Fatal(err)
	}
	if got != "sandlock" {
		t.Fatalf("sandbox type = %q", got)
	}
}

func TestEmptySandboxTypeEnvironmentIsRejected(t *testing.T) {
	t.Setenv(sandboxTypeEnv, "  ")
	_, err := sandboxTypeSeenByValidator(t, "")
	if err == nil || !strings.Contains(err.Error(), sandboxTypeEnv+" is empty") {
		t.Fatalf("err=%v", err)
	}
}

func TestDotEnvSandboxTypeOverridesProfileWhenEnvironmentIsUnset(t *testing.T) {
	unsetSandboxEnv(t)
	root, exe := createTestAriesRepository(t)
	if err := os.WriteFile(filepath.Join(root, ".env"), []byte("# export ARIES_SANDBOX_TYPE=\"docker\"\nexport OTHER=ignored\nexport ARIES_SANDBOX_TYPE=\"sandlock\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := sandboxTypeSeenByValidator(t, exe)
	if err != nil {
		t.Fatal(err)
	}
	if got != "sandlock" {
		t.Fatalf("sandbox type = %q", got)
	}
}

func TestProcessEnvironmentWinsOverDotEnv(t *testing.T) {
	t.Setenv(sandboxTypeEnv, "docker")
	root, exe := createTestAriesRepository(t)
	if err := os.WriteFile(filepath.Join(root, ".env"), []byte("export ARIES_SANDBOX_TYPE=sandlock\n"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := sandboxTypeSeenByValidator(t, exe)
	if err != nil {
		t.Fatal(err)
	}
	if got != "docker" {
		t.Fatalf("sandbox type = %q", got)
	}
}

func TestDotEnvAPIKeyIsUsedOnlyWhenTheProcessEnvironmentIsUnset(t *testing.T) {
	const name = "GEMINI_API_KEY"
	unsetEnv(t, name)
	root, exe := createTestAriesRepository(t)
	if err := os.WriteFile(filepath.Join(root, ".env"), []byte("export GEMINI_API_KEY=\"dotenv-canary\"\nexport ARIES_SANDBOX_TYPE=\"docker\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	value, found, err := dotenvValue(exe, name)
	if err != nil || !found || value != "dotenv-canary" {
		t.Fatalf("value=%q found=%t err=%v", value, found, err)
	}
	t.Setenv(name, "process-canary")
	got, ok := environmentAPIKeyLookup(name)
	if !ok || string(got) != "process-canary" {
		t.Fatalf("lookup = %q ok=%t", got, ok)
	}
}

func TestCommentedDotEnvSandboxTypeKeepsProfile(t *testing.T) {
	unsetSandboxEnv(t)
	root, exe := createTestAriesRepository(t)
	if err := os.WriteFile(filepath.Join(root, ".env"), []byte("# export ARIES_SANDBOX_TYPE=\"sandlock\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := sandboxTypeSeenByValidator(t, exe)
	if err != nil {
		t.Fatal(err)
	}
	if got != "docker" {
		t.Fatalf("sandbox type = %q", got)
	}
}

func sandboxTypeSeenByValidator(t *testing.T, executablePath string) (string, error) {
	t.Helper()
	profile := writeCommandProfile(t, filepath.Join(t.TempDir(), "runs"))
	var got string
	wiring := Wiring{
		ValidateComponents: func(cfg config.Config) error {
			got = cfg.Sandbox.Deployment.Backend
			return errors.New("stop after sandbox selection")
		},
		PrepareBackend: func(config.Config, string) (PreparedBackend, error) {
			t.Fatal("backend preparation ran")
			return PreparedBackend{}, nil
		},
	}
	err := Run(context.Background(), profile, io.Discard, Dependencies{ExecutablePath: executablePath, Wiring: wiring})
	if err == nil || !strings.Contains(err.Error(), "stop after sandbox selection") {
		return got, err
	}
	return got, nil
}

func unsetSandboxEnv(t *testing.T) {
	t.Helper()
	unsetEnv(t, sandboxTypeEnv)
}

func unsetEnv(t *testing.T, name string) {
	t.Helper()
	previous, existed := os.LookupEnv(name)
	if err := os.Unsetenv(name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if existed {
			_ = os.Setenv(name, previous)
			return
		}
		_ = os.Unsetenv(name)
	})
}
