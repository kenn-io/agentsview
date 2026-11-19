package rawderive

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawsync"
)

const (
	ParserChildFlag    = "--internal-raw-parser"
	parserErrorLimit   = 64 << 10
	parserProbeFile    = "parser-probe"
	parserProbeContent = "hosted parser source probe"
)

var (
	ErrSandboxUnavailable = errors.New("hosted parser isolation unavailable; Linux user, mount, network namespaces and seccomp TSYNC are required")
	errParserProtocol     = errors.New("parser protocol invalid or output limit exceeded")
	errParserFailed       = errors.New("isolated parser failed")
)

// SubprocessParser never invokes a provider in the hosting process. Limits are
// fixed hard ceilings; WallTimeout is validated before activation.
type SubprocessParser struct{ WallTimeout time.Duration }

func NewSubprocessParser(wall time.Duration) (*SubprocessParser, error) {
	if wall <= 0 || wall > 5*time.Minute {
		return nil, rawsync.ErrInvalid
	}
	return &SubprocessParser{WallTimeout: wall}, nil
}

type parserRequest struct {
	Identity      rawsync.AuthIdentity
	ManifestID    string
	CanonicalJSON []byte
	Probe         bool
}
type wireSourceError struct {
	SourceKey, DisplayPath, SessionID, Code string
	Retryable                               bool
}
type wireMessageState struct {
	PresenceKnown bool
	TokenUsageNil bool
}
type wireOutcome struct {
	Outcome               parser.ParseOutcome
	Tombstone             bool
	ReplaceSessionContent bool
	Errors                []wireSourceError
	Digests               [][]byte
	SessionPresence       []bool
	MessagePresence       [][]wireMessageState
}

func encodeParserOutcome(dst io.Writer, p ParsedManifest) error {
	w := wireOutcome{Outcome: p.Outcome, Tombstone: p.Tombstone, ReplaceSessionContent: p.ReplaceSessionContent}
	w.Outcome.SourceErrors = nil
	for _, e := range p.Outcome.SourceErrors {
		w.Errors = append(w.Errors, wireSourceError{e.SourceKey, e.DisplayPath, e.SessionID, jobErrorCode(e.Err), e.Retryable})
	}
	for _, r := range p.Outcome.Results {
		w.SessionPresence = append(w.SessionPresence, r.Result.Session.AggregateTokenPresenceKnown())
		messagePresence := make([]wireMessageState, len(r.Result.Messages))
		for i, m := range r.Result.Messages {
			messagePresence[i] = wireMessageState{PresenceKnown: m.TokenPresenceKnown(), TokenUsageNil: m.TokenUsage == nil}
		}
		w.MessagePresence = append(w.MessagePresence, messagePresence)
		for _, m := range r.Result.Messages {
			for _, c := range m.ToolCalls {
				for _, e := range c.ResultEvents {
					w.Digests = append(w.Digests, e.RawContentDigest)
				}
			}
		}
	}
	// Keep absent provider slices distinct from present empty collections.
	return json.MarshalWrite(dst, w, json.FormatNilSliceAsNull(true))
}

func decodeParserOutcome(src io.Reader) (ParsedManifest, error) {
	var w wireOutcome
	limited := &io.LimitedReader{R: src, N: parserOutputLimit + 1}
	err := json.UnmarshalRead(limited, &w, json.RejectUnknownMembers(true))
	if limited.N == 0 {
		return ParsedManifest{}, errors.Join(errParserProtocol, ErrSizeLimit)
	}
	if err != nil {
		return ParsedManifest{}, errParserProtocol
	}
	if len(w.Outcome.SourceErrors) != 0 {
		return ParsedManifest{}, errParserProtocol
	}
	for _, e := range w.Errors {
		var err error
		switch e.Code {
		case "":
		case "canceled":
			err = context.Canceled
		case "deadline_exceeded":
			err = context.DeadlineExceeded
		case "size_limit":
			err = ErrSizeLimit
		case "invalid":
			err = rawsync.ErrInvalid
		case "internal", "object_not_found", "missing_object", "conflict", "lease_lost":
			err = errParserFailed
		default:
			return ParsedManifest{}, errParserProtocol
		}
		w.Outcome.SourceErrors = append(w.Outcome.SourceErrors, parser.SourceError{SourceKey: e.SourceKey, DisplayPath: e.DisplayPath, SessionID: e.SessionID, Retryable: e.Retryable, Err: err})
	}
	if len(w.SessionPresence) != len(w.Outcome.Results) || len(w.MessagePresence) != len(w.Outcome.Results) {
		return ParsedManifest{}, errParserProtocol
	}
	n := 0
	for i := range w.Outcome.Results {
		result := &w.Outcome.Results[i].Result
		if len(w.MessagePresence[i]) != len(result.Messages) {
			return ParsedManifest{}, errParserProtocol
		}
		result.Session.RestoreAggregateTokenPresenceKnown(w.SessionPresence[i])
		for j := range result.Messages {
			result.Messages[j].RestoreTokenPresenceKnown(w.MessagePresence[i][j].PresenceKnown)
			if w.MessagePresence[i][j].TokenUsageNil {
				result.Messages[j].TokenUsage = nil
			}
		}
		for j := range w.Outcome.Results[i].Result.Messages {
			for k := range w.Outcome.Results[i].Result.Messages[j].ToolCalls {
				for l := range w.Outcome.Results[i].Result.Messages[j].ToolCalls[k].ResultEvents {
					if n >= len(w.Digests) {
						return ParsedManifest{}, errParserProtocol
					}
					w.Outcome.Results[i].Result.Messages[j].ToolCalls[k].ResultEvents[l].RawContentDigest = w.Digests[n]
					n++
				}
			}
		}
	}
	if n != len(w.Digests) {
		return ParsedManifest{}, errParserProtocol
	}
	return ParsedManifest{Outcome: w.Outcome, Tombstone: w.Tombstone, ReplaceSessionContent: w.ReplaceSessionContent}, nil
}

