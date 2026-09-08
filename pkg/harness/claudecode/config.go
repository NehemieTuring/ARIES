package claudecode

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/client"
)

// renderSettings builds Claude Code's settings.json. It intentionally carries
// no secret: the API key is staged as a separate private file (modelKeyPath,
// 0600, owned by runtimeUID) and referenced by the top-level `apiKeyHelper`
// setting, which Claude Code invokes as a shell command and reads the key
// from stdout.
//
// This is not a made-up field name: an earlier version of this function used
// an `ANTHROPIC_API_KEY_HELPER` environment variable instead, which Claude
// Code does not recognize (silently ignored — no error, just falls through to
// "Not logged in"). The real mechanism was found by direct experimentation
// (see rapport_integration_claude_code.md §7.3): a bare `ANTHROPIC_API_KEY`
// env var is insufficient without an existing browser-login session, and even
// an `apiKeyHelper` that reads `$ANTHROPIC_API_KEY` fails with "did not
// return a value" — Claude Code appears to strip that variable before
// invoking the helper, precisely to avoid this circular shape. Reading from a
// file, as done here, is the form confirmed to work.
func renderSettings(model core.ModelConfig, maxTurns int) ([]byte, error) {
	settings := map[string]any{
		"apiKeyHelper": "cat " + modelKeyPath,
		"model":        model.Model,
		"max_turns":    maxTurns,
		// permissions.deny disables Claude Code's native Write/Read/Edit/Glob/
		// Grep tools outright: they operate on THIS container's own local
		// filesystem (Node.js fs), which can never be routed to the sandbox
		// (rapport_integration_claude_code.md §7.5ter) — renderMCPConfig's
		// separate file replaces them with sandbox-routed equivalents
		// (cmd/aries-claudecode-mcpfiles, decoded by
		// pkg/bridge/claudecodessh/fileops.go). See §9/§9.5 for the design.
		//
		// CONFIRMED (§9.5): a real `claude mcp --help`/`claude --help` on the
		// pinned CLI (2.1.245) shows MCP servers are NOT read from
		// settings.json — only `--mcp-config <file>` (see harness.go's Run,
		// which passes mcpConfigContainerPath), `.mcp.json`, or `claude mcp
		// add` register them. An earlier version of this function put
		// `mcpServers` here; it was silently ignored (confirmed by a debug
		// trace showing the companion server was never even spawned).
		// `permissions.deny` alone, by contrast, IS a real settings.json
		// field and was confirmed to not break/hang the CLI on its own.
		"permissions": map[string]any{
			"deny": []string{"Write", "Read", "Edit", "Glob", "Grep"},
		},
	}
	env := map[string]string{}
	if strings.TrimSpace(model.BaseURL) != "" {
		env["ANTHROPIC_BASE_URL"] = model.BaseURL
	}
	if strings.TrimSpace(model.WorkspaceID) != "" {
		// NOT ANTHROPIC_WORKSPACE_ID — tried first, had no effect (see
		// harness.go's Start, which sets the real, confirmed-working
		// ANTHROPIC_CUSTOM_HEADERS as a container env var). Setting it here
		// too, in settings.json's "env", is redundant with that but
		// harmless — kept for whatever settings.json's "env" reaches that a
		// container-level var might not (unconfirmed either way).
		env["ANTHROPIC_CUSTOM_HEADERS"] = "anthropic-workspace-id: " + model.WorkspaceID
	}
	if len(env) > 0 {
		settings["env"] = env
	}
	encoded, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("render Claude Code settings: %w", err)
	}
	return append(encoded, '\n'), nil
}

