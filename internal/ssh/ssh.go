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

// buildSSHArgs constructs args for the ssh command.
//
// Remote commands are always executed through a POSIX shell via
// "sh -c '<cmd>'" so behavior is independent of the remote user's
// login shell (e.g. fish).
//
// Returns ["ssh", "user@host", "--", "sh -c '<cmd>'"] or
// ["ssh", "host", "--", "sh -c '<cmd>'"] when user is empty.
// Port adds "-p N" when > 0. Extra opts are inserted before the
// target (e.g. "-i keyfile").
func buildSSHArgs(
	host, user string, port int, sshOpts []string, cmd string,
) []string {
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
	return append(args, target, "--", remoteCmd)
}

// runSSH executes a command on the remote host and returns stdout.
// Returns an error containing stderr content on failure.
func runSSH(
	ctx context.Context,
	host, user string, port int, sshOpts []string,
	cmd string,
) ([]byte, error) {
	args := buildSSHArgs(host, user, port, sshOpts, cmd)
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
	args := buildSSHArgs(host, user, port, sshOpts, cmd)
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
			msg := strings.TrimSpace(stderr.String())
			if msg == "" {
				return fmt.Errorf(
					"ssh %s: %w", host, waitErr,
				)
			}
			return fmt.Errorf(
				"ssh %s: %w: %s", host, waitErr, msg,
			)
		}
		return nil
	}
	return stdout, cleanup, nil
}
