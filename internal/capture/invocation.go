package capture

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"go.kenn.io/agentsview/internal/pathutil"
)

type Provider string

const (
	ProviderClaude Provider = "claude"
	ProviderCodex  Provider = "codex"
)

type Streams struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

func invocationName(provider Provider) string {
	switch provider {
	case ProviderClaude:
		return "claude -p"
	case ProviderCodex:
		return "codex exec --json"
	default:
		return ""
	}
}

type preparedInvocation struct {
	argv      []string
	sessionID string
	direct    bool
}

func prepareInvocation(
	provider Provider, argv []string, suppliedSessionID string,
) (preparedInvocation, error) {
	if len(argv) == 0 {
		return preparedInvocation{}, errors.New("producer command is required")
	}
	name := strings.TrimSuffix(strings.ToLower(filepath.Base(argv[0])), ".exe")
	switch provider {
	case ProviderClaude:
		if hasNamedOption(
			argv[1:], "-c", "--continue", "-r", "--resume",
			"--fork-session", "--from-pr", "--teleport",
		) {
			return preparedInvocation{}, errors.New(
				"claude capture does not support continuation or resumed sessions",
			)
		}
		if name != "claude" {
			if suppliedSessionID == "" {
				return preparedInvocation{}, errors.New(
					"a claude wrapper requires --session-id with the UUID it passes to claude",
				)
			}
			if err := validateClaudeSessionID(suppliedSessionID); err != nil {
				return preparedInvocation{}, err
			}
			return preparedInvocation{argv: argv, sessionID: suppliedSessionID}, nil
		}
		if hasOption(argv[1:], "--no-session-persistence") {
			return preparedInvocation{}, errors.New(
				"claude capture requires transcript persistence; remove --no-session-persistence",
			)
		}
		if !hasOption(argv[1:], "-p", "--print") {
			return preparedInvocation{}, errors.New("claude capture requires a non-interactive -p or --print invocation")
		}
		fromArgs, err := optionValue(argv[1:], "--session-id")
		if err != nil {
			return preparedInvocation{}, err
		}
		if suppliedSessionID != "" && fromArgs != "" && suppliedSessionID != fromArgs {
			return preparedInvocation{}, errors.New("supplied claude session ID conflicts with child arguments")
		}
		sessionID := suppliedSessionID
		if sessionID == "" {
			sessionID = fromArgs
		}
		if sessionID == "" {
			sessionID, err = newUUID()
			if err != nil {
				return preparedInvocation{}, err
			}
			argv = append([]string{argv[0], "--session-id", sessionID}, argv[1:]...)
		}
		if err := validateClaudeSessionID(sessionID); err != nil {
			return preparedInvocation{}, err
		}
		return preparedInvocation{argv: argv, sessionID: sessionID, direct: true}, nil

	case ProviderCodex:
		if name != "codex" {
			return preparedInvocation{}, errors.New("codex capture requires a direct codex executable")
		}
		if len(argv) < 2 || argv[1] != "exec" || !hasOption(argv[2:], "--json") {
			return preparedInvocation{}, errors.New("codex capture requires codex exec --json")
		}
		if hasOption(argv[2:], "resume") {
			return preparedInvocation{}, errors.New(
				"codex capture does not support resumed sessions",
			)
		}
		if hasOption(argv[2:], "--ephemeral") {
			return preparedInvocation{}, errors.New(
				"codex capture requires transcript persistence; remove --ephemeral",
			)
		}
		if suppliedSessionID != "" {
			return preparedInvocation{}, errors.New("codex session identity comes from thread.started and cannot be supplied")
		}
		return preparedInvocation{argv: argv, direct: true}, nil
	default:
		return preparedInvocation{}, fmt.Errorf("unsupported capture provider %q", provider)
	}
}

func hasNamedOption(args []string, names ...string) bool {
	for _, arg := range args {
		if arg == "--" {
			return false
		}
		for _, name := range names {
			if arg == name || strings.HasPrefix(arg, name+"=") {
				return true
			}
		}
	}
	return false
}

func validateClaudeSessionID(value string) error {
	if !validUUID(value) {
		return errors.New("claude session ID must be a UUID")
	}
	if value != strings.ToLower(value) {
		return errors.New("claude session ID must use lowercase hexadecimal")
	}
	return nil
}