// renderMCPConfig builds the JSON file passed via `claude ... --mcp-config
// <file> --strict-mcp-config` (see harness.go's Run) — the confirmed-correct
// way to register the companion MCP file-tools server, unlike settings.json's
// mcpServers key (see renderSettings's doc comment and §9.5). `--strict-mcp-
// config` means only servers listed in this file are used, ignoring any
// ambient `.mcp.json`/`claude mcp add` configuration this container might
// otherwise pick up.
func renderMCPConfig(endpoint core.ToolEndpoint) ([]byte, error) {
	config := map[string]any{
		"mcpServers": map[string]any{
			"ariesfiles": map[string]any{
				"type":    "stdio",
				"command": mcpFilesContainerPath,
				"args":    []string{},
				// Passed explicitly rather than relied on via container-level
				// env inheritance: apiKeyHelper's own ANTHROPIC_API_KEY-
				// stripping behavior (see renderSettings's doc comment) is a
				// precedent for Claude Code sanitizing environments it hands
				// to processes it spawns itself — unconfirmed for MCP server
				// subprocesses specifically, so this costs nothing and
				// removes the question.
				"env": map[string]string{
					"ARIES_BRIDGE_ADDRESS":  endpoint.Address,
					"ARIES_BRIDGE_USERNAME": endpoint.Username,
					"ARIES_BRIDGE_IDENTITY": identityContainerFS,
				},
			},
		},
	}
	encoded, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("render Claude Code MCP config: %w", err)
	}
	return append(encoded, '\n'), nil
}

// containerEnvironment supplies bashWrapperScript (below) with the bridge
// endpoint it forwards every invocation to.
func containerEnvironment(endpoint core.ToolEndpoint) ([]string, error) {
	if endpoint.Protocol != "ssh" {
		return nil, fmt.Errorf("Claude Code bridge endpoint must be ssh, got %q", endpoint.Protocol)
	}
	return []string{
		"ARIES_BRIDGE_ADDRESS=" + endpoint.Address,
		"ARIES_BRIDGE_USERNAME=" + endpoint.Username,
		"ARIES_BRIDGE_IDENTITY=" + identityContainerFS,
	}, nil
}

// bashWrapperScript replaces bashWrapperPath (/bin/bash) entirely inside the
// harness container. It forwards every invocation verbatim as one SSH exec
// request to the sandbox, reconstructing the exact "bash -c <script>" /
// "bash -l <script>" wire shape pkg/bridge/claudecodessh/grammar.go decodes
// (captured empirically, see rapport_integration_claude_code.md §7.2/§7.4).
// The shebang deliberately targets /bin/sh (a distinct binary, typically
// dash on Debian-based images), not bash, to avoid self-reference once this
// file has replaced /bin/bash.
//
// TODO(claude-code): requires an `ssh` client present in the harness image
// (openssh-client) — not yet added to any pinned image/profile.
//
// BUG FOUND AND FIXED TWICE during end-to-end testing
// (rapport_integration_claude_code.md §7.6):
//
//  1. The once-per-session snapshot generator is invoked as THREE arguments —
//     `bash -c -l "<script>"` — not two. An earlier version of this script
//     only read $1 (flag) and $2 (script), silently dropping the real script.
//  2. Joining reconstructed arguments with plain spaces (even after fixing
//     #1) is fundamentally ambiguous for the bridge to re-split: real per-call
//     scripts turned out to sometimes themselves start with a token that
//     looks like a flag (observed: `bash -c -l shopt -u extglob ...`, where
//     the real script argument is `shopt -u extglob ...`, not preceded by the
//     `source $SNAPSHOT_FILE ...` line captured in an earlier, apparently
//     not-universal, run). Re-splitting on spaces cannot distinguish
//     "argument boundary" from "space inside the script text" once both have
//     been flattened into one string.
//
// The fix carries every original argv element across the wire verbatim,
// joined by the ASCII Unit Separator (0x1F) — a byte that cannot appear in a
// shell flag and is exceedingly unlikely to appear in real command text
// (the same non-printable-delimiter convention already used elsewhere in
// ARIES, e.g. Hermes's exec exit-trailer). No flags/script split happens on
// this side at all: every argv element $1..$n is forwarded as-is, and
// pkg/bridge/claudecodessh's splitCommand on the other end reverses this
// exactly with strings.Split, so there is no re-parsing ambiguity left.
const bashWrapperScript = `#!/bin/sh
set -e
if [ "$#" -lt 2 ]; then
  echo "aries claude-code bash wrapper: unsupported invocation: $*" >&2
  exit 1
fi
sep="$(printf '\037')"
payload="bash"
for arg in "$@"; do
  payload="$payload$sep$arg"
done
host="${ARIES_BRIDGE_ADDRESS%:*}"
port="${ARIES_BRIDGE_ADDRESS##*:}"
exec ssh -i "$ARIES_BRIDGE_IDENTITY" -p "$port" \
  -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o BatchMode=yes \
  "$ARIES_BRIDGE_USERNAME@$host" "$payload"
`

