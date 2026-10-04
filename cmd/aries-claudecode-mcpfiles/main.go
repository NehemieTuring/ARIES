// Command aries-claudecode-mcpfiles is the companion MCP server for Claude
// Code file tools. The native Write/Read/Edit/Glob/Grep tools operate on the
// harness container's own local filesystem (Node.js fs), never through bash,
// so they can never be routed to the sandbox by the bash-tool wrapper.
//
// The harness copies this binary into the container and registers it with
// --mcp-config. permissions.deny disables the native file tools. Claude Code
// spawns this process on stdio and calls write_file, read_file, edit_file,
// glob, and grep. Each call dials the same SSH bridge as the bash tool, using
// ARIES_BRIDGE_ADDRESS, ARIES_BRIDGE_USERNAME, and ARIES_BRIDGE_IDENTITY, and
// sends the aries-fileop wire format decoded by
// pkg/bridge/claudecodessh/fileops.go.
//
// A live MCP handshake is still required before treating the protocol version
// and permissions.deny behavior in headless mode as confirmed.
package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// Operation names — MUST match the fileOpXxx constants in
// pkg/bridge/claudecodessh/fileops.go exactly. Duplicated rather than shared
// because that package is internal to the bridge and this binary is spawned
// as a separate OS process, not linked against it; a mismatch here would be
// caught immediately by the bridge rejecting every call as an unknown
// operation, so this is easy to notice in testing, not a silent risk.
const (
	opReadFile  = "read_file"
	opWriteFile = "write_file"
	opEditFile  = "edit_file"
	opGlob      = "glob"
	opGrep      = "grep"
)

func main() {
	log.SetFlags(0)
	log.SetPrefix("aries-claudecode-mcpfiles: ")
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

// debugTrace opens a raw byte-level log of every incoming and outgoing message
// when ARIES_MCPFILES_DEBUG is set. It is a diagnostic for the stdio framing.
func debugTrace() io.Writer {
	path := os.Getenv("ARIES_MCPFILES_DEBUG")
	if path == "" {
		return nil
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return nil
	}
	return file
}

func run() error {
	var stdin io.Reader = os.Stdin
	var stdout io.Writer = os.Stdout
	if trace := debugTrace(); trace != nil {
		_, _ = fmt.Fprintf(trace, "--- session start pid=%d ---\n", os.Getpid())
		stdin = io.TeeReader(os.Stdin, prefixWriter{trace, "IN  "})
		stdout = io.MultiWriter(os.Stdout, prefixWriter{trace, "OUT "})
	}
	decoder := json.NewDecoder(stdin)
	encoder := json.NewEncoder(stdout)
	for {
		var request rpcMessage
		if err := decoder.Decode(&request); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("decode MCP request: %w", err)
		}
		response, hasResponse := dispatch(request)
		if !hasResponse {
			// A JSON-RPC notification (no "id") — e.g. "notifications/initialized" —
			// gets no reply at all, per the JSON-RPC 2.0 spec MCP builds on.
			continue
		}
		if err := encoder.Encode(response); err != nil {
			return fmt.Errorf("encode MCP response: %w", err)
		}
	}
}

// prefixWriter is only used by the temporary debugTrace diagnostic above.
type prefixWriter struct {
	destination io.Writer
	label       string
}

func (writer prefixWriter) Write(content []byte) (int, error) {
	_, err := fmt.Fprintf(writer.destination, "%s %q\n", writer.label, content)
	return len(content), err
}

// rpcMessage covers both requests/notifications received and responses sent:
// a minimal hand-rolled JSON-RPC 2.0 envelope (no external MCP SDK dependency
// — see the package doc comment on why this is hand-rolled).
type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func dispatch(request rpcMessage) (rpcMessage, bool) {
	isNotification := len(request.ID) == 0 || string(request.ID) == "null"
	reply := func(result any, err *rpcError) (rpcMessage, bool) {
		if isNotification {
			return rpcMessage{}, false
		}
		return rpcMessage{JSONRPC: "2.0", ID: request.ID, Result: result, Error: err}, true
	}
	switch request.Method {
	case "initialize":
		return reply(handleInitialize(request.Params), nil)
	case "notifications/initialized":
		return reply(nil, nil) // notification; reply() drops it via isNotification anyway
	case "ping":
		return reply(map[string]any{}, nil)
	case "tools/list":
		return reply(map[string]any{"tools": toolDefinitions()}, nil)
	case "tools/call":
		result, callErr := handleToolsCall(request.Params)
		if callErr != nil {
			return reply(nil, &rpcError{Code: -32000, Message: callErr.Error()})
		}
		return reply(result, nil)
	default:
		return reply(nil, &rpcError{Code: -32601, Message: "method not found: " + request.Method})
	}
}