func hasOption(args []string, names ...string) bool {
	for _, arg := range args {
		if arg == "--" {
			return false
		}
		if slices.Contains(names, arg) {
			return true
		}
	}
	return false
}

func optionValue(args []string, name string) (string, error) {
	var value string
	for i := 0; i < len(args); i++ {
		if args[i] == "--" {
			break
		}
		candidate := ""
		switch {
		case args[i] == name:
			if i+1 >= len(args) {
				return "", fmt.Errorf("%s requires a value", name)
			}
			candidate = args[i+1]
			i++
		case strings.HasPrefix(args[i], name+"="):
			candidate = strings.TrimPrefix(args[i], name+"=")
		default:
			continue
		}
		if value != "" && value != candidate {
			return "", fmt.Errorf("conflicting %s values", name)
		}
		value = candidate
	}
	return value, nil
}

func newUUID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generating session ID: %w", err)
	}
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	encoded := make([]byte, 36)
	hex.Encode(encoded[0:8], raw[0:4])
	encoded[8] = '-'
	hex.Encode(encoded[9:13], raw[4:6])
	encoded[13] = '-'
	hex.Encode(encoded[14:18], raw[6:8])
	encoded[18] = '-'
	hex.Encode(encoded[19:23], raw[8:10])
	encoded[23] = '-'
	hex.Encode(encoded[24:36], raw[10:16])
	return string(encoded), nil
}

func validUUID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for i, c := range value {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !strings.ContainsRune("0123456789abcdefABCDEF", c) {
				return false
			}
		}
	}
	return true
}

type threadMarker struct {
	mu         sync.Mutex
	line       []byte
	max        int
	threadID   string
	conflict   bool
	overflow   bool
	persistErr error
	onID       func(string) error
	onInvalid  func(ReasonCode) error
}

func (m *threadMarker) Write(data []byte) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, b := range data {
		if b == '\n' {
			m.consumeLine()
			m.line = m.line[:0]
			continue
		}
		if len(m.line) < m.max {
			m.line = append(m.line, b)
		} else if !m.overflow {
			m.overflow = true
			m.persistInvalid(ReasonCorrelationUnavailable)
		}
	}
	return len(data), nil
}

func (m *threadMarker) finish() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.line) > 0 {
		m.consumeLine()
		m.line = nil
	}
}

func (m *threadMarker) consumeLine() {
	if m.overflow || m.conflict {
		return
	}
	var event struct {
		Type     string `json:"type"`
		ThreadID string `json:"thread_id"`
	}
	if json.Unmarshal(m.line, &event) != nil || event.Type != "thread.started" ||
		!validUUID(event.ThreadID) {
		return
	}
	id := strings.ToLower(event.ThreadID)
	if m.threadID != "" && m.threadID != id {
		m.conflict = true
		m.persistInvalid(ReasonCorrelationConflict)
		return
	}
	if m.threadID == "" {
		m.threadID = id
		if m.onID != nil {
			m.persistErr = errors.Join(m.persistErr, m.onID(id))
		}
	}
}

func (m *threadMarker) persistInvalid(reason ReasonCode) {
	if m.onInvalid != nil {
		m.persistErr = errors.Join(m.persistErr, m.onInvalid(reason))
	}
}

func (m *threadMarker) persistenceError() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.persistErr
}

func (m *threadMarker) result() (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.overflow {
		return "", errorWithReason(
			ReasonCorrelationUnavailable,
			"codex JSONL line exceeds correlation limit",
		)
	}
	if m.conflict {
		return "", errorWithReason(
			ReasonCorrelationConflict,
			"codex emitted conflicting thread identities",
		)
	}
	if m.threadID == "" {
		return "", errorWithReason(
			ReasonCorrelationUnavailable,
			"codex did not emit thread.started",
		)
	}
	return m.threadID, nil
}

