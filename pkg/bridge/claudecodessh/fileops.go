package claudecodessh

// Package doc for this file: this is the SANDBOX side of the fix for
// rapport_integration_claude_code.md §7.5ter — Claude Code's native
// Write/Read/Edit/Glob/Grep tools operate on the harness container's local
// filesystem directly (Node.js fs), never invoking bash, so grammar.go's
// bash-tool grammar can never see them. The chosen fix (§9, option A) adds a
// companion MCP server, cmd/aries-claudecode-mcpfiles, that Claude Code calls
// INSTEAD of its native file tools (which are disabled via
// pkg/harness/claudecode/config.go's renderSettings, see permissions.deny).
// That MCP server is code we write, not Claude Code's own undocumented
// client, so — unlike grammar.go's wrapperPattern, which had to be
// reverse-engineered — the wire shape here is ours to define outright.
//
// Wire format: one SSH exec per call, argv[0] == "aries-fileop" (parallel to
// "bash" in bridge.go's handleSession dispatch), argv[1] the operation name,
// the rest its arguments, all \x1f-joined exactly like the bash wrapper (see
// pkg/harness/claudecode/config.go's bashWrapperScript). Every argument after
// the operation name is base64-encoded: a path or a search string can in
// principle contain any byte, including 0x1F itself, and there is no reason
// to risk that ambiguity when the fix is free.
//
// Every operation is executed as a real, argv-based (not shell-interpolated)
// command against the sandbox via bridgeSandbox.ExecStream, using
// "/usr/bin/env <tool> ..." rather than a hardcoded absolute path for the
// tool itself (base images differ on whether coreutils live under /bin or
// /usr/bin) — content and paths are never interpolated into a shell string,
// so there is no quoting/injection surface to get wrong here the way there
// was for the bash-wrapper grammar.
//
// STATUS: implemented, compiles, NOT yet empirically validated against a real
// MCP handshake from Claude Code (see rapport_integration_claude_code.md §9)
// — the same kind of validation that caught bugs 1-5 for the bash tool still
// needs to happen here before this can be trusted in production.

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
	"golang.org/x/crypto/ssh"
)

const (
	fileOpReadFile  = "read_file"
	fileOpWriteFile = "write_file"
	fileOpEditFile  = "edit_file"
	fileOpGlob      = "glob"
	fileOpGrep      = "grep"
)

// fileOp is the decoded representation of one "aries-fileop" exec command.
type fileOp struct {
	name string
	args []string // decoded (base64-unwrapped); meaning depends on name, see decodeFileOp
}

// decodeFileOp parses argv[1:] of an "aries-fileop" exec command — argv[0]
// ("aries-fileop") is stripped by the caller, the same convention
// decodeRemoteCommand uses for "bash". Expected shapes:
//
//	read_file  : [path]
//	write_file : [path]                     (content arrives over the channel's stdin)
//	edit_file  : [path, old_string, new_string]
//	glob       : [pattern, dir]
//	grep       : [pattern, dir]
func decodeFileOp(argv []string) (fileOp, error) {
	if len(argv) < 1 {
		return fileOp{}, errors.New("aries-fileop command is missing its operation name")
	}
	op := argv[0]
	args := make([]string, 0, len(argv)-1)
	for _, encoded := range argv[1:] {
		decoded, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return fileOp{}, fmt.Errorf("aries-fileop %s argument is not valid base64: %w", op, err)
		}
		args = append(args, string(decoded))
	}
	wantArgs := map[string]int{
		fileOpReadFile:  1,
		fileOpWriteFile: 1,
		fileOpEditFile:  3,
		fileOpGlob:      2,
		fileOpGrep:      2,
	}
	want, known := wantArgs[op]
	if !known {
		return fileOp{}, fmt.Errorf("unknown aries-fileop operation %q", op)
	}
	if len(args) != want {
		return fileOp{}, fmt.Errorf("aries-fileop %s expects %d argument(s), got %d", op, want, len(args))
	}
	return fileOp{name: op, args: args}, nil
}

// executeFileOp runs one decoded fileOp against the sandbox and reports the
// outcome back over the SSH channel, mirroring execute()'s shape (journal
// record, exit-status request) for the bash-tool path.
func (session *bridgeSession) executeFileOp(ctx context.Context, channel ssh.Channel, op fileOp) {
	started := time.Now()
	workdir := session.sandbox.Workdir()

	var (
		exitCode int
		runErr   error
	)
	switch op.name {
	case fileOpReadFile:
		result, err := session.sandbox.ExecStream(ctx,
			core.Command{Path: "/usr/bin/env", Args: []string{"cat", "--", op.args[0]}, Dir: workdir},
			nil, session.teeWriter(channel, "stdout"), session.teeWriter(channel.Stderr(), "stderr"))
		exitCode, runErr = result.ExitCode, err
	case fileOpWriteFile:
		exitCode, runErr = session.writeFile(ctx, channel, workdir, op.args[0])
	case fileOpEditFile:
		exitCode, runErr = session.editFile(ctx, channel, workdir, op.args[0], op.args[1], op.args[2])
	case fileOpGlob:
		result, err := session.sandbox.ExecStream(ctx,
			core.Command{Path: "/usr/bin/env", Args: []string{"find", op.args[1], "-type", "f", "-iname", op.args[0]}, Dir: workdir},
			nil, session.teeWriter(channel, "stdout"), session.teeWriter(channel.Stderr(), "stderr"))
		exitCode, runErr = result.ExitCode, err
	case fileOpGrep:
		result, err := session.sandbox.ExecStream(ctx,
			core.Command{Path: "/usr/bin/env", Args: []string{"grep", "-r", "-n", "-I", "--", op.args[0], op.args[1]}, Dir: workdir},
			nil, session.teeWriter(channel, "stdout"), session.teeWriter(channel.Stderr(), "stderr"))
		exitCode, runErr = result.ExitCode, err
	default:
		// Unreachable: decodeFileOp already rejects unknown operations.
		exitCode, runErr = 255, fmt.Errorf("unhandled aries-fileop operation %q", op.name)
	}

	status, message := "completed", ""
	if runErr != nil {
		status, message = "failed", runErr.Error()
		if exitCode == 0 {
			exitCode = 255
		}
	}
	exitCode = clampExitCode(exitCode)
	session.writeRecord(toolCallRecord{
		OperationClass: kindFileOp, Command: fileOpSummary(op), CommandHash: commandHash(fileOpSummary(op)),
		Workdir: workdir, ExitCode: exitCode,
		DurationMS: time.Since(started).Milliseconds(), Status: status, Error: message,
	})
	_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{uint32(exitCode)}))
}

