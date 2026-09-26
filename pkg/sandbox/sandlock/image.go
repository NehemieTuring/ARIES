package sandlock

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/hyscale-lab/aries/pkg/containerimage"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

// seedFromImage copies the image root filesystem into the private directory,
// then removes the helper container. The helper is not started and never runs
// Sandlock. The dynamic linker must live inside that tree: Sandlock resolves
// it from the chroot root, not from a host mount.
func seedFromImage(ctx context.Context, image, root, workdir string) error {
	if err := validateImage(image); err != nil {
		return err
	}
	api, err := client.New(client.WithHost("unix:///var/run/docker.sock"), client.WithUserAgent("aries-sandlock-seed/1"))
	if err != nil {
		return fmt.Errorf("sandlock workspace seed: %w", err)
	}
	defer api.Close()
	created, err := api.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config: &container.Config{Image: image, Cmd: []string{"/bin/true"}},
	})
	if err != nil {
		return fmt.Errorf("sandlock workspace seed: create helper container: %w", err)
	}
	if created.ID == "" {
		return errors.New("sandlock workspace seed: Docker returned an empty container ID")
	}
	defer func() {
		_, _ = api.ContainerRemove(context.WithoutCancel(ctx), created.ID, client.ContainerRemoveOptions{Force: true})
	}()
	copied, err := api.CopyFromContainer(ctx, created.ID, client.CopyFromContainerOptions{SourcePath: "/"})
	if err != nil {
		return fmt.Errorf("sandlock workspace seed: copy %s from %s: %w", workdir, image, err)
	}
	defer copied.Content.Close()
	if err := extractArchive(root, copied.Content); err != nil {
		return fmt.Errorf("sandlock workspace seed: %w", err)
	}
	hostWork, err := hostPath(root, workdir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(hostWork, 0o755); err != nil {
		return fmt.Errorf("sandlock workspace seed: create workdir: %w", err)
	}
	return nil
}

// installHostExecutable copies one host binary and its dynamic libraries into
// the private root as regular files. Tests use it when no task image is
// extracted. Absolute symlinks are copied as bytes so the chroot linker can
// open them.
func installHostExecutable(root, executable string) error {
	if _, err := cleanTaskPath(executable); err != nil {
		return err
	}
	if err := copyRegular(root, executable); err != nil {
		return err
	}
	output, err := exec.Command("ldd", executable).Output()
	if err != nil {
		return nil
	}
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		for _, field := range fields {
			if strings.HasPrefix(field, "/") {
				if err := copyRegular(root, field); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func copyHostTree(root, source string) error {
	target, err := hostPath(root, source)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	output, err := exec.Command("cp", "-a", source, target).CombinedOutput()
	if err != nil {
		return fmt.Errorf("copy %s into sandlock root: %w (%s)", source, err, output)
	}
	return nil
}

func copyRegular(root, source string) error {
	resolved, err := filepath.EvalSymlinks(source)
	if err != nil {
		return err
	}
	input, err := os.Open(resolved)
	if err != nil {
		return err
	}
	defer input.Close()
	info, err := input.Stat()
	if err != nil {
		return err
	}
	target, err := hostPath(root, source)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	output, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(output, input); err != nil {
		output.Close()
		return err
	}
	return output.Close()
}

func extractArchive(root string, content io.Reader) error {
	reader := tar.NewReader(content)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read image archive: %w", err)
		}
		name := strings.TrimPrefix(filepath.Clean("/"+header.Name), "/")
		if name == "" || name == "." {
			continue
		}
		target := filepath.Join(root, name)
		relative, err := filepath.Rel(root, target)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return fmt.Errorf("image archive path %q escapes the workspace", header.Name)
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			file, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(header.Mode)&0o777)
			if err != nil {
				return err
			}
			if _, err := io.Copy(file, reader); err != nil {
				file.Close()
				return err
			}
			if err := file.Close(); err != nil {
				return err
			}
		default:
			continue
		}
	}
}

func validateImage(image string) error {
	if err := containerimage.Validate(image); err == nil {
		return nil
	}
	tagged, err := containerimage.ValidateTagOnly(image)
	if err != nil {
		return fmt.Errorf("invalid sandlock task image: %w", err)
	}
	if tagged != image {
		return errors.New("invalid sandlock task image: image must not contain surrounding whitespace")
	}
	return nil
}

func validateEnvironment(environment core.Environment) error {
	if err := validateImage(environment.Image); err != nil {
		return err
	}
	if _, err := cleanTaskPath(environment.Workdir); err != nil {
		return fmt.Errorf("invalid sandlock workdir: %w", err)
	}
	if environment.CPU < 0 || environment.MemoryMB < 0 || environment.StorageMB < 0 || environment.GPUs < 0 {
		return errors.New("sandlock CPU, memory, storage, and GPU counts must be nonnegative")
	}
	for key, value := range environment.Env {
		if !validEnvName(key) || strings.ContainsRune(value, 0) {
			return fmt.Errorf("invalid sandlock environment %q", key)
		}
	}
	return nil
}

func validateIdentity(kind, value string) error {
	limit := 128
	if kind == "task" {
		limit = 149
	}
	if value == "" || len(value) > limit {
		return fmt.Errorf("sandlock sandbox %s ID must contain 1 to %d characters", kind, limit)
	}
	for index, character := range value {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || index > 0 && (character == '-' || character == '_' || character == '.') {
			continue
		}
		return fmt.Errorf("sandlock sandbox %s ID %q contains an unsafe character", kind, value)
	}
	return nil
}

func validEnvName(value string) bool {
	for index, r := range value {
		if r == '_' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || index > 0 && r >= '0' && r <= '9' {
			continue
		}
		return false
	}
	return value != ""
}
