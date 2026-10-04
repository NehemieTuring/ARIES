package sandlock

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

const minKernelMajor, minKernelMinor = 6, 12

// ErrUnsupportedKernel means the host cannot provide Sandlock's required
// Landlock protections. ARIES does not fall back to Docker.
var ErrUnsupportedKernel = errors.New("sandlock requires Linux kernel 6.12 or newer")

// ErrUnavailable means the Sandlock runtime could not be initialized.
var ErrUnavailable = errors.New("sandlock unavailable")

func validateHostKernel() error {
	raw, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil {
		return fmt.Errorf("%w: read kernel release: %v", ErrUnsupportedKernel, err)
	}
	major, minor, err := kernelVersion(string(raw))
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnsupportedKernel, err)
	}
	if major < minKernelMajor || (major == minKernelMajor && minor < minKernelMinor) {
		return fmt.Errorf("%w: host kernel %d.%d", ErrUnsupportedKernel, major, minor)
	}
	return nil
}

func kernelVersion(release string) (int, int, error) {
	release = strings.TrimSpace(release)
	if release == "" {
		return 0, 0, errors.New("empty kernel release")
	}
	version, _, _ := strings.Cut(release, "-")
	parts := strings.Split(version, ".")
	if len(parts) < 2 {
		return 0, 0, fmt.Errorf("kernel release %q has no minor version", release)
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, fmt.Errorf("kernel release %q: %w", release, err)
	}
	minor, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, fmt.Errorf("kernel release %q: %w", release, err)
	}
	return major, minor, nil
}
