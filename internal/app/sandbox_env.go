package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/hyscale-lab/aries/pkg/config"
)

const sandboxTypeEnv = "ARIES_SANDBOX_TYPE"

// selectSandboxType replaces the profile sandbox when ARIES_SANDBOX_TYPE is
// set. The process environment wins. Otherwise bin/aries reads that one
// variable from the repository-root .env. The profile value stays in place
// when neither source sets it.
func selectSandboxType(cfg config.Config, executablePath string) (config.Config, error) {
	if value, ok, err := sandboxTypeFromEnvironment(); err != nil || ok {
		if err != nil {
			return cfg, err
		}
		cfg.Sandbox.Type = value
		return cfg, nil
	}
	value, found, err := sandboxTypeFromDotEnv(executablePath)
	if err != nil {
		return cfg, err
	}
	if found {
		cfg.Sandbox.Type = value
	}
	return cfg, nil
}

func sandboxTypeFromEnvironment() (string, bool, error) {
	raw, ok := os.LookupEnv(sandboxTypeEnv)
	if !ok {
		return "", false, nil
	}
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", false, fmt.Errorf("%s is empty", sandboxTypeEnv)
	}
	return value, true, nil
}

func sandboxTypeFromDotEnv(executablePath string) (string, bool, error) {
	keyPath, anchored := repositoryAPIKeyPath(executablePath)
	if !anchored {
		return "", false, nil
	}
	content, err := os.ReadFile(filepath.Join(filepath.Dir(keyPath), ".env"))
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read .env: %w", err)
	}
	found := false
	value := ""
	for _, line := range strings.Split(string(content), "\n") {
		parsed, ok, err := sandboxTypeLine(line)
		if err != nil {
			return "", false, err
		}
		if !ok {
			continue
		}
		found = true
		value = parsed
	}
	return value, found, nil
}

func sandboxTypeLine(line string) (string, bool, error) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return "", false, nil
	}
	line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
	name, raw, ok := strings.Cut(line, "=")
	if !ok || strings.TrimSpace(name) != sandboxTypeEnv {
		return "", false, nil
	}
	value, err := unquoteEnvValue(raw)
	if err != nil {
		return "", false, fmt.Errorf("parse .env %s: %w", sandboxTypeEnv, err)
	}
	if value == "" {
		return "", false, fmt.Errorf(".env %s is empty", sandboxTypeEnv)
	}
	return value, true, nil
}

func unquoteEnvValue(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if len(value) >= 2 {
		quote := value[0]
		if (quote == '"' || quote == '\'') && value[len(value)-1] == quote {
			value = value[1 : len(value)-1]
		}
	}
	if strings.ContainsAny(value, "\r\n") {
		return "", errors.New("value contains a newline")
	}
	return strings.TrimSpace(value), nil
}