// handleInitialize echoes back whatever protocolVersion the client asked
// for, rather than asserting a specific version string of our own — the
// exact value Claude Code's current release sends is unconfirmed (see the
// package doc comment); this tolerant approach avoids a hard version
// mismatch until that can be verified empirically.
func handleInitialize(params json.RawMessage) map[string]any {
	var decoded struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	_ = json.Unmarshal(params, &decoded)
	protocolVersion := decoded.ProtocolVersion
	if protocolVersion == "" {
		protocolVersion = "2024-11-05"
	}
	return map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities":    map[string]any{"tools": map[string]any{}},
		"serverInfo":      map[string]any{"name": "aries-claudecode-mcpfiles", "version": "0.1.0"},
	}
}

func toolDefinitions() []map[string]any {
	return []map[string]any{
		{
			"name":        opWriteFile,
			"description": "Write content to a file in the sandbox (creates parent directories and the file itself; overwrites if it exists).",
			"inputSchema": objectSchema(map[string]any{
				"file_path": stringProp("Absolute or workdir-relative path of the file to write."),
				"content":   stringProp("The full content to write to the file."),
			}, "file_path", "content"),
		},
		{
			"name":        opReadFile,
			"description": "Read the full content of a file in the sandbox.",
			"inputSchema": objectSchema(map[string]any{
				"file_path": stringProp("Path of the file to read."),
			}, "file_path"),
		},
		{
			"name":        opEditFile,
			"description": "Replace one exact, unique occurrence of old_string with new_string in a file in the sandbox. Fails if old_string is missing or appears more than once.",
			"inputSchema": objectSchema(map[string]any{
				"file_path":  stringProp("Path of the file to edit."),
				"old_string": stringProp("The exact text to replace; must occur exactly once in the file."),
				"new_string": stringProp("The text to replace it with."),
			}, "file_path", "old_string", "new_string"),
		},
		{
			"name":        opGlob,
			"description": "Find files in the sandbox matching a glob pattern (e.g. \"*.go\").",
			"inputSchema": objectSchema(map[string]any{
				"pattern": stringProp("Glob pattern to match file names against."),
				"path":    stringProp("Directory to search from (default: the current workdir)."),
			}, "pattern"),
		},
		{
			"name":        opGrep,
			"description": "Search file contents in the sandbox for a pattern, recursively.",
			"inputSchema": objectSchema(map[string]any{
				"pattern": stringProp("Pattern to search for (grep -E style)."),
				"path":    stringProp("Directory to search from (default: the current workdir)."),
			}, "pattern"),
		},
	}
}

func stringProp(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}

func objectSchema(properties map[string]any, required ...string) map[string]any {
	return map[string]any{
		"type":       "object",
		"properties": properties,
		"required":   required,
	}
}

func handleToolsCall(params json.RawMessage) (map[string]any, error) {
	var call struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(params, &call); err != nil {
		return nil, fmt.Errorf("decode tools/call params: %w", err)
	}
	text, isError := callTool(call.Name, call.Arguments)
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
		"isError": isError,
	}, nil
}

func callTool(name string, arguments map[string]any) (text string, isError bool) {
	switch name {
	case opWriteFile:
		return handleWriteFile(arguments)
	case opReadFile:
		return handleReadFile(arguments)
	case opEditFile:
		return handleEditFile(arguments)
	case opGlob:
		return handleGlob(arguments)
	case opGrep:
		return handleGrep(arguments)
	default:
		return "unknown tool: " + name, true
	}
}

func stringArg(arguments map[string]any, key string) string {
	value, _ := arguments[key].(string)
	return value
}

func handleWriteFile(arguments map[string]any) (string, bool) {
	path := stringArg(arguments, "file_path")
	if path == "" {
		return "file_path is required", true
	}
	content := stringArg(arguments, "content")
	_, stderr, exitCode, err := runFileOp(opWriteFile, []string{path}, strings.NewReader(content))
	if err != nil {
		return fmt.Sprintf("write_file %s failed: %v", path, err), true
	}
	if exitCode != 0 {
		return fmt.Sprintf("write_file %s failed (exit %d): %s", path, exitCode, stderr), true
	}
	return fmt.Sprintf("Wrote %d bytes to %s", len(content), path), false
}

func handleReadFile(arguments map[string]any) (string, bool) {
	path := stringArg(arguments, "file_path")
	if path == "" {
		return "file_path is required", true
	}
	stdout, stderr, exitCode, err := runFileOp(opReadFile, []string{path}, nil)
	if err != nil {
		return fmt.Sprintf("read_file %s failed: %v", path, err), true
	}
	if exitCode != 0 {
		return fmt.Sprintf("read_file %s failed (exit %d): %s", path, exitCode, stderr), true
	}
	return stdout, false
}

