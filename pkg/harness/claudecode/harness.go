// Package claudecode implements runner.AgentHarness for Claude Code.
// The harness runtime is created through deployment.Deployment. One
// non-interactive Claude Code invocation runs the task instruction, then
// artifacts are collected before the runtime is stopped.
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
	"github.com/hyscale-lab/aries/pkg/deployment"
	"github.com/hyscale-lab/aries/pkg/runner"
	"github.com/sirupsen/logrus"
)

const (
	defaultCleanupTimeout = 30 * time.Second
	defaultStartTimeout   = 45 * time.Second
	defaultAgentTimeout   = 20 * time.Minute
	defaultMaxTurns       = 60
	maxDockerOutput       = 16 << 20
	maxAPIKeyBytes        = 16 << 10

	// runtimeUID/runtimeGID is the image user aries. Claude Code refuses
	// --dangerously-skip-permissions as root, so the container must run as
	// this unprivileged UID, and every staged file (see stageArchive in
	// config.go) must be owned by it. The image USER is aries; deployment
	// requests have no user field. Pre-existing paths such as the npm install
	// must stay readable by this UID.
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

	// mcpFilesContainerPath is where cmd/aries-claudecode-mcpfiles is staged
	// (see config.go's runtimeArchive). Claude Code's native file tools write
	// the harness container, so the companion binary replaces them.
	mcpFilesContainerPath = stateRoot + "/bin/aries-claudecode-mcpfiles"

	// mcpConfigContainerPath is the file renderMCPConfig writes and Run's
	// `--mcp-config` flag points at. NOT settings.json: a real
	// `claude --help`/`claude mcp --help` on the pinned CLI showed
	// settings.json has no recognized `mcpServers` key at all — confirmed by
	// a debug trace showing the companion server was never spawned when it
	// was declared there.
	mcpConfigContainerPath = stateRoot + "/mcp-servers.json"

	// bashWrapperPath REPLACES the image's real /bin/bash — confirmed by
	// capture of live invocations that Claude
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

	// Claude Code writes projects/<escaped-cwd>/*.jsonl under CLAUDE_CONFIG_DIR.
	// Start sets that directory to stateRoot, so transcripts are not under
	// the image user's default ~/.claude.
	claudeProjectsRoot = stateRoot + "/projects"
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
	Image      string
	OutputDir  string
	Deployment deployment.Deployment
	// APIKeyLookup returns the model API key (ANTHROPIC_API_KEY by convention)
	// for one environment name. Same ownership contract as Hermes/OpenClaw: the
	// harness clones what it needs and clears the returned buffer.
	APIKeyLookup   func(string) ([]byte, bool)
	MaxTurns       int
	CleanupTimeout time.Duration
	StartTimeout   time.Duration
	AgentTimeout   time.Duration
	Logger         *logrus.Logger
	// MCPFilesBinaryPath is a host-local path to the compiled
	// cmd/aries-claudecode-mcpfiles binary (linux/amd64, matching the pinned
	// harness image), staged into every container so Claude Code's native
	// Write/Read/Edit/Glob/Grep tools — which cannot be routed to the sandbox,
	// cannot be routed to the sandbox. MCP file tools replace them. Required; there is no fallback default
	// because a missing/wrong-arch binary would fail silently at container
	// start otherwise.
	//
	// TODO(claude-code): wire this from cmd/aries's own config the same way
	// Image itself still needs proper cfg.Versions plumbing (see
	// cmd/aries/wiring.go's existing TODO) — for now this is a plain path the
	// caller must build (`go build -o <path> ./cmd/aries-claudecode-mcpfiles`)
	// and supply themselves.
	MCPFilesBinaryPath string
}

type Manager struct {
	deployment     deployment.Deployment
	image          string
	outputDir      string
	cleanupTimeout time.Duration
	startTimeout   time.Duration
	agentTimeout   time.Duration
	maxTurns       int
	logger         *logrus.Logger
	apiKeyLookup   func(string) ([]byte, bool)
	newID          func() (string, error)
	mcpFilesBinary string

	mu        sync.Mutex
	active    *session
	stopping  bool
	stopDone  chan struct{}
	stopErr   error
	closeOnce sync.Once
	closeErr  error
}

type session struct {
	deploymentRequest deployment.Request
	runID             string
	taskID            string
	attemptID         string
	containerName     string
	containerID       string
	artifactDir       string
	endpoint          core.ToolEndpoint
	model             core.ModelConfig
	agentTimeout      time.Duration
	apiKey            []byte
	runAttempted      bool
	logPaths          []string
}

var _ runner.AgentHarness = (*Manager)(nil)

