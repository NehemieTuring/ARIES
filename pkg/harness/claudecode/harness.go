// Package claudecode implements runner.AgentHarness for Claude Code, Anthropic's
// CLI coding agent, following the same lifecycle shape as pkg/harness/hermes:
// an idle Docker container is created and staged first, then one non-interactive
// invocation runs the task instruction, then artifacts are collected before the
// container is torn down.
//
// STATUS: structural skeleton produced from rapport_integration_claude_code.md.
// It compiles against the same core/runner contracts Hermes and OpenClaw use,
// but several points are marked TODO pending the empirical steps in that report
// (in particular step 2: capturing Claude Code's real Bash-tool wire behavior
// before the paired bridge package can be finished). Do not wire this into
// cmd/aries without completing those TODOs and the bridge in
// pkg/bridge/claudecodessh.
package claudecode

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/hyscale-lab/aries/pkg/containerimage"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/runner"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
	"github.com/sirupsen/logrus"
)

const (
	defaultDockerSocket   = "/var/run/docker.sock"
	defaultCleanupTimeout = 30 * time.Second
	defaultStartTimeout   = 45 * time.Second
	defaultAgentTimeout   = 20 * time.Minute
	defaultMaxTurns       = 60
	maxDockerOutput       = 16 << 20
	maxAPIKeyBytes        = 16 << 10
	gracefulStopSeconds   = 5

	// runtimeUID/runtimeGID: the container's default user, matching
	// pkg/harness/hermes's identical constant and rationale — Claude Code
	// refuses --dangerously-skip-permissions as root (§7.3 of the integration
	// report), so the container must run as this unprivileged, fixed UID, and
	// every staged file (see stageArchive in config.go) must be owned by it.
	// The pinned image is expected to also default to this UID (e.g. via a
	// Dockerfile `USER` matching it) — Start does not depend on that alone
	// and sets it explicitly on the container, but a mismatched image UID for
	// pre-existing paths (e.g. npm's global install, Claude Code's own files)
	// would still need to be readable by this UID.
	runtimeUID = 10000
	runtimeGID = 10000

	// workspaceRoot is the workdir every exec and the agent invocation itself
	// run from. It matches the sandbox-visible path Claude Code should treat as
	// its project root.
	workspaceRoot = "/home/aries/workspace"

	// stateRoot holds everything staged privately for the container: the
	// rendered settings file, the API key, and the SSH identity used by the
	// bash-forwarding wrapper (see config.go and the bridge package).
	stateRoot             = "/run/aries/claude-code"
	settingsContainerPath = stateRoot + "/settings.json"
	modelKeyPath          = stateRoot + "/api-key"
	identityContainerFS   = stateRoot + "/ssh/id_ed25519"

	// bashWrapperPath REPLACES the image's real /bin/bash — confirmed by
	// empirical capture (rapport_integration_claude_code.md §7.2) that Claude
	// Code's Bash tool invokes this exact absolute path directly, never via a
	// PATH lookup (a PATH-shadowing shim at e.g. /usr/local/bin/bash was tried
	// first and never observed being invoked). Nothing else in this
	// container legitimately needs a local, non-forwarding bash: Claude
	// Code's own bootstrap invocations (the `env` probe and the one-time
	// shell-snapshot generator, see grammar.go) are themselves forwarded to
	// the sandbox like any other Bash-tool call, so there is no bash.real
	// fallback to preserve here — every invocation is meant to run in the
	// sandbox, never locally.
	bashWrapperPath = "/bin/bash"

	// claudeProjectsGlob is where Claude Code persists its own JSONL session
	// transcripts, keyed by an escaped form of the working directory. TODO:
	// confirm the exact escaping scheme against the pinned image's Claude Code
	// version before relying on this path in collectSessions.
	claudeProjectsRoot = "/home/aries/.claude/projects"
)

// idleEntrypoint/idleCommand mirror Hermes: ARIES owns exactly when the agent
// starts, so the container's own entrypoint is replaced with an indefinite
// sleep until Run is called.
var (
	idleEntrypoint = []string{"/bin/sh"}
	idleCommand    = []string{"-c", "exec sleep infinity"}
)