func handleEditFile(arguments map[string]any) (string, bool) {
	path := stringArg(arguments, "file_path")
	oldString := stringArg(arguments, "old_string")
	newString := stringArg(arguments, "new_string")
	if path == "" || oldString == "" {
		return "file_path and old_string are required", true
	}
	stdout, stderr, exitCode, err := runFileOp(opEditFile, []string{path, oldString, newString}, nil)
	if err != nil {
		return fmt.Sprintf("edit_file %s failed: %v", path, err), true
	}
	if exitCode != 0 {
		message := strings.TrimSpace(stderr)
		if message == "" {
			message = strings.TrimSpace(stdout)
		}
		return fmt.Sprintf("edit_file %s failed: %s", path, message), true
	}
	return fmt.Sprintf("Edited %s", path), false
}

func handleGlob(arguments map[string]any) (string, bool) {
	pattern := stringArg(arguments, "pattern")
	if pattern == "" {
		return "pattern is required", true
	}
	dir := stringArg(arguments, "path")
	if dir == "" {
		dir = "."
	}
	stdout, stderr, exitCode, err := runFileOp(opGlob, []string{pattern, dir}, nil)
	if err != nil {
		return fmt.Sprintf("glob %s failed: %v", pattern, err), true
	}
	if exitCode != 0 {
		return fmt.Sprintf("glob %s failed (exit %d): %s", pattern, exitCode, stderr), true
	}
	if strings.TrimSpace(stdout) == "" {
		return "no files matched", false
	}
	return stdout, false
}

func handleGrep(arguments map[string]any) (string, bool) {
	pattern := stringArg(arguments, "pattern")
	if pattern == "" {
		return "pattern is required", true
	}
	dir := stringArg(arguments, "path")
	if dir == "" {
		dir = "."
	}
	stdout, stderr, exitCode, err := runFileOp(opGrep, []string{pattern, dir}, nil)
	if err != nil {
		return fmt.Sprintf("grep %s failed: %v", pattern, err), true
	}
	// grep exits 1 (not an error) when nothing matches — distinct from a real
	// failure (exit >= 2, or stderr non-empty).
	if exitCode == 1 && strings.TrimSpace(stderr) == "" {
		return "no matches found", false
	}
	if exitCode != 0 {
		return fmt.Sprintf("grep %s failed (exit %d): %s", pattern, exitCode, stderr), true
	}
	return stdout, false
}

// runFileOp dials the claudecodessh bridge fresh for every call (one exec
// channel per call, mirroring the bash tool's own discrete-exec model — see
// pkg/bridge/claudecodessh/grammar.go's DESIGN NOTE) and sends one
// "aries-fileop" command, base64-encoding every argument after the operation
// name (see pkg/bridge/claudecodessh/fileops.go's package doc comment for
// why).
func runFileOp(op string, args []string, stdin io.Reader) (stdout, stderr string, exitCode int, err error) {
	client, err := dialBridge()
	if err != nil {
		return "", "", -1, err
	}
	defer client.Close()
	session, err := client.NewSession()
	if err != nil {
		return "", "", -1, fmt.Errorf("open bridge session: %w", err)
	}
	defer session.Close()

	parts := make([]string, 0, len(args)+2)
	parts = append(parts, "aries-fileop", op)
	for _, argument := range args {
		parts = append(parts, base64.StdEncoding.EncodeToString([]byte(argument)))
	}
	command := strings.Join(parts, "\x1f")

	var outBuf, errBuf bytes.Buffer
	session.Stdout = &outBuf
	session.Stderr = &errBuf
	if stdin != nil {
		session.Stdin = stdin
	}
	runErr := session.Run(command)
	if runErr == nil {
		return outBuf.String(), errBuf.String(), 0, nil
	}
	var exitError *ssh.ExitError
	if errors.As(runErr, &exitError) {
		return outBuf.String(), errBuf.String(), exitError.ExitStatus(), nil
	}
	return outBuf.String(), errBuf.String(), -1, fmt.Errorf("run bridge command: %w", runErr)
}

func dialBridge() (*ssh.Client, error) {
	address := os.Getenv("ARIES_BRIDGE_ADDRESS")
	username := os.Getenv("ARIES_BRIDGE_USERNAME")
	identityPath := os.Getenv("ARIES_BRIDGE_IDENTITY")
	if address == "" || username == "" || identityPath == "" {
		return nil, errors.New("ARIES_BRIDGE_ADDRESS, ARIES_BRIDGE_USERNAME and ARIES_BRIDGE_IDENTITY must all be set")
	}
	keyBytes, err := os.ReadFile(identityPath)
	if err != nil {
		return nil, fmt.Errorf("read bridge identity %q: %w", identityPath, err)
	}
	signer, err := ssh.ParsePrivateKey(keyBytes)
	if err != nil {
		return nil, fmt.Errorf("parse bridge identity: %w", err)
	}
	config := &ssh.ClientConfig{
		User:            username,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	}
	return ssh.Dial("tcp", address, config)
}