func (manager *Manager) Close() error {
	if manager == nil {
		return nil
	}
	manager.closeOnce.Do(func() {
		if manager.deployment != nil {
			manager.closeErr = manager.deployment.Close()
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
	if options.Deployment == nil {
		return nil, errors.New("Claude Code deployment is required")
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
	if strings.TrimSpace(options.MCPFilesBinaryPath) == "" {
		return nil, errors.New("Claude Code MCP file-tools binary path is required (MCPFilesBinaryPath)")
	}
	if _, err := os.Stat(options.MCPFilesBinaryPath); err != nil {
		return nil, fmt.Errorf("Claude Code MCP file-tools binary: %w", err)
	}
	return &Manager{
		deployment: options.Deployment, image: options.Image, outputDir: outputDir,
		cleanupTimeout: options.CleanupTimeout, startTimeout: options.StartTimeout,
		agentTimeout: options.AgentTimeout, maxTurns: options.MaxTurns,
		logger: options.Logger, apiKeyLookup: options.APIKeyLookup, newID: randomID,
		mcpFilesBinary: options.MCPFilesBinaryPath,
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
	if err := validateHarnessResources(request); err != nil {
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
	// ANTHROPIC_CUSTOM_HEADERS, not a plain ANTHROPIC_WORKSPACE_ID var: an
	// earlier attempt set that instead (a real env var name found in the
	// CLI binary's strings) and it had NO effect — the identical `400
	// anthropic-workspace-id is required` error still surfaced. Decompiled
	// CLI strings showed the actual code path only attaches that header
	// automatically for OAuth ("user_oauth") sessions; for apiKeyHelper-
	// based auth the header must be supplied via ANTHROPIC_CUSTOM_HEADERS —
	// confirmed working end-to-end (`is_error:false`) against the real API
	// confirmed against the Anthropic API. Format: newline-separated
	// "Header-Name: value" pairs, applied to every inference/model-discovery
	// request regardless of auth type. Not a secret — safe to set directly.
	if strings.TrimSpace(request.Model.WorkspaceID) != "" {
		environment = append(environment, "ANTHROPIC_CUSTOM_HEADERS=anthropic-workspace-id: "+request.Model.WorkspaceID)
	}

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
	deploymentRequest := deployment.Request{
		Name: "aries-claude-code-" + id, Network: request.Network, CPU: request.CPU, MemoryMB: request.MemoryMB,
		Image: manager.image, Env: environment,
		Entrypoint: append([]string(nil), idleEntrypoint...),
		Args:       append([]string(nil), idleCommand...),
		Labels: map[string]string{
			"aries.managed": "true", "aries.kind": "claude-code-harness",
			"aries.component": "harness",
			"aries.run":       request.RunID, "aries.task": request.TaskID,
			"aries.attempt": id,
		},
	}
	active := &session{
		deploymentRequest: deploymentRequest,
		runID:             request.RunID, taskID: request.TaskID, attemptID: id,
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

	active.containerID, err = manager.deployment.Create(ctx, deploymentRequest)
	if err != nil {
		return fail(fmt.Errorf("create Claude Code runtime: %w", err))
	}
	if strings.TrimSpace(active.containerID) == "" {
		return fail(errors.New("deployment returned an empty Claude Code runtime ID"))
	}
	if err := manager.deployment.UploadArchive(ctx, active.containerID, "/", bytes.NewReader(archive)); err != nil {
		return fail(fmt.Errorf("copy private Claude Code runtime: %w", err))
	}
	if err := manager.deployment.Validate(ctx, active.containerID, active.deploymentRequest, [][]byte{active.apiKey}); err != nil {
		return fail(err)
	}
	if err := manager.deployment.Start(ctx, active.containerID); err != nil {
		return fail(fmt.Errorf("start Claude Code runtime: %w", err))
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

func validateHarnessResources(request core.HarnessRequest) error {
	if request.CPU != nil {
		scaled := *request.CPU * 1e9
		if *request.CPU <= 0 || math.IsNaN(*request.CPU) || math.IsInf(*request.CPU, 0) || scaled >= math.Exp2(63) {
			return errors.New("Claude Code CPU must be finite, positive, and convert to NanoCPUs below 2^63")
		}
	}
	if request.MemoryMB != nil {
		if *request.MemoryMB <= 0 || int64(*request.MemoryMB) > math.MaxInt64>>20 {
			return fmt.Errorf("Claude Code memory must be positive and no greater than %d MiB", int64(math.MaxInt64)>>20)
		}
	}
	return nil
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
		// always runs as the image user aries (uid 10000), never root.
		"--dangerously-skip-permissions",
		// Registers the companion MCP file-tools server (config.go's
		// renderMCPConfig) — NOT settings.json, which has no recognized
		// mcpServers key on the pinned CLI (see mcpConfigContainerPath's doc
		// comment).
		// --strict-mcp-config additionally ignores any ambient .mcp.json/
		// `claude mcp add` configuration the image might otherwise carry.
		"--mcp-config", mcpConfigContainerPath,
		"--strict-mcp-config",
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
	if logs, err := manager.deployment.Logs(ctx, active.containerID, maxDockerOutput); err != nil {
		errs = append(errs, fmt.Errorf("collect Claude Code runtime logs: %w", err))
	} else {
		path := filepath.Join(active.artifactDir, "container.log")
		if err := writeArtifact(path, redactSession(logs, active)); err != nil {
			errs = append(errs, err)
		} else {
			active.logPaths = appendUnique(active.logPaths, path)
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
	content, _, err := manager.deployment.DownloadArchive(ctx, active.containerID, claudeProjectsRoot)
	if err != nil {
		return nil, fmt.Errorf("copy Claude Code session transcripts: %w", err)
	}
	defer content.Close()
	archive, err := readAllBounded(content, maxDockerOutput)
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
