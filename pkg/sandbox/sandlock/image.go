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
// then removes the helper container. The helper runs only long enough to
// install curl and uv when the image does not already have them, because
// Terminal-Bench verifiers download their test runner that way and this host
// cannot map the task uid to 0. The helper never runs Sandlock. The dynamic
// linker must live inside that tree: Sandlock resolves it from the chroot
// root, not from a host mount.
func seedFromImage(ctx context.Context, image, root, workdir string) error {
	if err := validateImage(image); err != nil {
		return err
	}
	api, err := client.New(client.WithHost("unix:///var/run/docker.sock"), client.WithUserAgent("aries-sandlock-seed/1"))
	if err != nil {
		return fmt.Errorf("sandlock workspace seed: %w", err)
	}
	defer api.Close()
	command, err := curlSeedCommand(workdir)
	if err != nil {
		return err
	}
	created, err := api.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config: &container.Config{
			Image:      image,
			User:       "0:0",
			Env:        []string{"DEBIAN_FRONTEND=noninteractive"},
			Entrypoint: []string{"/bin/sh", "-c"},
			Cmd:        []string{command},
		},
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
	wait := api.ContainerWait(ctx, created.ID, client.ContainerWaitOptions{Condition: container.WaitConditionNextExit})
	if _, err := api.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		return fmt.Errorf("sandlock workspace seed: start helper container: %w", err)
	}
	select {
	case err := <-wait.Error:
		if err != nil {
			return fmt.Errorf("sandlock workspace seed: wait helper container: %w", err)
		}
	case status := <-wait.Result:
		if status.Error != nil && status.Error.Message != "" {
			return fmt.Errorf("sandlock workspace seed: helper container: %s", status.Error.Message)
		}
		if status.StatusCode != 0 {
			return fmt.Errorf("sandlock workspace seed: helper container exited %d", status.StatusCode)
		}
	case <-ctx.Done():
		return fmt.Errorf("sandlock workspace seed: %w", ctx.Err())
	}
	copied, err := api.CopyFromContainer(ctx, created.ID, client.CopyFromContainerOptions{SourcePath: "/"})
	if err != nil {
		return fmt.Errorf("sandlock workspace seed: copy %s from %s: %w", workdir, image, err)
	}
	defer copied.Content.Close()
	if err := extractArchive(root, copied.Content); err != nil {
		return fmt.Errorf("sandlock workspace seed: %w", err)
	}
	if err := mkdirTaskPath(root, workdir, 0o755); err != nil {
		return fmt.Errorf("sandlock workspace seed: create workdir: %w", err)
	}
	return nil
}

const curlSeedScript = `if ! command -v curl >/dev/null 2>&1; then
  if ! command -v apt-get >/dev/null 2>&1; then exit 0; fi
  apt-get update
  apt-get install -y curl ca-certificates
fi
export HOME='%s'
if [ ! -x "$HOME/.local/bin/uv" ]; then
  curl -LsSf https://astral.sh/uv/0.9.5/install.sh | sh
fi
`

func curlSeedCommand(workdir string) (string, error) {
	if strings.ContainsAny(workdir, "'\n\r") || !strings.HasPrefix(workdir, "/") {
		return "", fmt.Errorf("sandlock workspace seed: workdir %q cannot be passed to the helper", workdir)
	}
	return fmt.Sprintf(curlSeedScript, workdir), nil
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
	fs, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer fs.Close()
	name, err := rootName(source)
	if err != nil {
		return err
	}
	return copyHostInto(fs, source, name)
}

func copyHostInto(fs *os.Root, source, name string) error {
	info, err := os.Lstat(source)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(source)
		if err != nil {
			return err
		}
		return fs.Symlink(target, name)
	}
	if info.IsDir() {
		if err := fs.MkdirAll(name, 0o755); err != nil {
			return err
		}
		entries, err := os.ReadDir(source)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if err := copyHostInto(fs, filepath.Join(source, entry.Name()), name+"/"+entry.Name()); err != nil {
				return err
			}
		}
		return nil
	}
	if !info.Mode().IsRegular() {
		return nil
	}
	return writeHostFile(fs, source, name, info.Mode().Perm())
}