// Options are the host-local inputs to one Claude Code container.
type Options struct {
	Image        string
	OutputDir    string
	DockerSocket string
	// APIKeyLookup returns the model API key (ANTHROPIC_API_KEY by convention)
	// for one environment name. Same ownership contract as Hermes/OpenClaw: the
	// harness clones what it needs and clears the returned buffer.
	APIKeyLookup   func(string) ([]byte, bool)
	MaxTurns       int
	CleanupTimeout time.Duration
	StartTimeout   time.Duration
	AgentTimeout   time.Duration
	Logger         *logrus.Logger
}

// dockerClient is the small Engine SDK surface this harness needs — the same
// shape as Hermes's, plus CopyFromContainer for session-transcript export.
type dockerClient interface {
	ContainerCreate(context.Context, client.ContainerCreateOptions) (client.ContainerCreateResult, error)
	CopyToContainer(context.Context, string, client.CopyToContainerOptions) (client.CopyToContainerResult, error)
	CopyFromContainer(context.Context, string, client.CopyFromContainerOptions) (client.CopyFromContainerResult, error)
	ContainerStart(context.Context, string, client.ContainerStartOptions) (client.ContainerStartResult, error)
	ContainerInspect(context.Context, string, client.ContainerInspectOptions) (client.ContainerInspectResult, error)
	ExecCreate(context.Context, string, client.ExecCreateOptions) (client.ExecCreateResult, error)
	ExecAttach(context.Context, string, client.ExecAttachOptions) (client.ExecAttachResult, error)
	ExecInspect(context.Context, string, client.ExecInspectOptions) (client.ExecInspectResult, error)
	ContainerLogs(context.Context, string, client.ContainerLogsOptions) (client.ContainerLogsResult, error)
	ContainerStop(context.Context, string, client.ContainerStopOptions) (client.ContainerStopResult, error)
	ContainerKill(context.Context, string, client.ContainerKillOptions) (client.ContainerKillResult, error)
	ContainerRemove(context.Context, string, client.ContainerRemoveOptions) (client.ContainerRemoveResult, error)
}

type Manager struct {
	client         dockerClient
	image          string
	outputDir      string
	cleanupTimeout time.Duration
	startTimeout   time.Duration
	agentTimeout   time.Duration
	maxTurns       int
	logger         *logrus.Logger
	apiKeyLookup   func(string) ([]byte, bool)
	newID          func() (string, error)

	mu        sync.Mutex
	active    *session
	stopping  bool
	stopDone  chan struct{}
	stopErr   error
	closeOnce sync.Once
	closeErr  error
}

type session struct {
	runID         string
	taskID        string
	attemptID     string
	containerName string
	containerID   string
	artifactDir   string
	endpoint      core.ToolEndpoint
	model         core.ModelConfig
	agentTimeout  time.Duration
	apiKey        []byte
	runAttempted  bool
	logPaths      []string
}

var _ runner.AgentHarness = (*Manager)(nil)

func (manager *Manager) Close() error {
	if manager == nil {
		return nil
	}
	manager.closeOnce.Do(func() {
		if closer, ok := manager.client.(interface{ Close() error }); ok {
			manager.closeErr = closer.Close()
		}
	})
	return manager.closeErr
}

// New constructs a harness without contacting Docker.
func New(options Options) (*Manager, error) {
	if err := containerimage.ValidatePinnedTagOnly(options.Image); err != nil {
		return nil, fmt.Errorf("Claude Code image: %w", err)
	}
	if strings.TrimSpace(options.OutputDir) == "" {
		return nil, errors.New("Claude Code output directory is required")
	}
	outputDir, err := filepath.Abs(options.OutputDir)
	if err != nil {
		return nil, fmt.Errorf("resolve Claude Code output directory: %w", err)
	}
	if err := ensurePrivateDirectory(outputDir); err != nil {
		return nil, fmt.Errorf("prepare Claude Code output directory: %w", err)
	}
	if options.DockerSocket == "" {
		options.DockerSocket = defaultDockerSocket
	}
	host := options.DockerSocket
	if !strings.Contains(host, "://") {
		host = "unix://" + host
	}
	api, err := client.New(client.WithHost(host), client.WithUserAgent("aries-claude-code/1"))
	if err != nil {
		return nil, fmt.Errorf("create Docker client: %w", err)
	}
	if options.CleanupTimeout <= 0 {
		options.CleanupTimeout = defaultCleanupTimeout
	}
	if options.StartTimeout <= 0 {
		options.StartTimeout = defaultStartTimeout
	}
	if options.AgentTimeout <= 0 {
		options.AgentTimeout = defaultAgentTimeout
	}
	if options.MaxTurns <= 0 {
		options.MaxTurns = defaultMaxTurns
	}
	if options.Logger == nil {
		options.Logger = logrus.StandardLogger()
	}
	if options.APIKeyLookup == nil {
		options.APIKeyLookup = environmentAPIKeyLookup
	}
	return &Manager{
		client: api, image: options.Image, outputDir: outputDir,
		cleanupTimeout: options.CleanupTimeout, startTimeout: options.StartTimeout,
		agentTimeout: options.AgentTimeout, maxTurns: options.MaxTurns,
		logger: options.Logger, apiKeyLookup: options.APIKeyLookup, newID: randomID,
	}, nil
}