type execResult struct {
	stdout   []byte
	stderr   []byte
	exitCode int
}

// execAttached, waitExec, and the exit-trailer mechanism are copied from
// pkg/harness/hermes/harness.go's equivalents (same rationale: Docker's exec
// inspection can race a fast child, so a delimited stderr trailer is the
// authoritative exit status rather than a second inspect call). Kept
// per-package rather than shared, matching the existing OpenClaw/Hermes
// duplication in this codebase (see rapport_integration_claude_code.md §2.3).
const execShell = `token=$1
shift
"$@"
status=$?
printf '\036ARIES_CLAUDE_EXIT_%s=%s\037' "$token" "$status" >&2
exit "$status"`

const execTrailerKeep = 256

func (manager *Manager) execAttached(ctx context.Context, containerID string, command []string, workdir string) (execResult, error) {
	token, err := randomID()
	if err != nil {
		return execResult{exitCode: -1}, fmt.Errorf("generate Claude Code exec token: %w", err)
	}
	wrapped := append([]string{"/bin/sh", "-c", execShell, "aries-claude-exec", token}, command...)
	created, err := manager.client.ExecCreate(ctx, containerID, client.ExecCreateOptions{
		AttachStdout: true, AttachStderr: true, Cmd: wrapped, WorkingDir: workdir,
	})
	if err != nil {
		return execResult{exitCode: -1}, fmt.Errorf("create Claude Code exec: %w", err)
	}
	attached, err := manager.client.ExecAttach(ctx, created.ID, client.ExecAttachOptions{})
	if err != nil {
		return execResult{exitCode: -1}, fmt.Errorf("attach Claude Code exec: %w", err)
	}
	defer attached.Close()
	_ = attached.CloseWrite()
	var stdout, stderr limitedBuffer
	stdout.limit, stderr.limit = maxDockerOutput, maxDockerOutput
	trailer := newExecTrailer(&stderr, token)
	copyDone := make(chan error, 1)
	go func() {
		_, err := stdcopy.StdCopy(&stdout, trailer, attached.Reader)
		copyDone <- err
	}()
	inspectDone := make(chan client.ExecInspectResult, 1)
	inspectErr := make(chan error, 1)
	inspectCtx, cancelInspect := context.WithCancel(ctx)
	defer cancelInspect()
	go func() {
		inspection, err := manager.waitExec(inspectCtx, created.ID)
		if err != nil {
			inspectErr <- err
			return
		}
		inspectDone <- inspection
	}()
	var copyErr error
	streamDone := false
	finished := false
	for !finished {
		select {
		case <-ctx.Done():
			attached.Close()
			if !streamDone {
				<-copyDone
			}
			return execResult{stdout: stdout.Bytes(), stderr: stderr.Bytes(), exitCode: -1}, ctx.Err()
		case err := <-inspectErr:
			attached.Close()
			if !streamDone {
				<-copyDone
			}
			return execResult{stdout: stdout.Bytes(), stderr: stderr.Bytes(), exitCode: -1}, err
		case <-inspectDone:
			finished = true
		case <-trailer.done:
			finished = true
		case copyErr = <-copyDone:
			streamDone = true
		}
	}
	cancelInspect()
	if !streamDone {
		select {
		case copyErr = <-copyDone:
			streamDone = true
		case <-time.After(200 * time.Millisecond):
			attached.Close()
			<-copyDone
			streamDone = true
			copyErr = nil
		}
	}
	if copyErr != nil {
		return execResult{stdout: stdout.Bytes(), stderr: stderr.Bytes(), exitCode: -1}, fmt.Errorf("read Claude Code exec: %w", copyErr)
	}
	if stdout.exceeded || stderr.exceeded {
		return execResult{stdout: stdout.Bytes(), stderr: stderr.Bytes(), exitCode: -1}, errors.New("Claude Code exec output exceeded its bound")
	}
	exitCode, err := trailer.Finish()
	if err != nil {
		return execResult{stdout: stdout.Bytes(), stderr: stderr.Bytes(), exitCode: -1}, err
	}
	return execResult{stdout: stdout.Bytes(), stderr: stderr.Bytes(), exitCode: exitCode}, nil
}

