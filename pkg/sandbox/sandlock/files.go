package sandlock

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Upload copies one host file into the task chroot. The destination is a
// logical task path such as /tests/test.sh.
func (s *Sandbox) Upload(_ context.Context, source, destination string) error {
	destination, err := cleanTaskPath(destination)
	if err != nil {
		return fmt.Errorf("invalid sandlock upload destination: %w", err)
	}
	info, err := os.Lstat(source)
	if err != nil {
		return fmt.Errorf("stat sandlock upload source: %w", err)
	}
	if !info.Mode().IsRegular() {
		return errors.New("sandlock upload source must be a regular file")
	}
	target, err := hostPath(s.root, destination)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fmt.Errorf("create sandlock upload directory: %w", err)
	}
	input, err := os.Open(source)
	if err != nil {
		return fmt.Errorf("open sandlock upload source: %w", err)
	}
	defer input.Close()
	output, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, info.Mode().Perm())
	if err != nil {
		return fmt.Errorf("create sandlock upload destination: %w", err)
	}
	if _, err := io.Copy(output, input); err != nil {
		output.Close()
		return fmt.Errorf("write sandlock upload: %w", err)
	}
	return output.Close()
}

// Download copies one regular file out of the task chroot into the run output
// directory.
func (s *Sandbox) Download(_ context.Context, source, destination string) error {
	source, err := cleanTaskPath(source)
	if err != nil {
		return fmt.Errorf("invalid sandlock download source: %w", err)
	}
	destination, err = outputPath(s.outputDir, destination)
	if err != nil {
		return err
	}
	host, err := hostPath(s.root, source)
	if err != nil {
		return err
	}
	info, err := os.Lstat(host)
	if err != nil {
		return fmt.Errorf("stat sandlock download source: %w", err)
	}
	if !info.Mode().IsRegular() {
		return errors.New("sandlock download source must be a regular file")
	}
	input, err := os.Open(host)
	if err != nil {
		return fmt.Errorf("open sandlock download source: %w", err)
	}
	defer input.Close()
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return fmt.Errorf("create sandlock download directory: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(destination), ".aries-download-*")
	if err != nil {
		return fmt.Errorf("create sandlock download destination: %w", err)
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if _, err := io.Copy(temporary, input); err != nil {
		temporary.Close()
		return fmt.Errorf("write sandlock download: %w", err)
	}
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return fmt.Errorf("secure sandlock download: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close sandlock download: %w", err)
	}
	if err := os.Rename(temporaryName, destination); err != nil {
		return fmt.Errorf("publish sandlock download: %w", err)
	}
	return nil
}

func cleanTaskPath(path string) (string, error) {
	if path == "" || strings.ContainsRune(path, 0) || !strings.HasPrefix(path, "/") {
		return "", errors.New("path must be absolute, nonempty, and NUL-free")
	}
	clean := filepath.Clean(path)
	if clean != path || clean == "/" {
		return "", errors.New("path must be a clean path below the task root")
	}
	return clean, nil
}

func outputPath(root, destination string) (string, error) {
	absolute, err := filepath.Abs(destination)
	if err != nil {
		return "", fmt.Errorf("resolve sandlock download destination: %w", err)
	}
	relative, err := filepath.Rel(root, absolute)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", errors.New("sandlock download destination is outside the configured output directory")
	}
	return absolute, nil
}