func (manager *Manager) Start(ctx context.Context, request core.HarnessRequest) error {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.active != nil || manager.stopping {
		return errors.New("Claude Code harness is already active")
	}
	if err := validateRunID(request.RunID); err != nil {
		return err
	}
	if err := validateTaskID(request.TaskID); err != nil {
		return err
	}
	resources, err := harnessResources(request)
	if err != nil {
		return err
	}
	agentTimeout := request.Timeout
	if agentTimeout == 0 {
		agentTimeout = manager.agentTimeout
	}

	settings, err := renderSettings(request.Model, manager.maxTurns)
	if err != nil {
		return err
	}
	// TODO(claude-code): confirm whether Claude Code needs any additional
	// container environment beyond ANTHROPIC_API_KEY / ANTHROPIC_BASE_URL —
	// e.g. a disabled-telemetry flag, or a config-dir override so it reads
	// settings.json from stateRoot instead of the default `~/.claude`.
	bridgeEnv, err := containerEnvironment(request.Endpoint)
	if err != nil {
		return err
	}
	environment := append([]string{
		"HOME=/home/aries",
		"CLAUDE_CONFIG_DIR=" + stateRoot,
	}, bridgeEnv...)

	apiKeySource, ok := manager.apiKeyLookup(request.Model.APIKeyEnv)
	if !ok {
		clear(apiKeySource)
		return fmt.Errorf("Claude Code API-key environment %q is not set", request.Model.APIKeyEnv)
	}
	apiKey := bytes.Clone(apiKeySource)
	clear(apiKeySource)
	if err := validateAPIKey(apiKey); err != nil {
		clear(apiKey)
		return err
	}
	if bytes.Contains(settings, apiKey) {
		clear(apiKey)
		return errors.New("rendered Claude Code settings contain the API-key value")
	}

	id, err := manager.newID()
	if err != nil {
		clear(apiKey)
		return fmt.Errorf("generate Claude Code harness ID: %w", err)
	}
	containerConfig := &container.Config{
		Image: manager.image,
		// Explicit, not left to the image default — see runtimeUID's doc
		// comment. Root would work for the idle/bootstrap command, but Claude
		// Code itself refuses to run non-interactively as root.
		User:       fmt.Sprintf("%d:%d", runtimeUID, runtimeGID),
		Env:        environment,
		Entrypoint: append([]string(nil), idleEntrypoint...),
		Cmd:        append([]string(nil), idleCommand...),
		Labels: map[string]string{
			"aries.managed": "true", "aries.kind": "claude-code-harness",
			"aries.component": "harness",
			"aries.run":       request.RunID, "aries.task": request.TaskID,
			"aries.attempt": id,
		},
	}
	hostConfig := &container.HostConfig{NetworkMode: container.NetworkMode(request.Endpoint.Network), Resources: resources}
	active := &session{
		runID: request.RunID, taskID: request.TaskID, attemptID: id,
		containerName: "aries-claude-code-" + id,
		artifactDir:   filepath.Join(manager.outputDir, request.TaskID, "harness"),
		endpoint:      request.Endpoint, model: request.Model,
		agentTimeout: agentTimeout, apiKey: apiKey,
	}

	fail := func(primary error) error {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), manager.cleanupTimeout)
		cleanupErr := manager.stopSession(cleanupCtx, active)
		cancel()
		if cleanupErr != nil {
			manager.active = active
			manager.stopErr = cleanupErr
			return errors.Join(primary, fmt.Errorf("rollback partial Claude Code harness: %w", cleanupErr))
		}
		_ = os.RemoveAll(active.artifactDir)
		return primary
	}

	if err := ensurePrivateDirectory(active.artifactDir); err != nil {
		return fail(fmt.Errorf("create Claude Code artifact directory: %w", err))
	}
	settingsArtifact := filepath.Join(active.artifactDir, "settings.json")
	if err := writeArtifact(settingsArtifact, redactSession(settings, active)); err != nil {
		return fail(fmt.Errorf("retain rendered Claude Code settings: %w", err))
	}
	active.logPaths = appendUnique(active.logPaths, settingsArtifact)

	archive, err := manager.runtimeArchive(active, settings)
	if err != nil {
		return fail(err)
	}
	defer clear(archive)

	created, err := manager.client.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name: active.containerName, Config: containerConfig, HostConfig: hostConfig,
	})
	if err != nil {
		return fail(fmt.Errorf("create Claude Code container: %w", err))
	}
	active.containerID = created.ID
	if strings.TrimSpace(active.containerID) == "" {
		return fail(errors.New("Docker returned an empty Claude Code container ID"))
	}
	if _, err := manager.client.CopyToContainer(ctx, active.containerID, client.CopyToContainerOptions{
		DestinationPath: "/", Content: bytes.NewReader(archive), CopyUIDGID: true,
	}); err != nil {
		return fail(fmt.Errorf("copy private Claude Code runtime: %w", err))
	}
	if _, err := manager.client.ContainerStart(ctx, active.containerID, client.ContainerStartOptions{}); err != nil {
		return fail(fmt.Errorf("start Claude Code container: %w", err))
	}
	readyCtx, cancel := context.WithTimeout(ctx, manager.startTimeout)
	err = manager.waitReady(readyCtx, active)
	cancel()
	if err != nil {
		return fail(err)
	}
	manager.active = active
	manager.stopErr = nil
	manager.logger.WithContext(ctx).WithFields(logrus.Fields{"task_id": active.taskID, "container": active.containerName}).Info("Claude Code harness started")
	return nil
}

