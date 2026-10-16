package sshremote

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// maxCapturedStderr bounds stderr kept for error messages. The tail is
// kept, because ssh and the serve command report their errors last.
const maxCapturedStderr = 32 << 10

// Runner runs fixed scripts on a remote host through the system ssh client.
type Runner struct {
	// Destination is the ssh destination argument.
	Destination string
	// SSHPath is the ssh executable. Empty means "ssh" on PATH.
	SSHPath string
}

// validateScript enforces the shape every remote script must have. The
// remote login shell may be csh or tcsh, which treat newlines and "!"
// specially even inside single quotes, or fish, which treats a backslash
// there specially.
func validateScript(script string) error {
	if strings.ContainsAny(script, "'\n!\\") {
		return fmt.Errorf("remote script must be one line without ', !, or a backslash: %q",
			script)
	}
	return nil
}

func (r Runner) command(ctx context.Context, script string) (*exec.Cmd, error) {
	if err := validateScript(script); err != nil {
		return nil, err
	}
	path := r.SSHPath
	if path == "" {
		path = "ssh"
	}
	// ServerAlive notices a dead peer within about a minute; TCP keepalive
	// alone takes hours while a sync holds the mirror lock.
	cmd := exec.CommandContext(ctx, path,
		"-T", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10", "-o", "Compression=no",
		"-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=4",
		"--", r.Destination, "sh -c '"+script+"'")
	// Untranslated system error text keeps offline classification reliable.
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	// Bound how long Wait blocks on pipes after ctx kills ssh.
	cmd.WaitDelay = 10 * time.Second
	return cmd, nil
}

// Output runs script, feeds it stdin, and returns stdout once ssh exits 0.
func (r Runner) Output(ctx context.Context, script string, stdin io.Reader) ([]byte, error) {
	body, err := r.Stream(ctx, script, stdin)
	if err != nil {
		return nil, err
	}
	out, readErr := io.ReadAll(body)
	closeErr := body.Close()
	if closeErr != nil {
		return nil, closeErr
	}
	if readErr != nil {
		return nil, fmt.Errorf("read ssh output: %w", readErr)
	}
	return out, nil
}

// Stream starts script and returns its stdout. Close waits for ssh and
// returns a *CommandError when it failed.
func (r Runner) Stream(ctx context.Context, script string, stdin io.Reader) (io.ReadCloser, error) {
	cmd, err := r.command(ctx, script)
	if err != nil {
		return nil, err
	}
	if stdin == nil {
		stdin = strings.NewReader("")
	}
	// Exec copies stdin in its own goroutine and closes ssh's stdin at EOF,
	// which a remote serve process needs before it can finish. A write error
	// there (EPIPE when ssh exits early) is reported by Wait only after ssh's
	// own status, which Close prefers.
	cmd.Stdin = stdin
	stderr := &boundedBuffer{limit: maxCapturedStderr}
	cmd.Stderr = stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("open ssh stdout: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start ssh: %w", err)
	}
	return &commandStream{ReadCloser: stdout, cmd: cmd, stderr: stderr}, nil
}

// errStreamAbandoned reports a Close before the remote output ended. The
// caller already stopped for its own reason, so this says nothing about the
// host's reachability.
var errStreamAbandoned = errors.New(
	"stopped ssh before the remote command finished sending its output")

type commandStream struct {
	io.ReadCloser
	cmd       *exec.Cmd
	stderr    *boundedBuffer
	eof       atomic.Bool
	closeOnce sync.Once
	closeErr  error
}

func (s *commandStream) Read(p []byte) (int, error) {
	n, err := s.ReadCloser.Read(p)
	if errors.Is(err, io.EOF) {
		s.eof.Store(true)
	}
	return n, err
}

// Close waits for ssh. Before EOF the remote may be blocked writing into a
// pipe nobody reads, so Close stops ssh first instead of waiting on it.
func (s *commandStream) Close() error {
	s.closeOnce.Do(func() {
		if s.eof.Load() {
			s.closeErr = s.wait()
			return
		}
		// Closing our end fails the remote's blocked write; killing ssh ends
		// the session even when the remote ignores that.
		_ = s.ReadCloser.Close()
		_ = s.cmd.Process.Kill()
		_ = s.cmd.Wait()
		s.closeErr = errStreamAbandoned
	})
	return s.closeErr
}

func (s *commandStream) wait() error {
	err := s.cmd.Wait()
	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok && exitErr.ExitCode() >= 0 {
		return newCommandError(exitErr.ExitCode(), s.stderr.String())
	}
	if err != nil {
		return fmt.Errorf("ssh: %w", err)
	}
	return nil
}

// boundedBuffer keeps the last limit bytes written to it.
type boundedBuffer struct {
	buf       []byte
	limit     int
	truncated bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.buf = append(b.buf, p...)
	if extra := len(b.buf) - b.limit; extra > 0 {
		b.buf = append(b.buf[:0], b.buf[extra:]...)
		b.truncated = true
	}
	return len(p), nil
}

// String returns the kept text. After truncation it starts at the first
// whole line, so no partial line is classified or shown.
func (b *boundedBuffer) String() string {
	if !b.truncated {
		return string(b.buf)
	}
	if _, rest, ok := bytes.Cut(b.buf, []byte("\n")); ok {
		return string(rest)
	}
	return ""
}
