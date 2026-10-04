// Package claudecodessh is the ToolBridge paired with pkg/harness/claudecode.
// Each Claude Code command is one SSH exec channel, the same model the Hermes
// and OpenClaw bridges use.
package claudecodessh

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/runner"
	"github.com/sirupsen/logrus"
	"golang.org/x/crypto/ssh"
)

const (
	defaultBridgeCleanup  = 20 * time.Second
	maxRecordedInputBytes = 16 << 20
	maxRawLogBytes        = 256 << 20

	identityContainerPath = "/run/aries/claude-code/ssh/id_ed25519"
	lockedUsername        = "aries"
	lockedConnectTimeout  = 5 * time.Second
)

// Options are the host-local inputs to one Claude Code SSH bridge.
type Options struct {
	OutputDir      string
	CleanupTimeout time.Duration
	Logger         *logrus.Logger
	// ResolveListen supplies the task-local listener and the address the harness dials.
	ResolveListen func(context.Context) (core.BridgeListen, error)
	// OmitRawLog drops ssh_raw.log — same rationale as hermesssh.Options.
	OmitRawLog bool
}

// bridgeSandbox is the same capability surface hermesssh/openclawssh require.
type bridgeSandbox interface {
	runner.Sandbox
	ContainerID() string
	ContainerName() string
	NetworkName() string
	RunID() string
	TaskID() string
	Workdir() string
	ExecStream(context.Context, core.Command, io.Reader, io.Writer, io.Writer) (core.CommandResult, error)
}

type Manager struct {
	outputDir      string
	cleanupTimeout time.Duration
	logger         *logrus.Logger
	omitRawLog     bool
	resolveListen  func(context.Context) (core.BridgeListen, error)

	mu       sync.Mutex
	active   *bridgeSession
	stopping bool
	stopDone chan struct{}
	stopErr  error
}

type bridgeSession struct {
	sandbox        bridgeSandbox
	listener       net.Listener
	configuration  *ssh.ServerConfig
	cancel         context.CancelFunc
	artifactDir    string
	identitySource string
	toolLogPath    string
	rawLogPath     string
	toolLog        *lockedBoundedFile
	rawLog         *lockedBoundedFile
	partialStart   bool

	mu          sync.Mutex
	connections map[net.Conn]struct{}
	sequence    uint64
	wait        sync.WaitGroup
	revokeOnce  sync.Once
}

type toolCallRecord struct {
	Sequence       uint64 `json:"sequence"`
	Timestamp      string `json:"timestamp"`
	ContainerID    string `json:"container_id"`
	ContainerName  string `json:"container_name"`
	OperationClass string `json:"operation_class"`
	Command        string `json:"command,omitempty"`
	CommandHash    string `json:"command_hash"`
	Workdir        string `json:"workdir,omitempty"`
	ExitCode       int    `json:"exit_code"`
	DurationMS     int64  `json:"duration_ms"`
	Status         string `json:"status"`
	Error          string `json:"error,omitempty"`
	RunID          string `json:"run_id,omitempty"`
	TaskID         string `json:"task_id,omitempty"`
}

var _ runner.ToolBridge = (*Manager)(nil)

func New(options Options) (*Manager, error) {
	if options.OutputDir == "" {
		return nil, errors.New("Claude Code SSH bridge output directory is required")
	}
	if options.ResolveListen == nil {
		return nil, errors.New("Claude Code SSH bridge listen resolver is required")
	}
	if options.CleanupTimeout <= 0 {
		options.CleanupTimeout = defaultBridgeCleanup
	}
	if options.Logger == nil {
		options.Logger = logrus.StandardLogger()
	}
	return &Manager{outputDir: options.OutputDir, cleanupTimeout: options.CleanupTimeout, logger: options.Logger, omitRawLog: options.OmitRawLog, resolveListen: options.ResolveListen}, nil
}