func harnessResources(request core.HarnessRequest) (container.Resources, error) {
	var resources container.Resources
	if request.CPU != nil {
		scaled := *request.CPU * 1e9
		if *request.CPU <= 0 || math.IsNaN(*request.CPU) || math.IsInf(*request.CPU, 0) || scaled >= math.Exp2(63) {
			return container.Resources{}, errors.New("Claude Code CPU must be finite, positive, and convert to NanoCPUs below 2^63")
		}
		resources.NanoCPUs = int64(scaled)
	}
	if request.MemoryMB != nil {
		if *request.MemoryMB <= 0 || int64(*request.MemoryMB) > math.MaxInt64>>20 {
			return container.Resources{}, fmt.Errorf("Claude Code memory must be positive and no greater than %d MiB", int64(math.MaxInt64)>>20)
		}
		resources.Memory = int64(*request.MemoryMB) << 20
	}
	return resources, nil
}

// Run launches exactly one non-interactive Claude Code invocation:
//
//	claude -p "<instruction>" --output-format json --max-turns <N>
//
// TODO(claude-code): confirm this is still the correct headless invocation
// form (and JSON schema) against the pinned image's Claude Code version before
// relying on FinalResponse extraction below — CLI flags are not part of
// ARIES's compatibility surface and can change between releases.
func (manager *Manager) Run(ctx context.Context, instruction string) (core.HarnessResult, error) {
	started := time.Now()
	manager.mu.Lock()
	active := manager.active
	if active == nil {
		manager.mu.Unlock()
		return core.HarnessResult{Status: core.StatusFailed}, errors.New("Claude Code harness is not started")
	}
	if active.runAttempted {
		manager.mu.Unlock()
		return core.HarnessResult{Status: core.StatusFailed}, errors.New("Claude Code harness accepts exactly one task instruction")
	}
	if strings.TrimSpace(instruction) == "" || strings.ContainsRune(instruction, 0) {
		manager.mu.Unlock()
		return core.HarnessResult{Status: core.StatusFailed}, errors.New("Claude Code task instruction is invalid")
	}
	active.runAttempted = true
	local := *active
	active = &local
	manager.mu.Unlock()

	runCtx, cancel := context.WithTimeout(ctx, active.agentTimeout)
	command := []string{
		"claude", "-p", instruction,
		"--output-format", "json",
		"--max-turns", fmt.Sprintf("%d", manager.maxTurns),
		// Required for non-interactive use: Claude Code otherwise blocks on
		// permission prompts with no TTY to answer them. Only usable as a
		// non-root user (Claude Code refuses it as root) — the container
		// always runs as runtimeUID, never root, so this is safe here (see
		// rapport_integration_claude_code.md §7.3).
		"--dangerously-skip-permissions",
	}
	result, runErr := manager.execAttached(runCtx, active.containerID, command, workspaceRoot)
	cancel()

	stdout := redactSession(result.stdout, active)
	stderr := redactSession(result.stderr, active)
	err := runErr
	if err == nil && result.exitCode != 0 {
		err = fmt.Errorf("Claude Code invocation exited with status %d", result.exitCode)
	}

	finalResponse, parseErr := extractFinalResponse(stdout)
	if parseErr != nil && err == nil {
		// A malformed --output-format json payload is a harness bug, not a task
		// failure worth masking — surface it, but keep the raw stdout artifact
		// below so it can be inspected.
		err = fmt.Errorf("parse Claude Code output: %w", parseErr)
	}

	artifactCtx, artifactCancel := context.WithTimeout(context.WithoutCancel(ctx), manager.cleanupTimeout)
	artifactErr := manager.collectArtifacts(artifactCtx, active, stdout, stderr)
	artifactCancel()
	err = errors.Join(err, artifactErr)
	if err != nil {
		err = redactSessionError(err, active)
		return failedHarnessResult(active, started, err), err
	}
	return core.HarnessResult{
		Status: core.StatusSucceeded, FinalResponse: finalResponse,
		Duration: time.Since(started), LogPaths: append([]string(nil), active.logPaths...),
	}, nil
}