func (p *SubprocessParser) Preflight(ctx context.Context) error {
	source, err := os.MkdirTemp("", "raw-probe-")
	if err != nil {
		return ErrSandboxUnavailable
	}
	defer os.RemoveAll(source)
	if os.WriteFile(filepath.Join(source, parserProbeFile), []byte(parserProbeContent), 0o400) != nil {
		return ErrSandboxUnavailable
	}
	_, err = p.execute(ctx, source, parserRequest{Probe: true})
	if err != nil {
		return ErrSandboxUnavailable
	}
	return nil
}

func (p *SubprocessParser) Parse(ctx context.Context, m rawsync.CanonicalManifest, tree *Materialization) (ParsedManifest, error) {
	if err := rawsync.ValidateCanonicalManifest(m); err != nil {
		return ParsedManifest{}, err
	}
	if m.Manifest.Kind == rawsync.ManifestTombstone {
		return ParsedManifest{Tombstone: true}, nil
	}
	if tree == nil || tree.Root() == "" {
		return ParsedManifest{}, rawsync.ErrInvalid
	}
	return p.execute(ctx, tree.Root(), parserRequest{Identity: m.Identity, ManifestID: m.ManifestID, CanonicalJSON: m.CanonicalJSON})
}

func (p *SubprocessParser) execute(parent context.Context, source string, req parserRequest) (ParsedManifest, error) {
	input, err := json.Marshal(req)
	if err != nil || len(input) > 2*rawsync.DefaultManifestLimits().MaxCanonicalBytes+4096 {
		return ParsedManifest{}, errParserProtocol
	}
	jail, err := os.MkdirTemp("", "raw-jail-")
	if err != nil {
		return ParsedManifest{}, ErrSandboxUnavailable
	}
	defer os.RemoveAll(jail)
	if os.Mkdir(filepath.Join(jail, "source"), 0o700) != nil {
		return ParsedManifest{}, ErrSandboxUnavailable
	}
	exe, err := os.Executable()
	if err != nil {
		return ParsedManifest{}, ErrSandboxUnavailable
	}
	ctx, cancel := context.WithTimeout(parent, p.WallTimeout)
	defer cancel()
	// Protocol rejection stops the child without replacing the attempt's
	// caller cancellation or wall deadline with an internal cancellation.
	childCtx, stopChild := context.WithCancel(ctx)
	defer stopChild()
	cmd := exec.CommandContext(childCtx, exe, ParserChildFlag, source, jail)
	cmd.Env = []string{"GOMAXPROCS=2", "GOMEMLIMIT=" + parserGoMemoryLimit, "TZ=UTC", "LANG=C"}
	if err = configureParserNamespace(cmd); err != nil {
		return ParsedManifest{}, err
	}
	var result ParsedManifest
	err = runParserProcess(ctx, stopChild, cmd, input, parserOutputLimit, parserErrorLimit, func(src io.Reader) error {
		if req.Probe {
			var extra [1]byte
			if _, readErr := io.ReadFull(src, extra[:]); !errors.Is(readErr, io.EOF) {
				return errParserProtocol
			}
			return nil
		}
		var decodeErr error
		result, decodeErr = decodeParserOutcome(src)
		return decodeErr
	})
	if err != nil {
		return ParsedManifest{}, err
	}
	return result, nil
}

type parserCapture struct {
	data     bytes.Buffer
	limit    int
	cancel   context.CancelFunc
	overflow bool
}

func (w *parserCapture) Write(p []byte) (int, error) {
	if len(p) > w.limit-w.data.Len() {
		w.overflow = true
		w.cancel()
		return 0, errParserProtocol
	}
	w.data.Write(p)
	return len(p), nil
}

type parserOutputWriter struct {
	dst       io.Writer
	remaining int64
}

func (w *parserOutputWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.remaining {
		return 0, ErrSizeLimit
	}
	n, err := w.dst.Write(p)
	w.remaining -= int64(n)
	if err == nil && n != len(p) {
		return n, io.ErrShortWrite
	}
	return n, err
}

