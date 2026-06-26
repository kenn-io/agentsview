package ssh

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
)

// sshConnectTimeoutSecs bounds the TCP connect phase of each ssh
// invocation so an unattended sync fails fast on an unreachable
// host instead of stalling on the OS default timeout.
const sshConnectTimeoutSecs = 10

// buildSSHArgs constructs args for the ssh command.
//
// Remote commands are always executed through a POSIX shell via
// "sh -c '<cmd>'" so behavior is independent of the remote user's
// login shell (e.g. fish).
//
// The invocation is non-interactive: it passes BatchMode=yes (never
// prompt for a password/passphrase -- remote sync requires key-based
// auth) and a bounded ConnectTimeout, so unattended runs fail fast
// with a clear error instead of stalling. These defaults follow
// sshOpts so an explicit override there wins (ssh uses the first
// value seen for each option).
//
// Returns ["ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=N",
// "--", "user@host", "sh -c '<cmd>'"] (or "host" when user is
// empty). Port adds "-p N" when > 0; extra sshOpts (e.g. "-i
// keyfile") are inserted before the defaults.
func buildSSHArgs(
	host, user string, port int, sshOpts []string, cmd string,
) ([]string, error) {
	if isOptionShapedTargetPart(host) {
		return nil, fmt.Errorf("ssh target host must not begin with '-'")
	}
	if isOptionShapedTargetPart(user) {
		return nil, fmt.Errorf("ssh target user must not begin with '-'")
	}
	target := host
	if user != "" {
		target = user + "@" + host
	}
	remoteCmd := "sh -c " + shellQuote(cmd)
	args := []string{"ssh"}
	if port > 0 {
		args = append(args, "-p", strconv.Itoa(port))
	}
	args = append(args, sshOpts...)
	args = append(args,
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout="+strconv.Itoa(sshConnectTimeoutSecs),
	)
	return append(args, "--", target, remoteCmd), nil
}

func isOptionShapedTargetPart(value string) bool {
	return strings.HasPrefix(strings.TrimSpace(value), "-")
}

// runSSH executes a command on the remote host and returns stdout.
// Returns an error containing stderr content on failure.
func runSSH(
	ctx context.Context,
	host, user string, port int, sshOpts []string,
	cmd string,
) ([]byte, error) {
	args, err := buildSSHArgs(host, user, port, sshOpts, cmd)
	if err != nil {
		return nil, err
	}
	c := exec.CommandContext(ctx, args[0], args[1:]...)
	var stderr bytes.Buffer
	c.Stderr = &stderr
	out, err := c.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			return nil, fmt.Errorf("ssh %s: %w", host, err)
		}
		return nil, fmt.Errorf(
			"ssh %s: %w: %s", host, err, msg,
		)
	}
	return out, nil
}

// runSSHStream executes a command on the remote host and returns a
// reader for stdout. Caller must call the returned cleanup func when
// done to wait for the process and release resources. Used for tar
// streams where buffering full output is impractical.
func runSSHStream(
	ctx context.Context,
	host, user string, port int, sshOpts []string,
	cmd string,
) (io.ReadCloser, func() error, error) {
	args, err := buildSSHArgs(host, user, port, sshOpts, cmd)
	if err != nil {
		return nil, nil, err
	}
	c := exec.CommandContext(ctx, args[0], args[1:]...)
	var stderr bytes.Buffer
	c.Stderr = &stderr

	stdout, err := c.StdoutPipe()
	if err != nil {
		return nil, nil, fmt.Errorf(
			"ssh %s: stdout pipe: %w", host, err,
		)
	}
	if err := c.Start(); err != nil {
		return nil, nil, fmt.Errorf(
			"ssh %s: start: %w", host, err,
		)
	}

	cleanup := func() error {
		if waitErr := c.Wait(); waitErr != nil {
			return &commandError{
				Host:   host,
				Stderr: strings.TrimSpace(stderr.String()),
				Err:    waitErr,
			}
		}
		return nil
	}
	return stdout, cleanup, nil
}

// commandError reports a streamed SSH command that exited non-zero. It
// carries the captured remote stderr so callers can tell benign tar
// warnings (e.g. "file changed as we read it") apart from fatal
// failures via remoteTarStderrBenign.
type commandError struct {
	Host   string
	Stderr string
	Err    error
}

func (e *commandError) Error() string {
	if e.Stderr == "" {
		return fmt.Sprintf("ssh %s: %v", e.Host, e.Err)
	}
	return fmt.Sprintf("ssh %s: %v: %s", e.Host, e.Err, e.Stderr)
}

func (e *commandError) Unwrap() error { return e.Err }