func (manager *Manager) Start(ctx context.Context, generic runner.Sandbox) (core.ToolEndpoint, error) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.active != nil || manager.stopping {
		return core.ToolEndpoint{}, errors.New("Claude Code SSH bridge is already active")
	}
	sandbox, ok := generic.(bridgeSandbox)
	if !ok {
		return core.ToolEndpoint{}, errors.New("Claude Code SSH bridge requires the local Docker sandbox capability")
	}
	listen, err := manager.resolveListen(ctx)
	if err != nil {
		return core.ToolEndpoint{}, fmt.Errorf("resolve Claude Code SSH listen address: %w", err)
	}
	if net.ParseIP(listen.BindHost) == nil || net.ParseIP(listen.AdvertiseHost) == nil {
		return core.ToolEndpoint{}, errors.New("Claude Code SSH listen address must be IPv4")
	}
	session := &bridgeSession{sandbox: sandbox, connections: make(map[net.Conn]struct{})}
	session.artifactDir = filepath.Join(manager.outputDir, sandbox.TaskID(), "bridge")

	fail := func(primary error) (core.ToolEndpoint, error) {
		session.partialStart = true
		session.revoke()
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), manager.cleanupTimeout)
		defer cancel()
		waitErr := session.waitFor(cleanupCtx)
		cleanupErr := session.finalize()
		if waitErr != nil || cleanupErr != nil {
			manager.active = session
		}
		return core.ToolEndpoint{}, errors.Join(primary, waitErr, cleanupErr)
	}

	if err := ensurePrivateDirectory(session.artifactDir); err != nil {
		return fail(fmt.Errorf("create private Claude Code SSH artifact directory: %w", err))
	}
	hostSigner, clientPEM, authorized, err := generateSessionKeys()
	if err != nil {
		return fail(err)
	}
	session.identitySource = filepath.Join(session.artifactDir, "id_ed25519")
	if err := writeExclusivePrivate(session.identitySource, clientPEM); err != nil {
		return fail(fmt.Errorf("write Claude Code SSH identity: %w", err))
	}
	session.toolLogPath = filepath.Join(session.artifactDir, "tool-calls.jsonl")
	toolLog, err := openBoundedFile(session.toolLogPath, maxRawLogBytes)
	if err != nil {
		return fail(fmt.Errorf("create Claude Code SSH tool log: %w", err))
	}
	session.toolLog = toolLog
	if !manager.omitRawLog {
		session.rawLogPath = filepath.Join(session.artifactDir, "ssh_raw.log")
		rawLog, err := openBoundedFile(session.rawLogPath, maxRawLogBytes)
		if err != nil {
			return fail(fmt.Errorf("create Claude Code SSH raw log: %w", err))
		}
		session.rawLog = rawLog
	}
	listener, err := net.Listen("tcp4", net.JoinHostPort(listen.BindHost, "0"))
	if err != nil {
		return fail(fmt.Errorf("listen on task network gateway: %w", err))
	}
	session.listener = listener
	_, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		return fail(fmt.Errorf("parse Claude Code SSH listener address: %w", err))
	}

	serveCtx, cancel := context.WithCancel(context.Background())
	session.cancel = cancel
	session.configuration = newServerConfig(hostSigner, authorized)
	session.wait.Add(1)
	go session.serve(serveCtx, manager.logger)

	manager.active = session
	manager.stopErr = nil
	address := net.JoinHostPort(listen.AdvertiseHost, port)
	manager.logger.WithContext(ctx).WithFields(logrus.Fields{"address": address, "network": sandbox.NetworkName(), "container": sandbox.ContainerName()}).Info("Claude Code SSH bridge started")

	logPaths := []string{session.toolLogPath}
	if session.rawLogPath != "" {
		logPaths = append(logPaths, session.rawLogPath)
	}
	return core.ToolEndpoint{
		Protocol: "ssh", Address: address, Username: lockedUsername,
		IdentityFile: identityContainerPath, IdentitySourceFile: session.identitySource,
		LogPaths: logPaths,
	}, nil
}

func newServerConfig(hostSigner ssh.Signer, authorized ssh.PublicKey) *ssh.ServerConfig {
	configuration := &ssh.ServerConfig{
		MaxAuthTries: 3,
		PublicKeyCallback: func(metadata ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if metadata.User() != lockedUsername || !bytes.Equal(key.Marshal(), authorized.Marshal()) {
				return nil, errors.New("public key rejected")
			}
			return &ssh.Permissions{}, nil
		},
	}
	configuration.AddHostKey(hostSigner)
	return configuration
}

func (session *bridgeSession) serve(ctx context.Context, logger *logrus.Logger) {
	defer session.wait.Done()
	for {
		connection, err := session.listener.Accept()
		if err != nil {
			if ctx.Err() == nil && !errors.Is(err, net.ErrClosed) {
				logger.WithError(err).Warn("Claude Code SSH accept failed")
			}
			return
		}
		session.mu.Lock()
		session.connections[connection] = struct{}{}
		session.mu.Unlock()
		session.wait.Add(1)
		go session.handleConnection(ctx, connection)
	}
}

