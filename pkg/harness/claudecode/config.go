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
	"strings"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
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
// by direct experimentation: a bare `ANTHROPIC_API_KEY`
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
		// filesystem (Node.js fs), which can never be routed to the sandbox.
		// renderMCPConfig's separate file replaces them with sandbox-routed
		// equivalents (cmd/aries-claudecode-mcpfiles, decoded by
		// pkg/bridge/claudecodessh/fileops.go).
		//
		// A real `claude mcp --help` and `claude --help` on the
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
// mcpServers key (see renderSettings's doc comment). `--strict-mcp-
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
// "bash -l <script>" wire shape pkg/bridge/claudecodessh/grammar.go decodes.
// The shebang deliberately targets /bin/sh (a distinct binary, typically
// dash on Debian-based images), not bash, to avoid self-reference once this
// file has replaced /bin/bash.
//
// TODO(claude-code): requires an `ssh` client present in the harness image
// (openssh-client) — not yet added to any pinned image/profile.
//
// Two end-to-end failures fixed the wrapper encoding:
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

// execAttached runs one command through the deployment and returns its output.
// Nonzero exits are results. Cancellation and transport failures are errors.
func (manager *Manager) execAttached(ctx context.Context, containerID string, command []string, workdir string) (execResult, error) {
	if len(command) == 0 {
		return execResult{exitCode: -1}, errors.New("Claude Code exec command is empty")
	}
	result, err := manager.deployment.Exec(ctx, containerID, core.Command{
		Path: command[0], Args: command[1:], Dir: workdir, OutputLimitBytes: maxDockerOutput,
	})
	if err != nil {
		return execResult{stdout: []byte(result.Stdout), stderr: []byte(result.Stderr), exitCode: result.ExitCode}, err
	}
	return execResult{stdout: []byte(result.Stdout), stderr: []byte(result.Stderr), exitCode: result.ExitCode}, nil
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
		running, inspectErr := manager.deployment.Running(ctx, active.containerID)
		if inspectErr != nil {
			return fmt.Errorf("inspect Claude Code readiness: %w", inspectErr)
		}
		if !running {
			return errors.New("Claude Code runtime exited before readiness")
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
// the bash-forwarding wrapper needs. The wrapper itself is staged at
// bashWrapperPath so it replaces the image's /bin/bash.
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
		// The companion MCP file-tools server (see renderMCPConfig).
		// Must be linux/amd64 (or whatever arch the pinned image actually
		// runs), matching the caller-supplied MCPFilesBinaryPath — see
		// harness.go's Options.MCPFilesBinaryPath doc comment.
		strings.TrimPrefix(mcpFilesContainerPath, "/"): {content: mcpFilesBinary, mode: 0o755},
	}
	return stageArchive(files)
}

// stageArchive owns every entry by runtimeUID/runtimeGID (harness.go), not
// root. Claude Code refuses `--dangerously-skip-permissions` as root, so the container's default user
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
	if active.containerID != "" {
		if err := manager.deployment.Stop(ctx, active.containerID); err != nil {
			clearSessionSecrets(active)
			return err
		}
		active.containerID = ""
	}
	clearSessionSecrets(active)
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