// runParserProcess retains Wait on every started process, including output
// overflow, protocol failure, cancellation and a provider ignoring context.
// Decode runs while the child writes; no complete stdout buffer is retained.
// Wait starts only after stdout has been consumed, as required by StdoutPipe.
// ctx bounds the attempt; stopChild cancels only the command's child context.
func runParserProcess(ctx context.Context, stopChild context.CancelFunc, cmd *exec.Cmd, input []byte, outLimit, errLimit int, decode func(io.Reader) error) error {
	ready := make(chan struct{})
	diagnostic := &parserCapture{limit: errLimit, cancel: stopChild}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return errParserFailed
	}
	cmd.Stderr = diagnostic
	stdin, err := cmd.StdinPipe()
	if err != nil {
		stdout.Close()
		return errParserFailed
	}
	if err = cmd.Start(); err != nil {
		stdin.Close()
		stdout.Close()
		return ErrSandboxUnavailable
	}

	done := make(chan error, 1)
	go func() {
		var prefix [6]byte
		_, readErr := io.ReadFull(stdout, prefix[:])
		if readErr != nil || string(prefix[:]) != "READY\n" {
			readErr = errParserProtocol
		} else {
			close(ready)
			limited := &io.LimitedReader{R: stdout, N: int64(outLimit) + 1}
			readErr = decode(limited)
			if readErr == nil {
				_, readErr = io.Copy(io.Discard, limited)
			}
			if limited.N == 0 {
				readErr = errors.Join(errParserProtocol, ErrSizeLimit)
			}
		}
		if readErr != nil {
			stopChild()
		}
		done <- readErr
	}()
	var outputErr, writeErr error
	select {
	case <-ready:
		_, writeErr = stdin.Write(input)
		stdin.Close()
		if writeErr != nil {
			stopChild()
		}
		outputErr = <-done
	case outputErr = <-done:
		stdin.Close()
	case <-ctx.Done():
		stdin.Close()
		outputErr = <-done
	}
	waitErr := cmd.Wait()
	if err := ctx.Err(); err != nil {
		return err
	}
	if diagnostic.overflow {
		return errParserProtocol
	}
	if errors.Is(outputErr, ErrSizeLimit) || cmd.ProcessState.ExitCode() == parserSizeLimitExitCode {
		return errors.Join(errParserProtocol, ErrSizeLimit)
	}
	if outputErr != nil {
		return errParserProtocol
	}
	if writeErr != nil {
		return errParserFailed
	}
	if waitErr != nil {
		if testing.Testing() {
			fmt.Fprintf(os.Stderr, "isolated parser child failed: %v\nstderr: %s\n", waitErr, diagnostic.data.Bytes())
		}
		return errParserFailed
	}
	return nil
}

// RunParserChild must be dispatched before ordinary CLI/config/database setup.
// Only a successful bootstrap may read the untrusted bounded request.
func RunParserChild(args []string) (bool, int) {
	if len(args) == 0 || args[0] != ParserChildFlag {
		return false, 0
	}
	if len(args) != 3 || !parserFDBootstrapCompleted() || isolateParser(args[1], args[2]) != nil {
		return true, 78
	}
	if _, err := os.Stdout.WriteString("READY\n"); err != nil {
		return true, 79
	}
	input, err := io.ReadAll(io.LimitReader(os.Stdin, int64(2*rawsync.DefaultManifestLimits().MaxCanonicalBytes+4097)))
	if err != nil || len(input) > 2*rawsync.DefaultManifestLimits().MaxCanonicalBytes+4096 {
		return true, 79
	}
	var req parserRequest
	if json.Unmarshal(input, &req) != nil {
		return true, 79
	}
	if req.Probe {
		if verifyParserProbe("/source") != nil {
			return true, 78
		}
		return true, 0
	}
	m, err := rawsync.ParseCanonicalManifest(req.Identity, req.ManifestID, req.CanonicalJSON, rawsync.DefaultManifestLimits())
	if err != nil {
		return true, 79
	}
	tree := &Materialization{root: "/source", entries: map[string]string{}}
	for _, e := range m.Manifest.Entries {
		tree.entries[e.Path] = filepath.Join("/source", filepath.FromSlash(e.Path))
	}
	provider, err := NewProviderParser(parser.ProviderFactories(), "hosted")
	if err != nil {
		return true, 79
	}
	result, err := provider.Parse(context.Background(), m, tree)
	if err != nil {
		return true, 79
	}
	output := &parserOutputWriter{dst: os.Stdout, remaining: parserOutputLimit}
	if err = encodeParserOutcome(output, result); err != nil {
		if errors.Is(err, ErrSizeLimit) {
			return true, parserSizeLimitExitCode
		}
		return true, 79
	}
	return true, 0
}

func verifyParserProbe(root string) error {
	data, err := os.ReadFile(filepath.Join(root, parserProbeFile))
	if err != nil || string(data) != parserProbeContent {
		return ErrSandboxUnavailable
	}
	return nil
}