func (manager *Manager) waitExec(ctx context.Context, execID string) (client.ExecInspectResult, error) {
	const (
		firstInterval = 20 * time.Millisecond
		lastInterval  = time.Second
	)
	interval := firstInterval
	timer := time.NewTimer(interval)
	defer timer.Stop()
	for {
		inspection, err := manager.client.ExecInspect(ctx, execID, client.ExecInspectOptions{})
		if err != nil {
			return client.ExecInspectResult{}, fmt.Errorf("inspect Claude Code exec: %w", err)
		}
		if !inspection.Running {
			return inspection, nil
		}
		select {
		case <-ctx.Done():
			return client.ExecInspectResult{}, ctx.Err()
		case <-timer.C:
		}
		if interval < lastInterval {
			interval = min(interval*2, lastInterval)
		}
		timer.Reset(interval)
	}
}

type execTrailer struct {
	destination io.Writer
	prefix      []byte
	buffer      bytes.Buffer
	done        chan struct{}
	once        sync.Once
}

func newExecTrailer(destination io.Writer, token string) *execTrailer {
	return &execTrailer{destination: destination, prefix: []byte("\x1eARIES_CLAUDE_EXIT_" + token + "="), done: make(chan struct{})}
}

func (trailer *execTrailer) Write(content []byte) (int, error) {
	written, _ := trailer.buffer.Write(content)
	buffered := trailer.buffer.Bytes()
	if len(buffered) > 0 && buffered[len(buffered)-1] == '\x1f' && bytes.LastIndex(buffered[:len(buffered)-1], trailer.prefix) >= 0 {
		trailer.once.Do(func() { close(trailer.done) })
	}
	if excess := trailer.buffer.Len() - execTrailerKeep; excess > 0 {
		chunk := trailer.buffer.Next(excess)
		n, err := trailer.destination.Write(chunk)
		if err != nil {
			return 0, err
		}
		if n != len(chunk) {
			return 0, io.ErrShortWrite
		}
	}
	return written, nil
}

func (trailer *execTrailer) Finish() (int, error) {
	content := trailer.buffer.Bytes()
	if len(content) == 0 || content[len(content)-1] != '\x1f' {
		return -1, errors.New("Claude Code exec output is missing its exit trailer")
	}
	start := bytes.LastIndex(content[:len(content)-1], trailer.prefix)
	if start < 0 {
		return -1, errors.New("Claude Code exec output has an invalid exit trailer")
	}
	exitCode, err := strconv.Atoi(string(content[start+len(trailer.prefix) : len(content)-1]))
	if err != nil || exitCode < 0 || exitCode > 255 {
		return -1, errors.New("Claude Code exec output has an invalid exit code")
	}
	if _, err := trailer.destination.Write(content[:start]); err != nil {
		return -1, fmt.Errorf("write Claude Code exec stderr: %w", err)
	}
	return exitCode, nil
}

type limitedBuffer struct {
	bytes.Buffer
	limit    int
	exceeded bool
}

