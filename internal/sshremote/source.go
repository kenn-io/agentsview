package sshremote

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"

	"go.kenn.io/agentsview/internal/remotesync"
	syncpkg "go.kenn.io/agentsview/internal/sync"
)

// zstdMagic starts every zstd frame. A response that starts otherwise was
// corrupted by output from the remote shell.
var zstdMagic = []byte{0x28, 0xb5, 0x2f, 0xfd}

const shellOutputMessage = "the remote shell printed output before agentsview ran; " +
	"make shell start-up files quiet for non-interactive sessions, as scp and rsync also require"

// shellOutputError carries its own summary because the cause is on the
// remote host and only the user can fix it.
type shellOutputError struct{}

func (shellOutputError) Error() string { return shellOutputMessage }

// FailureSummary is the message the CLI and API show for this failure.
func (shellOutputError) FailureSummary() string {
	return "SSH remote sync failed: " + shellOutputMessage
}

var errShellOutput error = shellOutputError{}

// Source fetches remote sync data by running agentsview over SSH.
type Source struct {
	provisioner Provisioner

	mu            sync.Mutex
	ensured       bool
	reprovisioned bool
	binary        remoteBinary
	ensureErr     error
}

// NewSource returns a Source for one sync run.
func NewSource(p Provisioner) *Source { return &Source{provisioner: p} }

func (s *Source) ensure(ctx context.Context) (remoteBinary, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.ensured {
		s.binary, s.ensureErr = s.provisioner.Ensure(ctx)
		s.ensured = true
	}
	return s.binary, s.ensureErr
}

// ReportProgress implements remotesync.ProgressReporter: upload messages go
// through the sync's own progress callback, which RemoteSync serializes
// across hosts that prepare at once.
func (s *Source) ReportProgress(progress syncpkg.ProgressFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.provisioner.Progress = func(detail string) { progress(syncpkg.Progress{Detail: detail}) }
}

// forgetBinary makes the next step provision again, once per sync. Another
// collector's retention can remove a cached binary just after this one's
// probe selected it.
func (s *Source) forgetBinary() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reprovisioned {
		return false
	}
	s.reprovisioned = true
	s.ensured = false
	return true
}

// binaryMissingExit is the serve script's exit status when the selected
// binary no longer exists. Shells disagree on exec's own status for a
// missing program (126 on macOS /bin/sh, 127 elsewhere), and 126 also means
// a program that exists but cannot run, so the script tests for the binary
// itself. 69 is EX_UNAVAILABLE from sysexits.h; serve itself exits 0 or 2.
const binaryMissingExit = 69

func binaryMissing(err error) bool {
	cmdErr, ok := errors.AsType[*CommandError](err)
	return ok && cmdErr.Remote == nil && cmdErr.ExitCode == binaryMissingExit
}

func serveScript(bin remoteBinary, op Op) (string, error) {
	if _, err := ParseOp(string(op)); err != nil {
		return "", err
	}
	script := "if " + bin.present + "; then exec " + bin.shellPath +
		" remote-sync serve --op " + string(op) + "; fi; " +
		"echo \"agentsview is no longer installed on the remote\" >&2; " +
		"exit " + strconv.Itoa(binaryMissingExit)
	return script, validateScript(script)
}

func (s *Source) start(ctx context.Context, op Op, req serveRequest) (*zstdChecked, error) {
	bin, err := s.ensure(ctx)
	if err != nil {
		return nil, err
	}
	script, err := serveScript(bin, op)
	if err != nil {
		return nil, err
	}
	req.ProtocolVersion = remotesync.ProtocolVersion
	var body bytes.Buffer
	if err := json.MarshalWrite(&body, req); err != nil {
		return nil, fmt.Errorf("encode %s request: %w", op, err)
	}
	stream, err := s.provisioner.Runner.Stream(ctx, script, &body)
	if err != nil {
		return nil, err
	}
	return newZstdChecked(stream), nil
}

// call runs one operation and decodes its JSON response. Retention removes a
// binary within moments of its idle check, so a binary that vanished after
// the probe shows up here, at the first step, before any archive transfer.
func (s *Source) call(ctx context.Context, op Op, req serveRequest, out any) error {
	err := s.callOnce(ctx, op, req, out)
	if binaryMissing(err) && s.forgetBinary() {
		err = s.callOnce(ctx, op, req, out)
	}
	return err
}