func runChild(
	argv, env []string,
	dir string,
	streams Streams,
	marker *threadMarker,
) (ExecutionOutcome, int, bool, error) {
	started := time.Now().UTC()
	outcome := ExecutionOutcome{StartedAt: started}
	command, err := resolveChildCommand(argv[0])
	if err != nil {
		completed := time.Now().UTC()
		outcome.CompletedAt = &completed
		return outcome, 0, false, fmt.Errorf("starting producer: %w", err)
	}
	cmd := exec.Command(command, argv[1:]...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdin = streams.Stdin
	cmd.Stderr = streams.Stderr
	if marker == nil {
		cmd.Stdout = streams.Stdout
	} else {
		cmd.Stdout = io.MultiWriter(marker, streams.Stdout)
	}
	configureChildProcess(cmd)
	signalCh := registerChildSignals()
	if err := cmd.Start(); err != nil {
		unregisterChildSignals(signalCh)
		completed := time.Now().UTC()
		outcome.CompletedAt = &completed
		return outcome, 0, false, fmt.Errorf("starting producer: %w", err)
	}
	stopForwarding := forwardSignals(cmd.Process, signalCh)
	err = cmd.Wait()
	wrapperSignal, wrapperSignalCode := stopForwarding()
	if marker != nil {
		marker.finish()
	}
	completed := time.Now().UTC()
	outcome.CompletedAt = &completed
	if cmd.ProcessState == nil {
		if err == nil {
			err = errors.New("producer process state is unavailable")
		}
		return outcome, 0, true, fmt.Errorf("waiting for producer: %w", err)
	}
	if wrapperSignal != "" {
		outcome.Signal = wrapperSignal
		return outcome, wrapperSignalCode, true, waitError(err)
	}
	if signal, code := processSignal(cmd.ProcessState); signal != "" {
		outcome.Signal = signal
		return outcome, code, true, waitError(err)
	}
	code := cmd.ProcessState.ExitCode()
	outcome.ExitCode = &code
	return outcome, code, true, waitError(err)
}

func waitError(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := errors.AsType[*exec.ExitError](err); ok {
		return nil
	}
	return fmt.Errorf("waiting for producer: %w", err)
}

func defaultStreams(streams Streams) Streams {
	if streams.Stdin == nil {
		streams.Stdin = os.Stdin
	}
	if streams.Stdout == nil {
		streams.Stdout = os.Stdout
	}
	if streams.Stderr == nil {
		streams.Stderr = os.Stderr
	}
	return streams
}

func producerRoot(provider Provider, explicit string) (string, error) {
	if explicit != "" {
		return absoluteExpandedPath(explicit)
	}
	var root string
	switch provider {
	case ProviderClaude:
		if root = os.Getenv("CLAUDE_CONFIG_DIR"); root != "" {
			root = filepath.Join(root, "projects")
			break
		}
		root = "~/.claude/projects"
	case ProviderCodex:
		if root = os.Getenv("CODEX_HOME"); root != "" {
			root = filepath.Join(root, "sessions")
			break
		}
		root = "~/.codex/sessions"
	default:
		return "", fmt.Errorf("unsupported capture provider %q", provider)
	}
	return absoluteExpandedPath(root)
}

func absoluteExpandedPath(path string) (string, error) {
	expanded, err := pathutil.ExpandHome(path)
	if err != nil {
		return "", err
	}
	return filepath.Abs(expanded)
}

func encodeClaudeWorkDir(dir string) string {
	dir = filepath.Clean(dir)
	return strings.Map(func(r rune) rune {
		if r == '-' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' ||
			r >= '0' && r <= '9' {
			return r
		}
		return '-'
	}, dir)
}

// scanFirstLine is used only for exact candidate validation. It stops before
// allocating beyond the caller's JSONL line bound.
func scanFirstLine(ctx context.Context, path string, max int) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return scanFirstLineReader(ctx, f, max)
}

func scanFirstLineReader(ctx context.Context, input io.Reader, max int) ([]byte, error) {
	r := bufio.NewReaderSize(input, min(max, 64<<10))
	line := make([]byte, 0, min(max, 64<<10))
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		chunk, readErr := r.ReadSlice('\n')
		if len(chunk) > max-len(line) {
			return nil, errorWithReason(
				ReasonSourceBytesLimit, "first JSONL line exceeds limit")
		}
		line = append(line, chunk...)
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		switch {
		case readErr == nil, errors.Is(readErr, io.EOF):
			return line, nil
		case errors.Is(readErr, bufio.ErrBufferFull):
			continue
		default:
			return nil, readErr
		}
	}
}