func (buffer *limitedBuffer) Write(content []byte) (int, error) {
	consumed := len(content)
	remaining := buffer.limit - buffer.Len()
	if len(content) > remaining {
		content = content[:max(0, remaining)]
		buffer.exceeded = true
	}
	_, err := buffer.Buffer.Write(content)
	return consumed, err
}

// waitReady confirms the container is running and the staged runtime is
// readable before any task instruction is accepted (same rationale as
// Hermes's waitReady: Claude Code exposes no readiness service of its own).
func (manager *Manager) waitReady(ctx context.Context, active *session) error {
	probe := `test -r ` + settingsContainerPath + ` && test -r ` + modelKeyPath +
		` && test -r ` + identityContainerFS + ` && command -v claude >/dev/null`
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		result, err := manager.execAttached(probeCtx, active.containerID, []string{"/bin/sh", "-c", probe}, workspaceRoot)
		cancel()
		if err == nil && result.exitCode == 0 {
			return nil
		}
		inspection, inspectErr := manager.client.ContainerInspect(ctx, active.containerID, client.ContainerInspectOptions{})
		if inspectErr != nil {
			return fmt.Errorf("inspect Claude Code readiness: %w", inspectErr)
		}
		if inspection.Container.State == nil || !inspection.Container.State.Running {
			return errors.New("Claude Code container exited before readiness")
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("await Claude Code readiness: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

type stagedFile struct {
	content []byte
	mode    int64
}

// runtimeArchive stages the settings file, the API key, and the SSH identity
// the bash-forwarding wrapper needs. TODO(claude-code): once
// pkg/bridge/claudecodessh exists, also stage the wrapper script itself at
// bashWrapperPath here (mirroring Hermes's agentWrapperScript), so it shadows
// the image's real /bin/bash on PATH — see rapport_integration_claude_code.md
// §3 option A.
func (manager *Manager) runtimeArchive(active *session, settings []byte) ([]byte, error) {
	identity, err := readStablePrivateFile(active.endpoint.IdentitySourceFile, 0o600)
	if err != nil {
		return nil, fmt.Errorf("read Claude Code SSH identity: %w", err)
	}
	defer clear(identity)
	mcpFilesBinary, err := os.ReadFile(manager.mcpFilesBinary)
	if err != nil {
		return nil, fmt.Errorf("read Claude Code MCP file-tools binary: %w", err)
	}
	mcpConfig, err := renderMCPConfig(active.endpoint)
	if err != nil {
		return nil, err
	}
	files := map[string]stagedFile{
		strings.TrimPrefix(settingsContainerPath, "/"):  {content: settings, mode: 0o600},
		strings.TrimPrefix(modelKeyPath, "/"):           {content: active.apiKey, mode: 0o600},
		strings.TrimPrefix(identityContainerFS, "/"):    {content: identity, mode: 0o600},
		strings.TrimPrefix(mcpConfigContainerPath, "/"): {content: mcpConfig, mode: 0o600},
		// Replaces the image's real /bin/bash outright — see bashWrapperPath
		// and bashWrapperScript's doc comments for why no bash.real fallback
		// is staged alongside it.
		strings.TrimPrefix(bashWrapperPath, "/"): {content: []byte(bashWrapperScript), mode: 0o755},
		// The companion MCP file-tools server (see this package's mcpServers
		// entry in renderSettings and rapport_integration_claude_code.md §9).
		// Must be linux/amd64 (or whatever arch the pinned image actually
		// runs), matching the caller-supplied MCPFilesBinaryPath — see
		// harness.go's Options.MCPFilesBinaryPath doc comment.
		strings.TrimPrefix(mcpFilesContainerPath, "/"): {content: mcpFilesBinary, mode: 0o755},
	}
	return stageArchive(files)
}

// stageArchive owns every entry by runtimeUID/runtimeGID (harness.go), not
// root. Claude Code refuses `--dangerously-skip-permissions` as root (see
// rapport_integration_claude_code.md §7.3), so the container's default user
// must be non-root — and a non-root user cannot read a root-owned 0600 file
// such as modelKeyPath, which the apiKeyHelper mechanism depends on. This
// was caught by inspection before the first real end-to-end image build,
// mirroring Hermes's identical ownership rationale
// (pkg/harness/hermes/harness.go's runtimeUID/runtimeGID comment).
func stageArchive(files map[string]stagedFile) ([]byte, error) {
	var output bytes.Buffer
	writer := tar.NewWriter(&output)
	directories := []string{"run/aries", "run/aries/claude-code", "run/aries/claude-code/ssh", "run/aries/claude-code/bin", "home/aries/workspace"}
	for _, name := range directories {
		mode := int64(0o700)
		if name == "home/aries/workspace" {
			mode = 0o755
		}
		if err := writer.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeDir, Mode: mode, Uid: runtimeUID, Gid: runtimeGID}); err != nil {
			return nil, err
		}
	}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		file := files[name]
		if name == "" || filepath.IsAbs(name) || filepath.Clean(name) != name || strings.HasPrefix(name, "../") {
			return nil, fmt.Errorf("invalid staged Claude Code path %q", name)
		}
		header := &tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: file.mode, Size: int64(len(file.content)), Uid: runtimeUID, Gid: runtimeGID}
		if err := writer.WriteHeader(header); err != nil {
			return nil, err
		}
		if _, err := writer.Write(file.content); err != nil {
			return nil, err
		}
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

func (manager *Manager) stopSession(ctx context.Context, active *session) error {
	if active == nil {
		return nil
	}
	if active.containerID == "" {
		clearSessionSecrets(active)
		return nil
	}
	var errs []error
	inspection, inspectErr := manager.client.ContainerInspect(ctx, active.containerID, client.ContainerInspectOptions{})
	notFound := inspectErr != nil && isNotFound(inspectErr)
	if notFound {
		active.containerID = ""
		clearSessionSecrets(active)
		return nil
	}
	if inspectErr != nil {
		errs = append(errs, fmt.Errorf("inspect Claude Code before stop: %w", inspectErr))
	}
	shouldStop := inspectErr != nil || inspection.Container.State == nil || inspection.Container.State.Running
	if shouldStop {
		timeout := gracefulStopSeconds
		if _, err := manager.client.ContainerStop(ctx, active.containerID, client.ContainerStopOptions{Timeout: &timeout}); err != nil && !isNotFound(err) {
			errs = append(errs, fmt.Errorf("stop Claude Code container: %w", err))
		}
		inspection, inspectErr = manager.client.ContainerInspect(ctx, active.containerID, client.ContainerInspectOptions{})
		if inspectErr != nil && !isNotFound(inspectErr) {
			errs = append(errs, fmt.Errorf("inspect Claude Code after stop: %w", inspectErr))
		}
		if !isNotFound(inspectErr) && (inspectErr != nil || inspection.Container.State == nil || inspection.Container.State.Running) {
			if _, err := manager.client.ContainerKill(ctx, active.containerID, client.ContainerKillOptions{Signal: "KILL"}); err != nil && !isNotFound(err) {
				errs = append(errs, fmt.Errorf("kill Claude Code container: %w", err))
			}
		}
	}
	if _, err := manager.client.ContainerRemove(ctx, active.containerID, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true}); err != nil && !isNotFound(err) {
		errs = append(errs, fmt.Errorf("remove Claude Code container: %w", err))
	}
	active.containerID = ""
	clearSessionSecrets(active)
	if warning := errors.Join(errs...); warning != nil {
		manager.logger.WithContext(ctx).WithField("task_id", active.taskID).WithError(warning).Warn("Claude Code cleanup recovered after lifecycle errors")
	}
	return nil
}