// claudeJSONOutput is the subset of `--output-format json`'s schema this
// harness relies on. TODO(claude-code): verify field names against the pinned
// version — this is a best-effort shape, not confirmed against a real run.
type claudeJSONOutput struct {
	Result string `json:"result"`
}

func extractFinalResponse(stdout []byte) (string, error) {
	trimmed := bytes.TrimSpace(stdout)
	if len(trimmed) == 0 {
		return "", errors.New("Claude Code produced no output")
	}
	var parsed claudeJSONOutput
	if err := json.Unmarshal(trimmed, &parsed); err != nil {
		return "", err
	}
	return parsed.Result, nil
}

func (manager *Manager) Stop(ctx context.Context) error {
	manager.mu.Lock()
	if manager.active == nil && !manager.stopping {
		err := manager.stopErr
		manager.mu.Unlock()
		return err
	}
	if manager.stopping {
		done := manager.stopDone
		manager.mu.Unlock()
		select {
		case <-done:
			manager.mu.Lock()
			err := manager.stopErr
			manager.mu.Unlock()
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	active := manager.active
	manager.stopping = true
	manager.stopDone = make(chan struct{})
	done := manager.stopDone
	manager.mu.Unlock()

	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), manager.cleanupTimeout)
	err := manager.stopSession(cleanupCtx, active)
	cancel()

	manager.mu.Lock()
	manager.stopErr = err
	manager.stopping = false
	if active == nil || active.containerID == "" {
		manager.active = nil
	}
	close(done)
	manager.mu.Unlock()
	return err
}

