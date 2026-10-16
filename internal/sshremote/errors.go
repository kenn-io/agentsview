package sshremote

import (
	"encoding/json/v2"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode"

	"go.kenn.io/agentsview/internal/stringutil"
)

// sshConnectionFailed is OpenSSH's exit status for any error of its own.
const sshConnectionFailed = 255

// offlinePatterns are the OpenSSH lines that mean the network could not reach
// the host or its jump host. They match the network errors
// remotesync.IsHostUnavailable accepts for HTTP. Matching needs LC_ALL=C.
var offlinePatterns = []*regexp.Regexp{
	regexp.MustCompile(`^ssh: Could not resolve hostname .+: .+$`),
	regexp.MustCompile(`^ssh: connect to host .+ port [0-9]+: ` +
		`(Connection refused|Connection timed out|Operation timed out|` +
		`No route to host|Network is unreachable)$`),
	regexp.MustCompile(`^Connection timed out during banner exchange$`),
}

// CommandError reports a failed ssh invocation.
type CommandError struct {
	ExitCode int
	// Stderr is the captured, bounded stderr of ssh and the remote command.
	Stderr string
	// Offline means ssh could not reach the host.
	Offline bool
	// Remote is the serve failure the remote command reported, if any.
	Remote *ServeError
}

func newCommandError(exitCode int, stderr string) *CommandError {
	e := &CommandError{ExitCode: exitCode, Stderr: stderr}
	lines := strings.Split(strings.ReplaceAll(stderr, "\r", ""), "\n")
	switch exitCode {
	case sshConnectionFailed:
		e.Offline = matchesOffline(lines)
	case ServeExitCode:
		e.Remote = lastServeError(lines)
	}
	return e
}

func matchesOffline(lines []string) bool {
	for _, line := range lines {
		for _, pattern := range offlinePatterns {
			if pattern.MatchString(line) {
				return true
			}
		}
	}
	return false
}

func lastServeError(lines []string) *ServeError {
	line := lastNonEmptyLine(lines)
	var serveErr ServeError
	if json.Unmarshal([]byte(line), &serveErr) == nil && serveErr.Code != "" {
		return &serveErr
	}
	return nil
}

// lastNonEmptyLine picks the line nearest the failure: ssh prints its own
// error after any banner or start-up text the remote sent.
func lastNonEmptyLine(lines []string) string {
	for _, line := range slices.Backward(lines) {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return ""
}

// teardownPatterns are the generic lines ssh prints after the real error
// when a session or a ProxyJump leg closes. They name no cause.
var teardownPatterns = []*regexp.Regexp{
	regexp.MustCompile(`^Connection closed by .+ port [0-9]+$`),
	regexp.MustCompile(`^stdio forwarding failed$`),
	regexp.MustCompile(`^kex_exchange_identification: Connection closed by remote host$`),
	regexp.MustCompile(`^Connection to .+ closed( by remote host)?\.$`),
}

// failureLine picks the line that names the cause: the last non-empty line
// that is not a teardown line, or the last line when every line is one.
func failureLine(lines []string) string {
	for _, line := range slices.Backward(lines) {
		line = strings.TrimSpace(line)
		if line != "" && !slices.ContainsFunc(teardownPatterns, func(p *regexp.Regexp) bool {
			return p.MatchString(line)
		}) {
			return line
		}
	}
	return lastNonEmptyLine(lines)
}

// ansiEscape matches CSI and OSC terminal escape sequences.
var ansiEscape = regexp.MustCompile(`\x1b(\[[0-?]*[ -/]*[@-~]|\][^\x07\x1b]*(\x07|\x1b\\)?)`)

// summaryText makes remote-controlled text safe to show in a summary: it
// drops terminal escape sequences, control and other non-printable
// characters, and invalid UTF-8, then bounds the length.
func summaryText(s string) string {
	s = ansiEscape.ReplaceAllString(s, "")
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '\t':
			return ' '
		case !unicode.IsPrint(r):
			return -1
		default:
			return r
		}
	}, strings.ToValidUTF8(s, ""))
	return stringutil.SafeTruncate(strings.TrimSpace(s), summaryDetailBytes)
}

func (e *CommandError) Error() string {
	if e.Remote != nil {
		return e.Remote.Error()
	}
	return fmt.Sprintf("ssh exited %d: %s", e.ExitCode, strings.TrimSpace(e.Stderr))
}

// HostUnavailable lets remotesync skip an unreachable host.
func (e *CommandError) HostUnavailable() bool { return e.Offline }

// RemoteRefused lets remotesync retry a refused delta as a full archive.
// Only a refusal qualifies: an internal failure partway through a delta
// would most likely fail the full archive too, after a much longer
// download.
func (e *CommandError) RemoteRefused() bool {
	return e.Remote != nil && e.Remote.Code == CodeForbidden
}

// ProvisionError reports that agentsview could not be placed on a remote
// that ssh already reached. It never means the host is offline, even when the
// cause is a network error talking to the release server.
type ProvisionError struct {
	Err error
}

func (e *ProvisionError) Error() string {
	return "install agentsview on the remote: " + e.Err.Error()
}

func (e *ProvisionError) Unwrap() error { return e.Err }

// HostUnavailable keeps remotesync from skipping a host ssh reached.
func (e *ProvisionError) HostUnavailable() bool { return false }

// FailureSummary keeps the cause and remedy, such as installing agentsview
// on the remote, which the generic summary would drop.
func (e *ProvisionError) FailureSummary() string {
	return "SSH remote sync failed: could not install agentsview on the remote: " +
		summaryText(e.Err.Error())
}

const summaryDetailBytes = 300

// FailureSummary is the message the CLI and API show for this failure.
func (e *CommandError) FailureSummary() string {
	switch {
	case e.Offline:
		return "SSH remote sync failed: host is unreachable"
	case e.Remote != nil:
		return fmt.Sprintf("SSH remote sync failed: remote agentsview reported %s: %s",
			summaryText(string(e.Remote.Code)), summaryText(e.Remote.Message))
	default:
		return "SSH remote sync failed: " + summaryText(failureLine(
			strings.Split(strings.ReplaceAll(e.Stderr, "\r", ""), "\n")))
	}
}