func failedHarnessResult(active *session, started time.Time, err error) core.HarnessResult {
	status := core.StatusFailed
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		status = core.StatusCanceled
	}
	errorText := ""
	if err != nil {
		errorText = string(redactSession([]byte(err.Error()), active))
	}
	return core.HarnessResult{Status: status, Duration: time.Since(started), LogPaths: append([]string(nil), active.logPaths...), Error: errorText}
}

func readStablePrivateFile(path string, mode os.FileMode) ([]byte, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != mode {
		return nil, errors.New("private source is not one regular file with the required mode")
	}
	return content, nil
}

func readAllBounded(reader io.Reader, limit int) ([]byte, error) {
	content, err := io.ReadAll(io.LimitReader(reader, int64(limit)+1))
	if err != nil {
		return nil, err
	}
	if len(content) > limit {
		return nil, errors.New("content exceeded its bound")
	}
	return content, nil
}

func writeArtifact(path string, content []byte) error {
	if err := ensurePrivateDirectory(filepath.Dir(path)); err != nil {
		return err
	}
	return os.WriteFile(path, content, 0o600)
}

func appendUnique(paths []string, additions ...string) []string {
	seen := make(map[string]struct{}, len(paths)+len(additions))
	for _, path := range paths {
		seen[path] = struct{}{}
	}
	for _, path := range additions {
		if _, ok := seen[path]; ok {
			continue
		}
		paths = append(paths, path)
		seen[path] = struct{}{}
	}
	return paths
}