func (manager *Manager) collectArtifacts(ctx context.Context, active *session, stdout, stderr []byte) error {
	var errs []error
	for _, item := range []struct {
		name    string
		content []byte
	}{{"claude_stdout.log", stdout}, {"claude_stderr.log", stderr}} {
		path := filepath.Join(active.artifactDir, item.name)
		if err := writeArtifact(path, item.content); err != nil {
			errs = append(errs, fmt.Errorf("retain %s: %w", item.name, err))
			continue
		}
		active.logPaths = appendUnique(active.logPaths, path)
	}
	if logs, err := manager.client.ContainerLogs(ctx, active.containerID, client.ContainerLogsOptions{ShowStdout: true, ShowStderr: true}); err != nil {
		errs = append(errs, fmt.Errorf("collect Claude Code container logs: %w", err))
	} else {
		var out, errBuffer limitedBuffer
		out.limit, errBuffer.limit = maxDockerOutput, maxDockerOutput
		_, copyErr := stdcopy.StdCopy(&out, &errBuffer, logs)
		closeErr := logs.Close()
		if copyErr != nil || closeErr != nil {
			errs = append(errs, errors.Join(copyErr, closeErr))
		} else {
			path := filepath.Join(active.artifactDir, "container.log")
			content := redactSession(append(out.Bytes(), errBuffer.Bytes()...), active)
			if err := writeArtifact(path, content); err != nil {
				errs = append(errs, err)
			} else {
				active.logPaths = appendUnique(active.logPaths, path)
			}
		}
	}
	sessionPaths, sessionErr := manager.collectSessions(ctx, active)
	if sessionErr != nil {
		// Non-fatal by design (see Hermes's collectSessions): a run that never
		// reached the model may leave no transcript, and that must not mask an
		// otherwise-successful task result.
		manager.logger.WithContext(ctx).WithField("task_id", active.taskID).WithError(sessionErr).Debug("Claude Code produced no session export")
	} else {
		active.logPaths = appendUnique(active.logPaths, sessionPaths...)
	}
	index, err := json.MarshalIndent(struct {
		Paths []string `json:"paths"`
	}{Paths: telemetryRelativePaths(active.artifactDir, active.logPaths)}, "", "  ")
	if err == nil {
		index = append(index, '\n')
		path := filepath.Join(active.artifactDir, "telemetry.index.json")
		err = writeArtifact(path, index)
		if err == nil {
			active.logPaths = appendUnique(active.logPaths, path)
		}
	}
	if err != nil {
		errs = append(errs, fmt.Errorf("write Claude Code telemetry index: %w", err))
	}
	return errors.Join(errs...)
}

// collectSessions copies out Claude Code's own JSONL transcript(s) under
// claudeProjectsRoot, mirroring OpenClaw's CopyFromContainer + tar-extract
// pattern (pkg/harness/openclaw/harness.go collectTelemetry/extractTelemetry)
// rather than Hermes's stdout-export pattern, since Claude Code has no
// equivalent `sessions export -` subcommand.
//
// TODO(claude-code): this is unverified — confirm the real transcript path
// (project-directory escaping scheme) and file naming against the pinned
// image before depending on it.
func (manager *Manager) collectSessions(ctx context.Context, active *session) ([]string, error) {
	result, err := manager.client.CopyFromContainer(ctx, active.containerID, client.CopyFromContainerOptions{SourcePath: claudeProjectsRoot})
	if err != nil {
		return nil, fmt.Errorf("copy Claude Code session transcripts: %w", err)
	}
	defer result.Content.Close()
	archive, err := readAllBounded(result.Content, maxDockerOutput)
	if err != nil {
		return nil, fmt.Errorf("read Claude Code session archive: %w", err)
	}
	return extractSessionArchive(active.artifactDir, archive, active.apiKey)
}

func extractSessionArchive(artifactDir string, archive []byte, secrets ...[]byte) ([]string, error) {
	reader := tar.NewReader(bytes.NewReader(archive))
	var written []string
	for {
		header, err := reader.Next()
		if err != nil {
			break // EOF or truncated archive; best-effort like OpenClaw's extractTelemetry.
		}
		if header.Typeflag != tar.TypeReg || !strings.HasSuffix(header.Name, ".jsonl") {
			continue
		}
		content, err := readAllBounded(reader, maxDockerOutput)
		if err != nil {
			continue
		}
		destination := filepath.Join(artifactDir, "telemetry", filepath.Base(header.Name))
		if err := writeArtifact(destination, redactSecrets(content, secrets...)); err != nil {
			continue
		}
		written = append(written, destination)
	}
	return written, nil
}
