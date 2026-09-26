package sandlock

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
	sandlocksdk "github.com/multikernel/sandlock/go"
)

// Exec runs one argv through Sandlock and captures stdout and stderr.
// A nonzero exit is a result, not an error. Timeout and cancellation are errors.
func (s *Sandbox) Exec(ctx context.Context, command core.Command) (core.CommandResult, error) {
	started := time.Now()
	if len(command.Stdin) > maxExecBytes {
		return core.CommandResult{ExitCode: -1, Duration: time.Since(started)}, fmt.Errorf("sandlock exec stdin exceeds %d bytes", maxExecBytes)
	}
	var stdout, stderr bytes.Buffer
	var stdin io.Reader
	if len(command.Stdin) > 0 {
		stdin = bytes.NewReader(command.Stdin)
	}
	result, err := s.ExecStream(ctx, command, stdin,
		&limitedWriter{writer: &stdout, limit: maxExecBytes},
		&limitedWriter{writer: &stderr, limit: maxExecBytes},
	)
	result.Stdout = stdout.String()
	result.Stderr = stderr.String()
	return result, err
}

// ExecStream is the bridge-facing streaming form of Exec.
func (s *Sandbox) ExecStream(ctx context.Context, command core.Command, stdin io.Reader, stdout, stderr io.Writer) (core.CommandResult, error) {
	started := time.Now()
	failure := func() core.CommandResult {
		return core.CommandResult{ExitCode: -1, Duration: time.Since(started)}
	}
	if err := validateCommand(command); err != nil {
		return failure(), err
	}
	if command.Dir == "" {
		command.Dir = s.workdir
	}
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	execCtx := ctx
	cancel := func() {}
	if command.Timeout > 0 {
		execCtx, cancel = context.WithTimeout(ctx, command.Timeout)
	}
	defer cancel()

	stdio := sandlocksdk.Stdio{Stdin: sandlocksdk.StdioNull, Stdout: sandlocksdk.StdioPiped, Stderr: sandlocksdk.StdioPiped}
	if stdin != nil {
		stdio.Stdin = sandlocksdk.StdioPiped
	}
	policy := s.policy.sandbox()
	policy.Cwd = command.Dir
	policy.Env = commandEnvironment(s.policy.Env, command.Env)
	policy.Name = s.name
	proc, err := policy.Popen(stdio, append([]string{command.Path}, command.Args...)...)
	if err != nil {
		wrapped := fmt.Errorf("start sandlock process: %w", err)
		s.record(execRecord{
			Backend: "sandlock", Command: command.Path, Args: command.Args,
			Started: started, Ended: time.Now(), ExitCode: -1, Error: wrapped.Error(),
		})
		return failure(), wrapped
	}
	if err := s.track(proc); err != nil {
		_ = proc.Kill()
		_ = proc.Close()
		return failure(), err
	}
	defer s.untrack(proc)
	defer proc.Close()

	stdinDone := make(chan struct{})
	go func() {
		defer close(stdinDone)
		if proc.Stdin == nil {
			return
		}
		limited := io.LimitReader(stdin, maxExecBytes+1)
		written, _ := io.Copy(proc.Stdin, limited)
		_ = proc.Stdin.Close()
		if written > maxExecBytes {
			_ = proc.Kill()
		}
	}()
	copyErr := make(chan error, 2)
	go func() { _, err := io.Copy(stdout, proc.Stdout); copyErr <- err }()
	go func() { _, err := io.Copy(stderr, proc.Stderr); copyErr <- err }()

	waitDone := make(chan struct{})
	var result *sandlocksdk.Result
	var waitErr error
	go func() {
		<-stdinDone
		result, waitErr = proc.Wait()
		close(waitDone)
	}()

	timedOut := false
	canceled := false
	select {
	case <-execCtx.Done():
		timedOut = errors.Is(execCtx.Err(), context.DeadlineExceeded)
		canceled = errors.Is(execCtx.Err(), context.Canceled)
		if proc.Stdin != nil {
			_ = proc.Stdin.Close()
		}
		_ = proc.Kill()
		<-waitDone
	case <-waitDone:
	}
	for range 2 {
		<-copyErr
	}
	ended := time.Now()
	record := execRecord{
		Backend: "sandlock", Command: command.Path, Args: command.Args,
		Started: started, Ended: ended, TimedOut: timedOut, Canceled: canceled,
	}
	if execCtx.Err() != nil {
		record.ExitCode = -1
		record.Error = execCtx.Err().Error()
		s.record(record)
		return failure(), execCtx.Err()
	}
	if waitErr != nil {
		record.ExitCode = -1
		record.Error = waitErr.Error()
		s.record(record)
		return failure(), fmt.Errorf("wait sandlock process: %w", waitErr)
	}
	exitCode := -1
	if result != nil {
		exitCode = result.ExitCode
		if result.Reason == sandlocksdk.ReasonTimeout {
			record.TimedOut = true
			record.ExitCode = exitCode
			record.Error = "sandlock process timed out"
			s.record(record)
			return failure(), context.DeadlineExceeded
		}
	}
	record.ExitCode = exitCode
	s.record(record)
	return core.CommandResult{ExitCode: exitCode, Duration: ended.Sub(started)}, nil
}

type limitedWriter struct {
	writer io.Writer
	limit  int
	used   int
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	if w.used >= w.limit {
		return len(p), nil
	}
	remain := w.limit - w.used
	chunk := p
	if len(chunk) > remain {
		chunk = chunk[:remain]
	}
	n, err := w.writer.Write(chunk)
	w.used += n
	if err != nil {
		return n, err
	}
	return len(p), nil
}

func validateCommand(command core.Command) error {
	if !strings.HasPrefix(command.Path, "/") || strings.ContainsRune(command.Path, 0) {
		return errors.New("invalid sandlock command path")
	}
	if command.Dir != "" && (!strings.HasPrefix(command.Dir, "/") || strings.ContainsRune(command.Dir, 0)) {
		return errors.New("invalid sandlock command directory")
	}
	if command.Timeout < 0 {
		return errors.New("sandlock command timeout must be nonnegative")
	}
	for _, argument := range command.Args {
		if strings.ContainsRune(argument, 0) {
			return errors.New("sandlock command argument contains NUL")
		}
	}
	for key, value := range command.Env {
		if !validEnvName(key) || strings.ContainsRune(value, 0) {
			return fmt.Errorf("invalid sandlock command environment %q", key)
		}
	}
	return nil
}
