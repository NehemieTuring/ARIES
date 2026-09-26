package sandlock

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/runner"
	sandlocksdk "github.com/multikernel/sandlock/go"
	"github.com/sirupsen/logrus"
)

const (
	defaultCleanupTimeout = 30 * time.Second
	maxExecBytes          = 16 << 20
	bridgeNetworkName     = "host"
	bridgeGateway         = "127.0.0.1"
)

var (
	_ runner.ToolSandbox = (*Manager)(nil)
	_ runner.Sandbox     = (*Sandbox)(nil)
)

// Options are the host-local inputs for a Sandlock manager.
//
// NetAllow, when non-nil, replaces the task AllowNetwork mapping. An empty
// slice denies outbound traffic. FSDenied, MaxProcesses, MaxOpenFiles, and
// MaxCPUPercent add Sandlock controls that Docker's cgroup fields do not
// represent.
type Options struct {
	OutputDir      string
	CleanupTimeout time.Duration
	Logger         *logrus.Logger
	NetAllow       []string
	FSDenied       []string
	MaxProcesses   uint32
	MaxOpenFiles   uint32
	MaxCPUPercent  uint8
	// SeedWorkspace prepares the task tree inside the private root. The
	// default copies the image workdir with the Docker engine, then every
	// command runs in Sandlock. Tests replace it.
	SeedWorkspace func(context.Context, string, string, string) error
}

// Manager starts one Sandlock task root per Start.
type Manager struct {
	outputDir      string
	cleanupTimeout time.Duration
	logger         *logrus.Logger
	configured     Options
	newID          func() (string, error)
}

// Sandbox is one live task. Commands run as Sandlock processes on the host,
// inside a private chroot. There is no task container.
type Sandbox struct {
	owner       *Manager
	root        string
	artifactDir string
	outputDir   string
	name        string
	workdir     string
	runID       string
	taskID      string
	policy      Policy
	logger      *logrus.Logger

	mu        sync.Mutex
	processes []*sandlocksdk.Process
	stopped   bool
	stopping  bool
	stopDone  chan struct{}
	stopErr   error
	logMu     sync.Mutex
}

// New constructs a manager without starting a sandbox or contacting Sandlock.
func New(options Options) (*Manager, error) {
	if options.OutputDir == "" {
		return nil, errors.New("sandlock sandbox output directory is required")
	}
	outputDir, err := filepath.Abs(options.OutputDir)
	if err != nil {
		return nil, fmt.Errorf("resolve sandlock sandbox output directory: %w", err)
	}
	if err := os.MkdirAll(outputDir, 0o700); err != nil {
		return nil, fmt.Errorf("create sandlock sandbox output directory: %w", err)
	}
	if options.CleanupTimeout <= 0 {
		options.CleanupTimeout = defaultCleanupTimeout
	}
	if options.Logger == nil {
		options.Logger = logrus.StandardLogger()
	}
	return &Manager{
		outputDir:      outputDir,
		cleanupTimeout: options.CleanupTimeout,
		logger:         options.Logger,
		configured:     options,
		newID:          randomID,
	}, nil
}

// Close releases nothing held by the manager. Task cleanup belongs to Stop.
func (m *Manager) Close() error { return nil }

// Start prepares a private workspace and checks that the host can enforce
// Sandlock. It does not start a long-lived process; each command is its own
// sandboxed process.
func (m *Manager) Start(ctx context.Context, request core.SandboxRequest) (runner.Sandbox, error) {
	if err := validateIdentity("run", request.RunID); err != nil {
		return nil, err
	}
	if err := validateIdentity("task", request.TaskID); err != nil {
		return nil, err
	}
	if err := validateEnvironment(request.Environment); err != nil {
		return nil, err
	}
	if err := validateHostKernel(); err != nil {
		return nil, err
	}
	if abi := sandlocksdk.LandlockABIVersion(); abi >= 0 && abi < sandlocksdk.MinLandlockABI() {
		return nil, fmt.Errorf("%w: Landlock ABI %d is below the required %d", ErrUnsupportedKernel, abi, sandlocksdk.MinLandlockABI())
	}
	id, err := m.newID()
	if err != nil {
		return nil, fmt.Errorf("generate sandlock sandbox ID: %w", err)
	}
	artifactDir := filepath.Join(m.outputDir, request.TaskID, "sandlock")
	root := filepath.Join(artifactDir, "root")
	sandbox := &Sandbox{
		owner:       m,
		root:        root,
		artifactDir: artifactDir,
		outputDir:   m.outputDir,
		name:        "aries-sandlock-" + id,
		workdir:     request.Environment.Workdir,
		runID:       request.RunID,
		taskID:      request.TaskID,
		logger:      m.logger,
	}
	if err := os.MkdirAll(artifactDir, 0o700); err != nil {
		return nil, fmt.Errorf("create sandlock artifact directory: %w", err)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("create sandlock workspace: %w", err)
	}
	policy, err := taskPolicy(root, request.Environment.Workdir, request.Environment, m.configured)
	if err != nil {
		_ = os.RemoveAll(root)
		return nil, err
	}
	sandbox.policy = policy
	for _, containerPath := range []string{request.Environment.Workdir, "/tmp", "/logs", "/tests"} {
		host, err := hostPath(root, containerPath)
		if err != nil {
			_ = os.RemoveAll(root)
			return nil, err
		}
		if err := os.MkdirAll(host, 0o755); err != nil {
			_ = os.RemoveAll(root)
			return nil, fmt.Errorf("create sandlock path %s: %w", containerPath, err)
		}
	}
	seed := m.configured.SeedWorkspace
	if seed == nil {
		seed = seedFromImage
	}
	if err := seed(ctx, request.Environment.Image, root, request.Environment.Workdir); err != nil {
		_ = os.RemoveAll(root)
		return nil, fmt.Errorf("prepare sandlock workspace: %w", err)
	}
	m.logger.WithContext(ctx).WithFields(logrus.Fields{
		"sandbox": sandbox.name, "backend": "sandlock", "workdir": sandbox.workdir,
	}).Info("sandlock task sandbox started")
	return sandbox, nil
}

