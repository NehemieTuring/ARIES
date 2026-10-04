package sandbox

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/hyscale-lab/aries/pkg/containerimage"
	sandlocksandbox "github.com/hyscale-lab/aries/pkg/sandbox/sandlock"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

// seedFromImage copies the image root filesystem into the private directory,
// then removes the helper container. The helper runs only long enough to
// install curl and uv when the image does not already have them, because
// Terminal-Bench verifiers download their test runner that way and this host
// cannot map the task uid to 0. The helper never runs Sandlock.
func seedFromImage(ctx context.Context, image, root, workdir string) error {
	if err := validateSeedImage(image); err != nil {
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
	if err := sandlocksandbox.ImportRoot(root, workdir, copied.Content); err != nil {
		return fmt.Errorf("sandlock workspace seed: %w", err)
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

func validateSeedImage(image string) error {
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