// fileOpSummary is what gets journaled to tool-calls.jsonl — the operation
// and its path-like arguments, never file content (edit_file's old/new
// strings are omitted for the same reason the bash grammar only journals the
// command, not arbitrary stdin).
func fileOpSummary(op fileOp) string {
	switch op.name {
	case fileOpEditFile:
		return fmt.Sprintf("%s %s", op.name, op.args[0])
	default:
		return fmt.Sprintf("%s %s", op.name, strings.Join(op.args, " "))
	}
}

// writeFile streams the channel's stdin to path inside the sandbox, creating
// its parent directory first (matching Claude Code's native Write tool,
// which creates intermediate directories as needed). Both steps run as
// direct argv commands — no shell — so path is never interpolated into a
// command string.
func (session *bridgeSession) writeFile(ctx context.Context, channel ssh.Channel, workdir, path string) (int, error) {
	parent := filepath.Dir(path)
	if parent != "." && parent != "/" {
		result, err := session.sandbox.ExecStream(ctx,
			core.Command{Path: "/usr/bin/env", Args: []string{"mkdir", "-p", "--", parent}, Dir: workdir},
			nil, io.Discard, io.Discard)
		if err != nil {
			return 255, fmt.Errorf("write_file: create parent directory: %w", err)
		}
		if result.ExitCode != 0 {
			return result.ExitCode, fmt.Errorf("write_file: mkdir -p %q exited %d", parent, result.ExitCode)
		}
	}
	var stderr bytes.Buffer
	result, err := session.sandbox.ExecStream(ctx,
		core.Command{Path: "/usr/bin/env", Args: []string{"dd", "of=" + path, "status=none"}, Dir: workdir},
		session.teeReader(channel, "stdin"), io.Discard, &stderr)
	if err != nil {
		return 255, fmt.Errorf("write_file: %w", err)
	}
	if result.ExitCode != 0 {
		return result.ExitCode, fmt.Errorf("write_file: dd exited %d: %s", result.ExitCode, stderr.String())
	}
	return 0, nil
}

// editFile reproduces the native Edit tool's uniqueness contract: oldString
// must occur exactly once in the file. Both the read and the write happen
// bridge-side (not streamed to the channel) since the replacement itself
// must happen here; only the final success/error is reported to the caller.
func (session *bridgeSession) editFile(ctx context.Context, channel ssh.Channel, workdir, path, oldString, newString string) (int, error) {
	var stdout, stderr bytes.Buffer
	readResult, err := session.sandbox.ExecStream(ctx,
		core.Command{Path: "/usr/bin/env", Args: []string{"cat", "--", path}, Dir: workdir},
		nil, &stdout, &stderr)
	if err != nil {
		return 255, fmt.Errorf("edit_file: read: %w", err)
	}
	if readResult.ExitCode != 0 {
		return readResult.ExitCode, fmt.Errorf("edit_file: cat %q exited %d: %s", path, readResult.ExitCode, stderr.String())
	}
	content := stdout.String()
	count := strings.Count(content, oldString)
	switch {
	case count == 0:
		_, _ = fmt.Fprintf(session.teeWriter(channel.Stderr(), "stderr"), "edit_file: old_string not found in %s\n", path)
		return 1, fmt.Errorf("edit_file: old_string not found in %s", path)
	case count > 1:
		_, _ = fmt.Fprintf(session.teeWriter(channel.Stderr(), "stderr"), "edit_file: old_string is not unique in %s (%d occurrences)\n", path, count)
		return 1, fmt.Errorf("edit_file: old_string is not unique in %s (%d occurrences)", path, count)
	}
	updated := strings.Replace(content, oldString, newString, 1)
	var writeStderr bytes.Buffer
	writeResult, err := session.sandbox.ExecStream(ctx,
		core.Command{Path: "/usr/bin/env", Args: []string{"dd", "of=" + path, "status=none"}, Dir: workdir},
		strings.NewReader(updated), io.Discard, &writeStderr)
	if err != nil {
		return 255, fmt.Errorf("edit_file: write: %w", err)
	}
	if writeResult.ExitCode != 0 {
		return writeResult.ExitCode, fmt.Errorf("edit_file: dd exited %d: %s", writeResult.ExitCode, writeStderr.String())
	}
	return 0, nil
}