func telemetryRelativePaths(artifactDir string, paths []string) []string {
	prefix := filepath.Join(artifactDir, "telemetry") + string(filepath.Separator)
	var relative []string
	for _, path := range paths {
		if strings.HasPrefix(path, prefix) {
			name, err := filepath.Rel(artifactDir, path)
			if err == nil {
				relative = append(relative, filepath.ToSlash(name))
			}
		}
	}
	return relative
}

func ensurePrivateDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	return os.Chmod(path, 0o700)
}

func validateAPIKey(value []byte) error {
	if len(value) == 0 || len(value) > maxAPIKeyBytes {
		return errors.New("Claude Code API key is empty or exceeds its bound")
	}
	if bytes.ContainsAny(value, "\x00\r\n") {
		return errors.New("Claude Code API key contains NUL or a line break")
	}
	return nil
}

func environmentAPIKeyLookup(name string) ([]byte, bool) {
	value, ok := os.LookupEnv(name)
	return []byte(value), ok
}

func validateRunID(value string) error {
	if value == "" || len(value) > 128 || !safeIdentifierChars(value) {
		return errors.New("Claude Code run ID must contain 1 to 128 safe characters")
	}
	return nil
}

func validateTaskID(value string) error {
	if value == "" || len(value) > 149 || !safeIdentifierChars(value) {
		return errors.New("Claude Code task ID is invalid")
	}
	return nil
}

func safeIdentifierChars(value string) bool {
	for index, character := range value {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || index > 0 && (character == '-' || character == '_' || character == '.') {
			continue
		}
		return false
	}
	return true
}

func randomID() (string, error) {
	var content [8]byte
	if _, err := rand.Read(content[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(content[:]), nil
}

func clearSessionSecrets(active *session) {
	clear(active.apiKey)
	active.apiKey = nil
}

func redactSecrets(content []byte, secrets ...[]byte) []byte {
	for _, secret := range secrets {
		if len(secret) == 0 {
			continue
		}
		content = bytes.ReplaceAll(content, secret, []byte("[REDACTED]"))
	}
	return content
}

func redactSession(content []byte, active *session) []byte {
	return redactSecrets(content, active.apiKey)
}

type sessionRedactedError struct {
	message string
	cause   error
}

func (err *sessionRedactedError) Error() string { return err.message }
func (err *sessionRedactedError) Unwrap() error { return err.cause }

func redactSessionError(err error, active *session) error {
	if err == nil {
		return nil
	}
	message := string(redactSession([]byte(err.Error()), active))
	if message == err.Error() {
		return err
	}
	return &sessionRedactedError{message: message, cause: err}
}

// isNotFound is a narrow stand-in for errdefs.IsNotFound (used by Hermes/
// OpenClaw) to avoid pulling in that dependency for this skeleton — replace
// with errdefs.IsNotFound(err) when wiring this package for real.
func isNotFound(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "no such container")
}
