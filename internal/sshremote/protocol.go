// Package sshremote syncs a remote host over SSH by running agentsview on
// the remote for one short operation at a time.
package sshremote

import (
	"fmt"

	"go.kenn.io/agentsview/internal/remotesync"
)

// Op names one remote-sync serve operation.
type Op string

const (
	OpTargets  Op = "targets"
	OpManifest Op = "manifest"
	OpArchive  Op = "archive"
)

// ParseOp validates a command-line operation name.
func ParseOp(name string) (Op, error) {
	switch op := Op(name); op {
	case OpTargets, OpManifest, OpArchive:
		return op, nil
	default:
		return "", fmt.Errorf("unknown remote-sync operation %q", name)
	}
}

type serveRequest struct {
	ProtocolVersion int                        `json:"protocol_version"`
	Targets         *remotesync.TargetSet      `json:"targets,omitempty"`
	Archive         *remotesync.ArchiveRequest `json:"request,omitempty"`
}

// ErrorCode classifies a serve failure. The codes carry the meaning of the
// HTTP remote-sync status codes 426, 403, 501, and 500.
type ErrorCode string

const (
	CodeIncompatibleProtocol ErrorCode = "incompatible_protocol"
	CodeForbidden            ErrorCode = "forbidden"
	CodeUnsupported          ErrorCode = "unsupported"
	CodeInternal             ErrorCode = "internal"
)

// ServeError is a failure reported by the remote serve command. It travels as
// one JSON line on stderr.
type ServeError struct {
	Code    ErrorCode `json:"code"`
	Message string    `json:"message"`
}

func (e *ServeError) Error() string {
	return fmt.Sprintf("remote sync %s: %s", e.Code, e.Message)
}

// ServeExitCode is the exit status of a serve command that reported a
// ServeError on stderr.
const ServeExitCode = 2

// ProbeResult identifies a remote binary.
type ProbeResult struct {
	Version         string `json:"version"`
	ProtocolVersion int    `json:"protocol_version"`
}
