package rawderive

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawsync"
)

const ParserChildFlag = "--internal-raw-parser"
const parserOutputLimit = 32 << 20
const parserErrorLimit = 64 << 10
const parserProbeFile = "parser-probe"
const parserProbeContent = "hosted parser source probe"

var ErrSandboxUnavailable = errors.New("hosted parser isolation unavailable; Linux user, mount, network namespaces and seccomp TSYNC are required")
var errParserProtocol = errors.New("parser protocol invalid or output limit exceeded")
var errParserFailed = errors.New("isolated parser failed")

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
	Outcome         parser.ParseOutcome
	Tombstone       bool
	Errors          []wireSourceError
	Digests         [][]byte
	SessionPresence []bool
	MessagePresence [][]wireMessageState
}

func encodeParserOutcome(p ParsedManifest) ([]byte, error) {
	w := wireOutcome{Outcome: p.Outcome, Tombstone: p.Tombstone}
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
	return json.Marshal(w)
}
func decodeParserOutcome(data []byte) (ParsedManifest, error) {
	var w wireOutcome
	if len(data) > parserOutputLimit {
		return ParsedManifest{}, errParserProtocol
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&w); err != nil {
		return ParsedManifest{}, errParserProtocol
	}
	var extra any
	if dec.Decode(&extra) != io.EOF || len(w.Outcome.SourceErrors) != 0 {
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
	return ParsedManifest{Outcome: w.Outcome, Tombstone: w.Tombstone}, nil
}

func (p *SubprocessParser) Preflight(ctx context.Context) error {
	source, err := os.MkdirTemp("", "raw-probe-")
	if err != nil {
		return ErrSandboxUnavailable
	}
	defer os.RemoveAll(source)
	if os.WriteFile(filepath.Join(source, parserProbeFile), []byte(parserProbeContent), 0400) != nil {
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
	data, err := p.execute(ctx, tree.Root(), parserRequest{Identity: m.Identity, ManifestID: m.ManifestID, CanonicalJSON: m.CanonicalJSON})
	if err != nil {
		return ParsedManifest{}, err
	}
	return decodeParserOutcome(data)
}
func (p *SubprocessParser) execute(parent context.Context, source string, req parserRequest) ([]byte, error) {
	input, err := json.Marshal(req)
	if err != nil || len(input) > 2*rawsync.DefaultManifestLimits().MaxCanonicalBytes+4096 {
		return nil, errParserProtocol
	}
	jail, err := os.MkdirTemp("", "raw-jail-")
	if err != nil {
		return nil, ErrSandboxUnavailable
	}
	defer os.RemoveAll(jail)
	if os.Mkdir(filepath.Join(jail, "source"), 0700) != nil {
		return nil, ErrSandboxUnavailable
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, ErrSandboxUnavailable
	}
	ctx, cancel := context.WithTimeout(parent, p.WallTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, ParserChildFlag, source, jail)
	cmd.Env = []string{"GOMAXPROCS=2", "GOMEMLIMIT=384MiB", "TZ=UTC", "LANG=C"}
	if err = configureParserNamespace(cmd); err != nil {
		return nil, err
	}
	return runParserProcess(ctx, cancel, cmd, input, parserOutputLimit, parserErrorLimit)
}

type parserCapture struct {
	data     bytes.Buffer
	limit    int
	cancel   context.CancelFunc
	ready    chan struct{}
	once     sync.Once
	overflow bool
}

func (w *parserCapture) Write(p []byte) (int, error) {
	if len(p) > w.limit-w.data.Len() {
		w.overflow = true
		w.cancel()
		return 0, errParserProtocol
	}
	w.data.Write(p)
	if w.ready != nil && w.data.Len() >= 6 {
		w.once.Do(func() { close(w.ready) })
	}
	return len(p), nil
}

// runParserProcess retains Wait on every started process, including output
// overflow, protocol failure, cancellation and a provider ignoring context.
func runParserProcess(ctx context.Context, cancel context.CancelFunc, cmd *exec.Cmd, input []byte, outLimit, errLimit int) ([]byte, error) {
	ready := make(chan struct{})
	out := &parserCapture{limit: outLimit, cancel: cancel, ready: ready}
	diagnostic := &parserCapture{limit: errLimit, cancel: cancel}
	cmd.Stdout = out
	cmd.Stderr = diagnostic
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, errParserFailed
	}
	if err = cmd.Start(); err != nil {
		stdin.Close()
		return nil, ErrSandboxUnavailable
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var waitErr error
	select {
	case <-ready:
		// The bounded writer is still active. Validate its readiness prefix
		// after Wait; never read the growing buffer concurrently.
		_, err = stdin.Write(input)
		stdin.Close()
		if err != nil {
			cancel()
		}
		waitErr = <-done
	case waitErr = <-done:
		stdin.Close()
	case <-ctx.Done():
		cancel()
		stdin.Close()
		waitErr = <-done
	}
	if out.overflow || diagnostic.overflow {
		return nil, errParserProtocol
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if waitErr != nil {
		return nil, errParserFailed
	}
	if !bytes.HasPrefix(out.data.Bytes(), []byte("READY\n")) {
		return nil, errParserProtocol
	}
	return bytes.Clone(out.data.Bytes()[6:]), nil
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
	data, err := encodeParserOutcome(result)
	if err != nil || len(data) > parserOutputLimit {
		return true, 79
	}
	if _, err = os.Stdout.Write(data); err != nil {
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