func (session *bridgeSession) handleConnection(ctx context.Context, connection net.Conn) {
	defer session.wait.Done()
	defer func() {
		_ = connection.Close()
		session.mu.Lock()
		delete(session.connections, connection)
		session.mu.Unlock()
	}()
	_ = connection.SetDeadline(time.Now().Add(lockedConnectTimeout))
	server, channels, requests, err := ssh.NewServerConn(connection, session.configuration)
	if err != nil {
		return
	}
	defer server.Close()
	// Claude Code's Bash tool opens a fresh channel per command over one
	// reused connection (see grammar.go) — the handshake deadline must not
	// survive into those channels.
	_ = connection.SetDeadline(time.Time{})
	go serveGlobalRequests(requests)
	for newChannel := range channels {
		if newChannel.ChannelType() != "session" {
			_ = newChannel.Reject(ssh.UnknownChannelType, "only a session channel is supported")
			continue
		}
		channel, channelRequests, err := newChannel.Accept()
		if err != nil {
			continue
		}
		session.wait.Add(1)
		go func() {
			defer session.wait.Done()
			session.handleSession(ctx, channel, channelRequests)
		}()
	}
}

func serveGlobalRequests(requests <-chan *ssh.Request) {
	for request := range requests {
		if request.WantReply {
			_ = request.Reply(false, nil)
		}
	}
}

// handleSession expects exactly one "exec" request per channel — matching
// hermesssh's handleSession — carrying one of the shapes grammar.go
// documents. Anything else (in particular an "env" request, which Claude
// Code's own SSH client library may still send per channel before exec, the
// same way OpenSSH does for Hermes) is refused without closing the channel.
func (session *bridgeSession) handleSession(ctx context.Context, channel ssh.Channel, requests <-chan *ssh.Request) {
	defer channel.Close()
	for request := range requests {
		if request.Type != "exec" {
			if request.WantReply {
				_ = request.Reply(false, nil)
			}
			continue
		}
		var payload struct{ Command string }
		if err := ssh.Unmarshal(request.Payload, &payload); err != nil {
			_ = session.reply(request, false)
			return
		}
		argv, err := splitCommand(payload.Command)
		if err != nil {
			_ = session.reply(request, false)
			session.logRequestFailure(payload.Command, kindUnknown, "rejected", err.Error())
			return
		}
		if len(argv) == 0 {
			_ = session.reply(request, false)
			session.logRequestFailure(payload.Command, kindUnknown, "rejected", "Claude Code exec command is empty")
			return
		}
		// "aries-fileop" is the companion MCP file-tools server's own wire
		// format (fileops.go), not Claude Code's bash-tool grammar — see
		// fileops.go's package doc comment for why it needs a separate path.
		if argv[0] == "aries-fileop" {
			op, err := decodeFileOp(argv[1:])
			if err != nil {
				_ = session.reply(request, false)
				session.logRequestFailure(payload.Command, kindFileOp, "rejected", err.Error())
				return
			}
			if err := session.reply(request, true); err != nil {
				return
			}
			session.executeFileOp(ctx, channel, op)
			return
		}
		if argv[0] != "bash" {
			_ = session.reply(request, false)
			session.logRequestFailure(payload.Command, kindUnknown, "rejected", "Claude Code exec must invoke bash")
			return
		}
		command, err := decodeRemoteCommand(argv[1:])
		if err != nil {
			_ = session.reply(request, false)
			session.logRequestFailure(payload.Command, kindUnknown, "rejected", err.Error())
			return
		}
		if command.kind == kindBootstrap {
			// The env probe and the once-per-session snapshot generator are
			// Claude Code's own scaffolding, not agent intent — run them
			// (needed for later calls to work) but do not journal them as
			// tool calls.
			if err := session.reply(request, true); err != nil {
				return
			}
			session.runBootstrap(ctx, channel, argv)
			return
		}
		if err := session.reply(request, true); err != nil {
			return
		}
		session.execute(ctx, channel, argv, command)
		return
	}
}

func (session *bridgeSession) reply(request *ssh.Request, accepted bool) error {
	return request.Reply(accepted, nil)
}

func (session *bridgeSession) runBootstrap(ctx context.Context, channel ssh.Channel, argv []string) {
	// argv[0] is "bash" (the program name); core.Command.Args excludes it —
	// see the identical fix note in execute() below.
	command := core.Command{Path: "/bin/bash", Args: argv[1:], Dir: session.sandbox.Workdir()}
	stdin := session.teeReader(channel, "stdin")
	stdout := session.teeWriter(channel, "stdout")
	stderr := session.teeWriter(channel.Stderr(), "stderr")
	result, _ := session.sandbox.ExecStream(ctx, command, stdin, stdout, stderr)
	exitCode := clampExitCode(result.ExitCode)
	_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{uint32(exitCode)}))
}