func (s *Source) callOnce(ctx context.Context, op Op, req serveRequest, out any) error {
	stream, err := s.start(ctx, op, req)
	if err != nil {
		return err
	}
	decodeErr := decodeZstdJSON(stream, out)
	closeErr := stream.Close()
	// A decode failure stops reading early, so Close then only reports that
	// it stopped ssh; the decode failure is the cause.
	if closeErr != nil && (decodeErr == nil || !errors.Is(closeErr, errStreamAbandoned)) {
		return closeErr
	}
	if decodeErr != nil {
		return fmt.Errorf("decode remote %s response: %w", op, decodeErr)
	}
	return nil
}

// Targets implements remotesync.Source.
func (s *Source) Targets(ctx context.Context) (remotesync.TargetSet, error) {
	var targets remotesync.TargetSet
	err := s.call(ctx, OpTargets, serveRequest{}, &targets)
	return targets, err
}

// Manifest implements remotesync.Source.
func (s *Source) Manifest(
	ctx context.Context, targets remotesync.TargetSet,
) (remotesync.Manifest, bool, error) {
	var manifest remotesync.Manifest
	err := s.call(ctx, OpManifest, serveRequest{Targets: &targets}, &manifest)
	if cmdErr, ok := errors.AsType[*CommandError](err); ok &&
		cmdErr.Remote != nil && cmdErr.Remote.Code == CodeUnsupported {
		return remotesync.Manifest{}, false, nil
	}
	return manifest, err == nil, err
}

// Archive implements remotesync.Source.
func (s *Source) Archive(
	ctx context.Context, request remotesync.ArchiveRequest,
) (*remotesync.ArchiveStream, error) {
	stream, err := s.startArchive(ctx, request)
	if binaryMissing(err) && s.forgetBinary() {
		stream, err = s.startArchive(ctx, request)
	}
	if err != nil {
		return nil, err
	}
	return &remotesync.ArchiveStream{Body: stream, Encoding: remotesync.ArchiveEncodingZstd}, nil
}

// startArchive starts the archive and waits for its first byte. A remote
// whose binary vanished exits before writing anything, so its failure shows
// here, where Archive can still provision again, instead of mid-transfer.
func (s *Source) startArchive(
	ctx context.Context, request remotesync.ArchiveRequest,
) (*zstdChecked, error) {
	stream, err := s.start(ctx, OpArchive, serveRequest{Archive: &request})
	if err != nil {
		return nil, err
	}
	if _, peekErr := stream.r.Peek(1); peekErr != nil {
		// At EOF, Close reports the remote's exit; otherwise it only reports
		// that it stopped ssh, and the read error is the cause.
		closeErr := stream.Close()
		if closeErr != nil && !errors.Is(closeErr, errStreamAbandoned) {
			return nil, closeErr
		}
		return nil, fmt.Errorf("remote archive sent no data: %w", peekErr)
	}
	return stream, nil
}

// zstdChecked fails the first Read when the stream does not start with a
// zstd frame. It only peeks, so the decoder still sees every byte.
type zstdChecked struct {
	r       *bufio.Reader
	closer  io.Closer
	checked bool
	err     error
}

func newZstdChecked(rc io.ReadCloser) *zstdChecked {
	return &zstdChecked{r: bufio.NewReader(rc), closer: rc}
}

func (c *zstdChecked) Read(p []byte) (int, error) {
	if c.err != nil {
		return 0, c.err
	}
	if !c.checked {
		c.checked = true
		// A stream shorter than the magic passes through: Read returns its
		// bytes and error, and an empty stream lets Close report the remote's
		// failure.
		head, _ := c.r.Peek(len(zstdMagic))
		if len(head) == len(zstdMagic) && !bytes.Equal(head, zstdMagic) {
			c.err = errShellOutput
			return 0, c.err
		}
	}
	return c.r.Read(p)
}

func (c *zstdChecked) Close() error { return c.closer.Close() }

// decodeZstdJSON decodes one zstd-compressed JSON value and reads r to EOF,
// so the ssh stream behind it ends normally.
func decodeZstdJSON(r io.Reader, out any) error {
	zr, err := remotesync.NewZstdReader(r)
	if err != nil {
		return err
	}
	defer zr.Close()
	if err := json.UnmarshalRead(zr, out); err != nil {
		return err
	}
	if _, err := io.Copy(io.Discard, r); err != nil {
		return fmt.Errorf("read past response: %w", err)
	}
	return nil
}