func copyRegular(root, source string) error {
	resolved, err := filepath.EvalSymlinks(source)
	if err != nil {
		return err
	}
	info, err := os.Lstat(resolved)
	if err != nil {
		return err
	}
	name, err := rootName(source)
	if err != nil {
		return err
	}
	fs, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer fs.Close()
	return writeHostFile(fs, resolved, name, info.Mode().Perm())
}

func writeHostFile(fs *os.Root, source, name string, perm os.FileMode) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	parent := filepath.Dir(name)
	if parent != "." {
		if err := fs.MkdirAll(parent, 0o755); err != nil {
			return err
		}
	}
	output, err := fs.OpenFile(name, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, perm)
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
	fs, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer fs.Close()
	reader := tar.NewReader(content)
	var links []archiveLink
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read image archive: %w", err)
		}
		name, ok, err := archiveName(header.Name)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := fs.MkdirAll(name, 0o755); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := mkdirParent(fs, name); err != nil {
				return err
			}
			file, err := fs.OpenFile(name, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(header.Mode)&0o777)
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
		case tar.TypeSymlink:
			// Merged Debian images store /bin as a symlink to usr/bin. The
			// target is rewritten relative to the private root so a host walk
			// cannot leave that directory, while the chroot still resolves it.
			link, err := relativeSymlink(name, header.Linkname)
			if err != nil {
				return err
			}
			if err := mkdirParent(fs, name); err != nil {
				return err
			}
			if err := fs.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			if err := fs.Symlink(link, name); err != nil {
				return fmt.Errorf("image archive symlink %q: %w", header.Name, err)
			}
		case tar.TypeLink:
			links = append(links, archiveLink{target: name, sourceName: header.Linkname, name: header.Name})
		default:
			continue
		}
	}
	for _, link := range links {
		source, ok, err := archiveName(link.sourceName)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("image archive hard link %q has an empty source", link.name)
		}
		if err := mkdirParent(fs, link.target); err != nil {
			return err
		}
		if err := fs.Link(source, link.target); err != nil {
			return fmt.Errorf("image archive hard link %q: %w", link.name, err)
		}
	}
	return nil
}

func mkdirParent(fs *os.Root, name string) error {
	parent := filepath.Dir(name)
	if parent == "." {
		return nil
	}
	return fs.MkdirAll(parent, 0o755)
}

func mkdirTaskPath(root, containerPath string, perm os.FileMode) error {
	name, err := rootName(containerPath)
	if err != nil {
		return err
	}
	fs, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer fs.Close()
	if err := fs.MkdirAll(name, perm); err != nil {
		return err
	}
	return fs.Chmod(name, perm)
}

type archiveLink struct {
	target     string
	sourceName string
	name       string
}

func archiveName(name string) (string, bool, error) {
	cleaned := strings.TrimPrefix(filepath.Clean("/"+name), "/")
	if cleaned == "" || cleaned == "." {
		return "", false, nil
	}
	if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return "", false, fmt.Errorf("image archive path %q escapes the workspace", name)
	}
	return cleaned, true, nil
}

func relativeSymlink(linkName, raw string) (string, error) {
	const base = "/sandlock-root"
	if raw == "" || strings.ContainsRune(raw, 0) {
		return "", fmt.Errorf("image archive symlink %q has an empty target", linkName)
	}
	linkPath := filepath.Join(base, linkName)
	linkDir := filepath.Dir(linkPath)
	var destination string
	if filepath.IsAbs(raw) {
		cleaned := filepath.Clean(raw)
		if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
			return "", fmt.Errorf("image archive symlink %q escapes the workspace", linkName)
		}
		destination = filepath.Join(base, strings.TrimPrefix(cleaned, "/"))
	} else {
		destination = filepath.Clean(filepath.Join(linkDir, raw))
	}
	relative, err := filepath.Rel(base, destination)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("image archive symlink %q escapes the workspace", linkName)
	}
	rewritten, err := filepath.Rel(linkDir, destination)
	if err != nil {
		return "", fmt.Errorf("image archive symlink %q escapes the workspace", linkName)
	}
	return rewritten, nil
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
