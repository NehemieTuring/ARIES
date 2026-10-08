package sandlock

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"syscall"
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
	policy.Name = s.name + "-" + strconv.FormatUint(s.seq.Add(1), 10)
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

	// Wait closes the Sandlock stdin fd. Keep a duplicate so that close does
	// not drop bytes, and do not wait for the caller's EOF before reaping.
	// The SSH bridge leaves the channel open until it sees an exit status, so
	// waiting here holds the sandbox name and every later popen fails.
	stdinCopy, err := duplicateStdin(proc.Stdin)
	if err != nil {
		_ = proc.Kill()
		_ = proc.Close()
		return failure(), err
	}
	go copyStdin(proc, stdinCopy, stdin)
	copyErr := make(chan error, 2)
	go func() { _, err := io.Copy(stdout, proc.Stdout); copyErr <- err }()
	go func() { _, err := io.Copy(stderr, proc.Stderr); copyErr <- err }()

	waitDone := make(chan struct{})
	var result *sandlocksdk.Result
	var waitErr error
	go func() {
		result, waitErr = proc.Wait()
		close(waitDone)
	}()

	timedOut := false
	canceled := false
	select {
	case <-execCtx.Done():
		timedOut = errors.Is(execCtx.Err(), context.DeadlineExceeded)
		canceled = errors.Is(execCtx.Err(), context.Canceled)
		_ = proc.Kill()
		<-waitDone
	case <-waitDone:
	}
	pending := 2
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
drainCopies:
	for pending > 0 {
		select {
		case <-copyErr:
			pending--
		case <-timer.C:
			_ = proc.Stdout.Close()
			_ = proc.Stderr.Close()
			break drainCopies
		}
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

func duplicateStdin(stdin *os.File) (*os.File, error) {
	if stdin == nil {
		return nil, nil
	}
	fd, err := syscall.Dup(int(stdin.Fd()))
	if err != nil {
		return nil, fmt.Errorf("duplicate sandlock stdin: %w", err)
	}
	return os.NewFile(uintptr(fd), "sandlock-stdin"), nil
}

func copyStdin(proc *sandlocksdk.Process, stdinCopy *os.File, stdin io.Reader) {
	if stdinCopy == nil {
		return
	}
	defer stdinCopy.Close()
	written, _ := io.Copy(stdinCopy, io.LimitReader(stdin, maxExecBytes+1))
	if written > maxExecBytes {
		_ = proc.Kill()
	}
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