func (session *bridgeSession) execute(ctx context.Context, channel ssh.Channel, argv []string, command remoteCommand) {
	started := time.Now()
	// argv[0] is "bash" (the program name, from splitCommand); core.Command.Args
	// must exclude it — including it duplicated the program name as bash's
	// first positional argument, which bash then tried to run as a script file
	// named "bash", failing with "cannot execute binary file".
	dockerCommand := core.Command{Path: "/bin/bash", Args: argv[1:], Dir: session.sandbox.Workdir()}
	stdin := session.teeReader(channel, "stdin")
	stdout := session.teeWriter(channel, "stdout")
	stderr := session.teeWriter(channel.Stderr(), "stderr")
	result, err := session.sandbox.ExecStream(ctx, dockerCommand, stdin, stdout, stderr)
	exitCode := result.ExitCode
	status, message := "completed", ""
	if err != nil {
		exitCode = 255
		status, message = "failed", "sandbox execution failed"
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			status, message = "canceled", "session canceled"
		}
	}
	exitCode = clampExitCode(exitCode)
	session.writeRecord(toolCallRecord{
		OperationClass: kindAgent, Command: command.script, CommandHash: commandHash(command.script),
		Workdir: session.sandbox.Workdir(), ExitCode: exitCode,
		DurationMS: time.Since(started).Milliseconds(), Status: status, Error: message,
	})
	_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{uint32(exitCode)}))
}

func (session *bridgeSession) logRequestFailure(rawCommand, kind, status, message string) {
	session.writeRecord(toolCallRecord{
		OperationClass: kind, CommandHash: commandHash(rawCommand),
		ExitCode: -1, Status: status, Error: message,
	})
	// The structured record only carries a hash by design (matching
	// hermesssh's convention of keeping raw wire content out of
	// tool-calls.jsonl). For a REJECTED request there is no ExecStream call to
	// tee through teeReader/teeWriter, so without this the raw payload that
	// caused the rejection is unrecoverable.
	if session.rawLog != nil {
		_ = session.rawLog.write("rejected", []byte(fmt.Sprintf("kind=%s status=%s error=%q payload=%q\n", kind, status, message, rawCommand)))
	}
}

func clampExitCode(code int) int {
	if code < 0 || code > 255 {
		return 255
	}
	return code
}

func commandHash(command string) string {
	sum := sha256.Sum256([]byte(command))
	return hex.EncodeToString(sum[:])
}

func (session *bridgeSession) writeRecord(record toolCallRecord) {
	session.mu.Lock()
	session.sequence++
	record.Sequence = session.sequence
	session.mu.Unlock()
	record.Timestamp = time.Now().UTC().Format(time.RFC3339Nano)
	record.ContainerID = session.sandbox.ContainerID()
	record.ContainerName = session.sandbox.ContainerName()
	record.RunID = session.sandbox.RunID()
	record.TaskID = session.sandbox.TaskID()
	if session.toolLog == nil {
		return
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		return
	}
	_ = session.toolLog.write("record", append(encoded, '\n'))
}

func (session *bridgeSession) teeReader(source io.Reader, label string) io.Reader {
	if session.rawLog == nil {
		return source
	}
	return io.TeeReader(source, session.rawLog.taggedWriter(label))
}

func (session *bridgeSession) teeWriter(destination io.Writer, label string) io.Writer {
	if session.rawLog == nil {
		return destination
	}
	return io.MultiWriter(destination, session.rawLog.taggedWriter(label))
}

// splitCommand parses the SSH "exec" payload as a POSIX-ish argument vector.
// Claude Code's own SSH client sends the command as a single string, exactly
// like OpenSSH does; a proper shlex-style splitter belongs here once the
// exact quoting Claude Code's client uses is confirmed (see grammar.go's
// TODO) — this minimal splitter only handles the two shapes observed so far
// (`bash -c "<script>"` and `bash -c -l "<script>"`), splitting on the first
// two space-separated tokens and treating the remainder as one argument.
// splitCommand reverses the harness-side wrapper's own encoding exactly:
// every original argv element ($0..$n, i.e. "bash", then bash's own flags,
// then the script), joined by the ASCII Unit Separator (0x1F) — see
// pkg/harness/claudecode/config.go's bashWrapperScript for the client side
// and its doc comment for why plain-space joining (tried first) was
// discarded as ambiguous to re-split: a real per-call script can itself start with what looks like a
// flag-shaped token, so there is no way to tell "argument boundary" from
// "space inside the script text" once both have been flattened into one
// plain-space-joined string. The separator byte cannot appear in a shell
// flag and is not expected in real command text, so this split is exact.
func splitCommand(payload string) ([]string, error) {
	if payload == "" {
		return nil, errors.New("Claude Code exec command is empty")
	}
	return strings.Split(payload, "\x1f"), nil
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
	session := manager.active
	manager.stopping = true
	manager.stopDone = make(chan struct{})
	done := manager.stopDone
	manager.mu.Unlock()

	session.revoke()
	err := session.waitFor(ctx)
	if err == nil {
		err = session.finalize()
	}
	manager.mu.Lock()
	manager.stopErr = err
	manager.stopping = false
	if err == nil {
		manager.active = nil
	}
	close(done)
	manager.mu.Unlock()
	return err
}

