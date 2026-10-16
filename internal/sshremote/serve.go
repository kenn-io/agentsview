package sshremote

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"

	"github.com/klauspost/compress/zstd"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/remotesync"
)

// serveZstdLevel is chosen from the benchmark in bench_test.go; the pull
// request records the measurements.
const serveZstdLevel = zstd.SpeedDefault

// maxServeRequestBytes bounds the request; delta file lists are the largest.
const maxServeRequestBytes = 64 << 20

// Serve runs one operation. It reads the JSON request from in and writes the
// zstd-compressed response to out. Failures are *ServeError values.
func Serve(ctx context.Context, op Op, cfg config.Config, in io.Reader, out io.Writer) error {
	req, err := readServeRequest(ctx, in)
	if err != nil {
		return err
	}
	if req.ProtocolVersion != remotesync.ProtocolVersion {
		return &ServeError{Code: CodeIncompatibleProtocol, Message: fmt.Sprintf(
			"collector speaks remote-sync protocol %d, this agentsview speaks %d",
			req.ProtocolVersion, remotesync.ProtocolVersion)}
	}
	if err := requireSourcePublishing(ctx, cfg); err != nil {
		return err
	}
	zw, err := remotesync.NewZstdWriter(out, serveZstdLevel)
	if err != nil {
		return &ServeError{Code: CodeInternal, Message: err.Error()}
	}
	if err := serveOp(ctx, op, cfg, req, zw); err != nil {
		return classifyServeError(err)
	}
	if err := zw.Close(); err != nil {
		return &ServeError{Code: CodeInternal, Message: "finish response: " + err.Error()}
	}
	return nil
}

// requireSourcePublishing refuses to publish sources from an archive-only
// database, as the HTTP remote-sync routes do. The check opens an existing
// database read-only and creates nothing when there is none.
func requireSourcePublishing(ctx context.Context, cfg config.Config) error {
	archiveOnly, err := db.ArchiveOnlyAt(ctx, cfg.DBPath)
	if err != nil {
		return &ServeError{Code: CodeInternal, Message: "read archive mode: " + err.Error()}
	}
	if archiveOnly {
		return &ServeError{Code: CodeForbidden, Message: db.ErrArchiveOnly.Error()}
	}
	return nil
}

// readServeRequest decodes the request and gives up when ctx ends. A
// collector that stalls mid-request keeps stdin open, and a signal must still
// end the process; the abandoned read ends when the process exits.
func readServeRequest(ctx context.Context, in io.Reader) (serveRequest, error) {
	type decoded struct {
		req serveRequest
		err error
	}
	done := make(chan decoded, 1)
	go func() {
		var req serveRequest
		err := json.UnmarshalRead(io.LimitReader(in, maxServeRequestBytes), &req)
		done <- decoded{req: req, err: err}
	}()
	select {
	case d := <-done:
		if d.err != nil {
			return serveRequest{}, &ServeError{
				Code: CodeInternal, Message: "decode request: " + d.err.Error(),
			}
		}
		return d.req, nil
	case <-ctx.Done():
		return serveRequest{}, &ServeError{
			Code:    CodeInternal,
			Message: "interrupted while reading the request: " + ctx.Err().Error(),
		}
	}
}

func serveOp(ctx context.Context, op Op, cfg config.Config, req serveRequest, out io.Writer) error {
	switch op {
	case OpTargets:
		targets, err := remotesync.ResolveTargets(cfg)
		if err != nil {
			return fmt.Errorf("resolve targets: %w", err)
		}
		return json.MarshalWrite(out, targets)
	case OpManifest:
		if req.Targets == nil {
			return errors.New("manifest request has no targets")
		}
		manifest, err := remotesync.ServeManifest(ctx, cfg, *req.Targets)
		if err != nil {
			return err
		}
		return json.MarshalWrite(out, manifest)
	case OpArchive:
		if req.Archive == nil {
			return errors.New("archive request has no body")
		}
		plan, err := remotesync.PlanArchive(cfg, *req.Archive)
		if err != nil {
			return err
		}
		return plan.Write(ctx, out)
	default:
		return fmt.Errorf("unknown remote-sync operation %q", op)
	}
}

func classifyServeError(err error) *ServeError {
	if serveErr, ok := errors.AsType[*ServeError](err); ok {
		return serveErr
	}
	code := CodeInternal
	switch {
	case errors.Is(err, remotesync.ErrTargetNotAllowed),
		errors.Is(err, remotesync.ErrFileNotAllowed):
		code = CodeForbidden
	case errors.Is(err, remotesync.ErrManifestUnsupported):
		code = CodeUnsupported
	}
	return &ServeError{Code: code, Message: err.Error()}
}
