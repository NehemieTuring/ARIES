package sandlock

import (
	_ "embed"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

//go:embed realpath_compat.src
var realpathCompatSource string

const realpathCompatPath = "/usr/lib/libaries-realpath.so"

var (
	realpathCompatOnce  sync.Once
	realpathCompatBytes []byte
	realpathCompatErr   error
)

func realpathCompatLibrary() ([]byte, error) {
	realpathCompatOnce.Do(func() {
		dir, err := os.MkdirTemp("", "aries-realpath-")
		if err != nil {
			realpathCompatErr = err
			return
		}
		defer os.RemoveAll(dir)
		source := filepath.Join(dir, "realpath_compat.c")
		if err := os.WriteFile(source, []byte(realpathCompatSource), 0o644); err != nil {
			realpathCompatErr = err
			return
		}
		out := filepath.Join(dir, "libaries-realpath.so")
		cmd := exec.Command("gcc", "-shared", "-fPIC", "-O2", "-o", out, source, "-ldl")
		if output, err := cmd.CombinedOutput(); err != nil {
			realpathCompatErr = fmt.Errorf("build sandlock realpath helper: %w: %s", err, output)
			return
		}
		realpathCompatBytes, realpathCompatErr = os.ReadFile(out)
	})
	return realpathCompatBytes, realpathCompatErr
}

func installRealpathCompat(root string) error {
	library, err := realpathCompatLibrary()
	if err != nil {
		return err
	}
	dest, err := hostPath(root, realpathCompatPath)
	if err != nil {
		return err
	}
	parent := filepath.Dir(dest)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("create sandlock realpath helper directory: %w", err)
	}
	resolvedParent, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return fmt.Errorf("resolve sandlock realpath helper directory: %w", err)
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return fmt.Errorf("resolve sandlock root: %w", err)
	}
	if resolvedParent != resolvedRoot && !strings.HasPrefix(resolvedParent, resolvedRoot+string(os.PathSeparator)) {
		return fmt.Errorf("sandlock realpath helper %s escapes the private root", realpathCompatPath)
	}
	if err := os.WriteFile(dest, library, 0o755); err != nil {
		return fmt.Errorf("install sandlock realpath helper: %w", err)
	}
	return nil
}