func (session *bridgeSession) finalize() error {
	var errs []error
	if session.toolLog != nil {
		errs = append(errs, session.toolLog.Close())
	}
	if session.rawLog != nil {
		errs = append(errs, session.rawLog.Close())
	}
	errs = append(errs, removeIfPresent(session.identitySource))
	if session.partialStart {
		errs = append(errs, os.RemoveAll(session.artifactDir))
	}
	return errors.Join(errs...)
}

func (session *bridgeSession) revoke() {
	session.revokeOnce.Do(func() {
		if session.cancel != nil {
			session.cancel()
		}
		if session.listener != nil {
			_ = session.listener.Close()
		}
		session.mu.Lock()
		for connection := range session.connections {
			_ = connection.Close()
		}
		session.mu.Unlock()
	})
}

func (session *bridgeSession) waitFor(ctx context.Context) error {
	done := make(chan struct{})
	go func() { session.wait.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func generateSessionKeys() (ssh.Signer, []byte, ssh.PublicKey, error) {
	_, hostPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("generate SSH host key: %w", err)
	}
	hostSigner, err := ssh.NewSignerFromKey(hostPrivate)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("create SSH host signer: %w", err)
	}
	_, clientPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("generate SSH client key: %w", err)
	}
	clientSigner, err := ssh.NewSignerFromKey(clientPrivate)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("create SSH client signer: %w", err)
	}
	// Native OpenSSH format ("OPENSSH PRIVATE KEY"), not a generic PKCS8 PEM
	// block: empirically, this host's real `ssh`/`ssh-keygen` (OpenSSH 8.9p1)
	// rejects a PKCS8-wrapped ed25519 key with "is not a key file" even though
	// it is valid per `openssl pkey`. hermesssh's
	// identical PKCS8 approach may only work because Hermes's own container
	// bundles a newer/different SSH client than this host's; this bridge does
	// not rely on that assumption.
	block, err := ssh.MarshalPrivateKey(clientPrivate, "aries-claude-code")
	if err != nil {
		return nil, nil, nil, fmt.Errorf("marshal SSH client key: %w", err)
	}
	clientPEM := pem.EncodeToMemory(block)
	return hostSigner, clientPEM, clientSigner.PublicKey(), nil
}

func writeExclusivePrivate(path string, content []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(content); err != nil {
		file.Close()
		_ = os.Remove(path)
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		_ = os.Remove(path)
		return err
	}
	return file.Close()
}

func ensurePrivateDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	return os.Chmod(path, 0o700)
}

func removeIfPresent(path string) error {
	if path == "" {
		return nil
	}
	err := os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// lockedBoundedFile is a minimal stand-in for hermesssh's async auditWriter —
// this skeleton only needs a bounded, thread-safe append target.
type lockedBoundedFile struct {
	mu     sync.Mutex
	file   *os.File
	limit  int
	length int
}

func openBoundedFile(path string, limit int) (*lockedBoundedFile, error) {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	return &lockedBoundedFile{file: file, limit: limit}, nil
}

func (bounded *lockedBoundedFile) taggedWriter(label string) io.Writer {
	return taggedWriteFunc(func(content []byte) (int, error) {
		return len(content), bounded.write(label, content)
	})
}

func (bounded *lockedBoundedFile) write(label string, content []byte) error {
	bounded.mu.Lock()
	defer bounded.mu.Unlock()
	if bounded.length >= bounded.limit {
		return nil
	}
	remaining := bounded.limit - bounded.length
	if len(content) > remaining {
		content = content[:remaining]
	}
	prefix := []byte("[" + label + "] ")
	if _, err := bounded.file.Write(prefix); err != nil {
		return err
	}
	n, err := bounded.file.Write(content)
	bounded.length += len(prefix) + n
	return err
}

func (bounded *lockedBoundedFile) Close() error {
	bounded.mu.Lock()
	defer bounded.mu.Unlock()
	return bounded.file.Close()
}

type taggedWriteFunc func([]byte) (int, error)

func (fn taggedWriteFunc) Write(content []byte) (int, error) { return fn(content) }