// Stop terminates leftover processes and removes the workspace. A second call
// returns the same result.
func (m *Manager) Stop(ctx context.Context, live runner.Sandbox) error {
	if live == nil {
		return errors.New("stop sandlock sandbox: sandbox is required")
	}
	sandbox, ok := live.(*Sandbox)
	if !ok || sandbox == nil {
		return fmt.Errorf("stop sandlock sandbox: unsupported sandbox type %T", live)
	}
	if sandbox.owner != m {
		return errors.New("stop sandlock sandbox: sandbox belongs to another manager")
	}
	return sandbox.stop(ctx)
}

func (s *Sandbox) stop(ctx context.Context) error {
	s.mu.Lock()
	if s.stopped {
		err := s.stopErr
		s.mu.Unlock()
		return err
	}
	if s.stopping {
		done := s.stopDone
		s.mu.Unlock()
		select {
		case <-done:
			s.mu.Lock()
			err := s.stopErr
			s.mu.Unlock()
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	s.stopping = true
	s.stopDone = make(chan struct{})
	processes := append([]*sandlocksdk.Process(nil), s.processes...)
	s.mu.Unlock()

	var killErr error
	for _, proc := range processes {
		if proc == nil {
			continue
		}
		if err := proc.Kill(); err != nil && !errors.Is(err, sandlocksdk.ErrNotRunning) {
			killErr = errors.Join(killErr, fmt.Errorf("kill sandlock process: %w", err))
		}
		_ = proc.Close()
	}
	removeErr := os.RemoveAll(s.root)
	if _, statErr := os.Stat(s.root); statErr == nil {
		removeErr = errors.Join(removeErr, errors.New("sandlock workspace still exists after removal"))
	} else if !errors.Is(statErr, os.ErrNotExist) {
		removeErr = errors.Join(removeErr, statErr)
	}
	err := errors.Join(killErr, removeErr)

	s.mu.Lock()
	s.stopped = true
	s.stopErr = err
	close(s.stopDone)
	s.mu.Unlock()
	if err == nil {
		s.logger.WithContext(ctx).WithField("sandbox", s.name).Info("sandlock task sandbox stopped")
	}
	return err
}

func (s *Sandbox) track(proc *sandlocksdk.Process) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped || s.stopping {
		return errors.New("sandlock sandbox is stopped")
	}
	s.processes = append(s.processes, proc)
	return nil
}

func (s *Sandbox) untrack(proc *sandlocksdk.Process) {
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := s.processes[:0]
	for _, existing := range s.processes {
		if existing != proc {
			kept = append(kept, existing)
		}
	}
	s.processes = kept
}

func (s *Sandbox) record(entry execRecord) {
	s.logMu.Lock()
	defer s.logMu.Unlock()
	file, err := os.OpenFile(filepath.Join(s.artifactDir, "exec.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		s.logger.WithError(err).Warn("sandlock exec record")
		return
	}
	defer file.Close()
	encoded, err := json.Marshal(entry)
	if err != nil {
		s.logger.WithError(err).Warn("sandlock exec record")
		return
	}
	_, _ = file.Write(append(encoded, '\n'))
}

// ContainerID is the bridge compatibility name. It is not a Docker container ID.
func (s *Sandbox) ContainerID() string { return s.name }

// ContainerName is the bridge compatibility name. No container is created.
func (s *Sandbox) ContainerName() string { return s.name }

// NetworkName is the Docker network mode the existing harness joins so it can
// reach the SSH bridge. Sandlock task processes do not join this network.
func (s *Sandbox) NetworkName() string { return bridgeNetworkName }

// NetworkGateway is the host address the SSH bridge binds. Task processes
// keep their own Sandlock network policy.
func (s *Sandbox) NetworkGateway(context.Context) (string, error) { return bridgeGateway, nil }

// RunID returns the experiment run identity.
func (s *Sandbox) RunID() string { return s.runID }

// TaskID returns the benchmark task identity.
func (s *Sandbox) TaskID() string { return s.taskID }

// Workdir returns the benchmark working directory inside the chroot.
func (s *Sandbox) Workdir() string { return s.workdir }

type execRecord struct {
	Backend  string    `json:"backend"`
	Command  string    `json:"command"`
	Args     []string  `json:"args,omitempty"`
	Started  time.Time `json:"started"`
	Ended    time.Time `json:"ended"`
	ExitCode int       `json:"exit_code"`
	Stdout   string    `json:"stdout,omitempty"`
	Stderr   string    `json:"stderr,omitempty"`
	Error    string    `json:"error,omitempty"`
	TimedOut bool      `json:"timed_out,omitempty"`
	Canceled bool      `json:"canceled,omitempty"`
}

func randomID() (string, error) {
	var value [8]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}
