package sshremote

import (
	"context"
	"encoding/json/v2"
	"io"
	"os"
	"os/signal"
	"syscall"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/remotesync"
)

// RunServeProcess is the body of `agentsview remote-sync serve`. It owns
// stdout and signals so an interrupted transfer still removes its temporary
// files, and returns the process exit code.
func RunServeProcess(op Op, stdin io.Reader, stderr io.Writer) int {
	// Without Notify, a write to a closed stdout kills the process with
	// SIGPIPE before deferred cleanup runs. With it, the write returns EPIPE.
	signal.Notify(make(chan os.Signal, 1), syscall.SIGPIPE)
	ctx, stop := signal.NotifyContext(context.Background(),
		syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	out, restoreStdout, err := pollableStdout()
	if err != nil {
		return reportServeError(stderr, &ServeError{Code: CodeInternal, Message: err.Error()})
	}
	// Serve has returned by the time this runs, so no write is in progress.
	// A restore failure is not reported: the response is already complete,
	// and stderr carries at most the one JSON error line the collector reads.
	defer func() { _ = restoreStdout() }()
	// A signal cancels ctx but cannot interrupt a write blocked on a full
	// pipe. Closing the pollable stdout releases that write with an error.
	go func() {
		<-ctx.Done()
		_ = out.Close()
	}()

	cfg, err := config.LoadReadOnly()
	if err != nil {
		return reportServeError(stderr,
			&ServeError{Code: CodeInternal, Message: "load config: " + err.Error()})
	}
	if err := Serve(ctx, op, cfg, stdin, out); err != nil {
		return reportServeError(stderr, classifyServeError(err))
	}
	return 0
}

func reportServeError(stderr io.Writer, serveErr *ServeError) int {
	_ = json.MarshalWrite(stderr, serveErr)
	_, _ = io.WriteString(stderr, "\n")
	return ServeExitCode
}

// WriteProbe prints the ProbeResult line for this binary.
func WriteProbe(w io.Writer, version string) error {
	if err := json.MarshalWrite(w, ProbeResult{
		Version: version, ProtocolVersion: remotesync.ProtocolVersion,
	}); err != nil {
		return err
	}
	_, err := io.WriteString(w, "\n")
	return err
}
