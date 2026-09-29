package sync_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	gosync "sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/sync"
)

// claudeFullParseCounter wraps the Claude provider factory and counts
// whole-transcript Parse calls. The incremental path reads only appended
// bytes through ParseIncremental, so a full Parse after a small append is
// the observable "re-parsed the whole transcript" signal.
type claudeFullParseCounter struct {
	inner parser.ProviderFactory
	full  gosync.Int64
}

func (f *claudeFullParseCounter) Definition() parser.AgentDef {
	return f.inner.Definition()
}

func (f *claudeFullParseCounter) Capabilities() parser.Capabilities {
	return f.inner.Capabilities()
}

func (f *claudeFullParseCounter) NewProvider(
	cfg parser.ProviderConfig,
) parser.Provider {
	return &claudeFullParseCountProvider{
		Provider: f.inner.NewProvider(cfg),
		full:     &f.full,
	}
}

type claudeFullParseCountProvider struct {
	parser.Provider
	full *gosync.Int64
}

func (p *claudeFullParseCountProvider) Parse(
	ctx context.Context, req parser.ParseRequest,
) (parser.ParseOutcome, error) {
	p.full.Add(1)
	return p.Provider.Parse(ctx, req)
}

// claudeIncrementalErrorFactory fails one ParseIncremental call with a
// real error rather than one of the parser's whole-parse sentinels, so a
// test can tell "the run could not be re-parsed" apart from an expected
// fallback.
type claudeIncrementalErrorFactory struct {
	inner  parser.ProviderFactory
	calls  gosync.Int64
	failAt int64
}

func (f *claudeIncrementalErrorFactory) Definition() parser.AgentDef {
	return f.inner.Definition()
}

func (f *claudeIncrementalErrorFactory) Capabilities() parser.Capabilities {
	return f.inner.Capabilities()
}

func (f *claudeIncrementalErrorFactory) NewProvider(
	cfg parser.ProviderConfig,
) parser.Provider {
	return &claudeIncrementalErrorProvider{
		Provider: f.inner.NewProvider(cfg),
		calls:    &f.calls,
		failAt:   f.failAt,
	}
}

type claudeIncrementalErrorProvider struct {
	parser.Provider
	calls  *gosync.Int64
	failAt int64
}

func (p *claudeIncrementalErrorProvider) ParseIncremental(
	ctx context.Context, req parser.IncrementalRequest,
) (parser.IncrementalOutcome, parser.IncrementalStatus, error) {
	if p.calls.Add(1) == p.failAt {
		return parser.IncrementalOutcome{}, parser.IncrementalUnsupported,
			errors.New("synthetic transcript read failure")
	}
	return p.Provider.ParseIncremental(ctx, req)
}

// claudeSplitFactory returns the registered Claude provider factory.
func claudeSplitFactory(t *testing.T) parser.ProviderFactory {
	t.Helper()
	var factory parser.ProviderFactory
	for _, f := range parser.ProviderFactories() {
		if f.Definition().Type == parser.AgentClaude {
			factory = f
		}
	}
	require.NotNil(t, factory, "claude provider factory not registered")
	return factory
}

func newClaudeSplitTestEnv(
	t *testing.T, factory parser.ProviderFactory,
	archive config.ArchiveContent,
) (*testEnv, string) {
	t.Helper()
	dir := t.TempDir()
	env := &testEnv{claudeDir: dir, db: dbtest.OpenTestDB(t)}
	env.engine = sync.NewEngine(t.Context(), env.db, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentClaude: {dir},
		},
		Machine:           "local",
		ArchiveContent:    archive,
		ProviderFactories: []parser.ProviderFactory{factory},
	})
	t.Cleanup(env.engine.Close)
	return env, dir
}

func appendClaudeSplitLines(t *testing.T, path string, lines ...string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(t, err, "open for append")
	_, err = f.WriteString(strings.Join(lines, "\n") + "\n")
	require.NoError(t, err, "append")
	require.NoError(t, f.Close(), "close")
}

// claudeSplitShapes are same-message.id runs split across two syncs.
// Each entry writes the first splitAfter records, syncs, then appends the
// rest so the second sync starts inside the run. The leading attachment
// and the assistant records that parent it mirror a real CLI transcript,
// whose chain routes through attachment records and so parses linearly.
var claudeSplitShapes = map[string]struct {
	lines      []string
	splitAfter int
}{
	"cumulative_text": {
		lines: []string{
			`{"type":"attachment","timestamp":"2024-01-01T10:00:00Z","uuid":"at0","content":"context"}`,
			`{"type":"user","timestamp":"2024-01-01T10:00:00Z","uuid":"u1","message":{"content":"hello"},"cwd":"/tmp"}`,
			`{"type":"assistant","timestamp":"2024-01-01T10:00:01Z","uuid":"a1","parentUuid":"at0","message":{"id":"m","model":"claude-sonnet-4-20250514","content":[{"type":"text","text":"Hello"}],"usage":{"input_tokens":10,"output_tokens":1},"stop_reason":"tool_use"}}`,
			`{"type":"assistant","timestamp":"2024-01-01T10:00:02Z","uuid":"a2","parentUuid":"a1","message":{"id":"m","model":"claude-sonnet-4-20250514","content":[{"type":"text","text":"Hello world"}],"usage":{"input_tokens":10,"output_tokens":2},"stop_reason":"end_turn"}}`,
		},
		splitAfter: 3,
	},
	"cumulative_growing_tool": {
		lines: []string{
			`{"type":"attachment","timestamp":"2024-01-01T10:00:00Z","uuid":"at0","content":"context"}`,
			`{"type":"user","timestamp":"2024-01-01T10:00:00Z","uuid":"u1","message":{"content":"hello"},"cwd":"/tmp"}`,
			`{"type":"assistant","timestamp":"2024-01-01T10:00:01Z","uuid":"a1","parentUuid":"at0","message":{"id":"m","model":"claude-sonnet-4-20250514","content":[{"type":"text","text":"Work"},{"type":"tool_use","id":"t1","name":"Agent","input":{"description":"insp"}}],"usage":{"input_tokens":10,"output_tokens":1},"stop_reason":"tool_use"}}`,
			`{"type":"assistant","timestamp":"2024-01-01T10:00:02Z","uuid":"a2","parentUuid":"a1","message":{"id":"m","model":"claude-sonnet-4-20250514","content":[{"type":"text","text":"Working"},{"type":"tool_use","id":"t1","name":"Agent","input":{"description":"inspect schema","subagent_type":"Explore"}}],"usage":{"input_tokens":10,"output_tokens":2},"stop_reason":"end_turn"}}`,
		},
		splitAfter: 3,
	},
	"additive_distinct_text": {
		lines: []string{
			`{"type":"attachment","timestamp":"2024-01-01T10:00:00Z","uuid":"at0","content":"context"}`,
			`{"type":"user","timestamp":"2024-01-01T10:00:00Z","uuid":"u1","message":{"content":"hello"},"cwd":"/tmp"}`,
			`{"type":"assistant","timestamp":"2024-01-01T10:00:01Z","uuid":"a1","parentUuid":"at0","message":{"id":"m","model":"claude-sonnet-4-20250514","content":[{"type":"text","text":"First sentence."}],"usage":{"input_tokens":10,"output_tokens":1},"stop_reason":"end_turn"}}`,
			`{"type":"assistant","timestamp":"2024-01-01T10:00:02Z","uuid":"a2","parentUuid":"a1","message":{"id":"m","model":"claude-sonnet-4-20250514","content":[{"type":"text","text":"Second sentence."}],"usage":{"input_tokens":10,"output_tokens":2},"stop_reason":"end_turn"}}`,
			`{"type":"assistant","timestamp":"2024-01-01T10:00:03Z","uuid":"a3","parentUuid":"a2","message":{"id":"m","model":"claude-sonnet-4-20250514","content":[{"type":"text","text":"Third sentence."}],"usage":{"input_tokens":10,"output_tokens":3},"stop_reason":"end_turn"}}`,
		},
		splitAfter: 3,
	},
	"additive_prefix_collision": {
		lines: []string{
			`{"type":"attachment","timestamp":"2024-01-01T10:00:00Z","uuid":"at0","content":"context"}`,
			`{"type":"user","timestamp":"2024-01-01T10:00:00Z","uuid":"u1","message":{"content":"hello"},"cwd":"/tmp"}`,
			`{"type":"assistant","timestamp":"2024-01-01T10:00:01Z","uuid":"a1","parentUuid":"at0","message":{"id":"m","model":"claude-sonnet-4-20250514","content":[{"type":"text","text":"First."}],"usage":{"input_tokens":10,"output_tokens":1},"stop_reason":"end_turn"}}`,
			`{"type":"assistant","timestamp":"2024-01-01T10:00:02Z","uuid":"a2","parentUuid":"a1","message":{"id":"m","model":"claude-sonnet-4-20250514","content":[{"type":"text","text":"First. Continued."}],"usage":{"input_tokens":10,"output_tokens":2},"stop_reason":"end_turn"}}`,
		},
		splitAfter: 3,
	},
	"attachment_between_chunks": {
		lines: []string{
			`{"type":"attachment","timestamp":"2024-01-01T10:00:00Z","uuid":"at0","content":"context"}`,
			`{"type":"user","timestamp":"2024-01-01T10:00:00Z","uuid":"u1","message":{"content":"hello"},"cwd":"/tmp"}`,
			`{"type":"assistant","timestamp":"2024-01-01T10:00:01Z","uuid":"a1","parentUuid":"at0","message":{"id":"m","model":"claude-sonnet-4-20250514","content":[{"type":"text","text":"Work"}],"usage":{"input_tokens":10,"output_tokens":1},"stop_reason":"tool_use"}}`,
			`{"type":"attachment","timestamp":"2024-01-01T10:00:01Z","uuid":"at1","parentUuid":"a1","content":"queued"}`,
			`{"type":"assistant","timestamp":"2024-01-01T10:00:02Z","uuid":"a2","parentUuid":"at1","message":{"id":"m","model":"claude-sonnet-4-20250514","content":[{"type":"text","text":"Working"}],"usage":{"input_tokens":10,"output_tokens":2},"stop_reason":"end_turn"}}`,
		},
		splitAfter: 3,
	},
	// A completed earlier turn with a tool call sits below the replaced
	// range, so the ranged write must keep its rows and aggregates.
	"earlier_turn_kept": {
		lines: []string{
			`{"type":"attachment","timestamp":"2024-01-01T10:00:00Z","uuid":"at0","content":"context"}`,
			`{"type":"user","timestamp":"2024-01-01T10:00:00Z","uuid":"u1","message":{"content":"hello"},"cwd":"/tmp"}`,
			`{"type":"assistant","timestamp":"2024-01-01T10:00:01Z","uuid":"p1","parentUuid":"at0","message":{"id":"p","model":"claude-sonnet-4-20250514","content":[{"type":"tool_use","id":"t0","name":"Read","input":{"file_path":"a.go"}}],"usage":{"input_tokens":40,"output_tokens":7},"stop_reason":"tool_use"}}`,
			`{"type":"user","timestamp":"2024-01-01T10:00:02Z","uuid":"r0","parentUuid":"p1","message":{"content":[{"type":"tool_result","tool_use_id":"t0","content":"package a"}]},"cwd":"/tmp"}`,
			`{"type":"assistant","timestamp":"2024-01-01T10:00:03Z","uuid":"a1","parentUuid":"r0","message":{"id":"m","model":"claude-sonnet-4-20250514","content":[{"type":"text","text":"Reading"}],"usage":{"input_tokens":50,"output_tokens":1},"stop_reason":"tool_use"}}`,
			`{"type":"assistant","timestamp":"2024-01-01T10:00:04Z","uuid":"a2","parentUuid":"a1","message":{"id":"m","model":"claude-sonnet-4-20250514","content":[{"type":"text","text":"Reading done"}],"usage":{"input_tokens":50,"output_tokens":3},"stop_reason":"end_turn"}}`,
		},
		splitAfter: 5,
	},
	// The stored partial run carries a larger context than the finished
	// run, and the earlier turn a smaller one, so the session peak must be
	// recomputed rather than kept from the replaced row.
	"shrinking_context_usage": {
		lines: []string{
			`{"type":"attachment","timestamp":"2024-01-01T10:00:00Z","uuid":"at0","content":"context"}`,
			`{"type":"user","timestamp":"2024-01-01T10:00:00Z","uuid":"u1","message":{"content":"hello"},"cwd":"/tmp"}`,
			`{"type":"assistant","timestamp":"2024-01-01T10:00:01Z","uuid":"p1","parentUuid":"at0","message":{"id":"p","model":"claude-sonnet-4-20250514","content":[{"type":"text","text":"Earlier"}],"usage":{"input_tokens":15,"output_tokens":2},"stop_reason":"end_turn"}}`,
			`{"type":"user","timestamp":"2024-01-01T10:00:02Z","uuid":"u2","parentUuid":"p1","message":{"content":"again"},"cwd":"/tmp"}`,
			`{"type":"assistant","timestamp":"2024-01-01T10:00:03Z","uuid":"a1","parentUuid":"u2","message":{"id":"m","model":"claude-sonnet-4-20250514","content":[{"type":"text","text":"Later"}],"usage":{"input_tokens":30,"output_tokens":1},"stop_reason":"tool_use"}}`,
			`{"type":"assistant","timestamp":"2024-01-01T10:00:04Z","uuid":"a2","parentUuid":"a1","message":{"id":"m","model":"claude-sonnet-4-20250514","content":[{"type":"text","text":"Later still"}],"usage":{"input_tokens":20,"output_tokens":2},"stop_reason":"end_turn"}}`,
		},
		splitAfter: 5,
	},
	// The appended window finishes the run and then starts a new user
	// turn, so the session counts must grow by the new rows too.
	"user_turn_after_run": {
		lines: []string{
			`{"type":"attachment","timestamp":"2024-01-01T10:00:00Z","uuid":"at0","content":"context"}`,
			`{"type":"user","timestamp":"2024-01-01T10:00:00Z","uuid":"u1","message":{"content":"hello"},"cwd":"/tmp"}`,
			`{"type":"assistant","timestamp":"2024-01-01T10:00:01Z","uuid":"a1","parentUuid":"at0","message":{"id":"m","model":"claude-sonnet-4-20250514","content":[{"type":"text","text":"Hello"}],"usage":{"input_tokens":10,"output_tokens":1},"stop_reason":"tool_use"}}`,
			`{"type":"assistant","timestamp":"2024-01-01T10:00:02Z","uuid":"a2","parentUuid":"a1","message":{"id":"m","model":"claude-sonnet-4-20250514","content":[{"type":"text","text":"Hello world"}],"usage":{"input_tokens":10,"output_tokens":2},"stop_reason":"end_turn"}}`,
			`{"type":"user","timestamp":"2024-01-01T10:00:03Z","uuid":"u2","parentUuid":"a2","message":{"content":"next question"},"cwd":"/tmp"}`,
			`{"type":"assistant","timestamp":"2024-01-01T10:00:04Z","uuid":"a3","parentUuid":"u2","message":{"id":"n","model":"claude-sonnet-4-20250514","content":[{"type":"text","text":"Answer"}],"usage":{"input_tokens":20,"output_tokens":3},"stop_reason":"end_turn"}}`,
		},
		splitAfter: 3,
	},
	// Parallel tool calls: the tool_result lands between two records of
	// the same response, so the full parser keeps them apart and the
	// second sync is a plain append.
	"parallel_tool_results": claudeParallelToolShape,
}

// claudeParallelToolShape interleaves a tool_result between two
// assistant records that share message.id, and splits after it.
var claudeParallelToolShape = struct {
	lines      []string
	splitAfter int
}{
	lines: []string{
		`{"type":"attachment","timestamp":"2024-01-01T10:00:00Z","uuid":"at0","content":"context"}`,
		`{"type":"user","timestamp":"2024-01-01T10:00:00Z","uuid":"u1","message":{"content":"hello"},"cwd":"/tmp"}`,
		`{"type":"assistant","timestamp":"2024-01-01T10:00:01Z","uuid":"a1","parentUuid":"at0","message":{"id":"m","model":"claude-sonnet-4-20250514","content":[{"type":"tool_use","id":"t1","name":"Read","input":{"file_path":"a.go"}}],"usage":{"input_tokens":10,"output_tokens":1},"stop_reason":"tool_use"}}`,
		`{"type":"user","timestamp":"2024-01-01T10:00:02Z","uuid":"r1","parentUuid":"a1","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"package a"}]},"cwd":"/tmp"}`,
		`{"type":"assistant","timestamp":"2024-01-01T10:00:03Z","uuid":"a2","parentUuid":"r1","message":{"id":"m","model":"claude-sonnet-4-20250514","content":[{"type":"tool_use","id":"t2","name":"Read","input":{"file_path":"b.go"}}],"usage":{"input_tokens":10,"output_tokens":2},"stop_reason":"tool_use"}}`,
		`{"type":"user","timestamp":"2024-01-01T10:00:04Z","uuid":"r2","parentUuid":"a2","message":{"content":[{"type":"tool_result","tool_use_id":"t2","content":"package b"}]},"cwd":"/tmp"}`,
	},
	splitAfter: 4,
}

type splitMessageSnapshot struct {
	Ordinal          int
	Role             string
	Content          string
	SourceUUID       string
	ClaudeMessageID  string
	ContextTokens    int
	OutputTokens     int
	HasContextTokens bool
	HasOutputTokens  bool
	ToolUses         []string
}

type splitSessionSnapshot struct {
	MessageCount         int
	UserMessageCount     int
	TotalOutputTokens    int
	PeakContextTokens    int
	HasTotalOutputTokens bool
	HasPeakContextTokens bool
}

func snapshotSplitMessages(t *testing.T, database *db.DB, sessionID string) []splitMessageSnapshot {
	t.Helper()
	msgs := fetchMessages(t, database, sessionID)
	out := make([]splitMessageSnapshot, 0, len(msgs))
	for _, m := range msgs {
		var toolUses []string
		for _, tc := range m.ToolCalls {
			toolUses = append(toolUses, tc.ToolUseID+"|"+tc.ToolName)
		}
		out = append(out, splitMessageSnapshot{
			Ordinal:          m.Ordinal,
			Role:             m.Role,
			Content:          m.Content,
			SourceUUID:       m.SourceUUID,
			ClaudeMessageID:  m.ClaudeMessageID,
			ContextTokens:    m.ContextTokens,
			OutputTokens:     m.OutputTokens,
			HasContextTokens: m.HasContextTokens,
			HasOutputTokens:  m.HasOutputTokens,
			ToolUses:         toolUses,
		})
	}
	return out
}

func snapshotSplitSession(t *testing.T, database *db.DB, sessionID string) splitSessionSnapshot {
	t.Helper()
	sess, err := database.GetSession(t.Context(), sessionID)
	require.NoError(t, err, "GetSession(%q)", sessionID)
	require.NotNil(t, sess, "Session %q not found", sessionID)
	return splitSessionSnapshot{
		MessageCount:         sess.MessageCount,
		UserMessageCount:     sess.UserMessageCount,
		TotalOutputTokens:    sess.TotalOutputTokens,
		PeakContextTokens:    sess.PeakContextTokens,
		HasTotalOutputTokens: sess.HasTotalOutputTokens,
		HasPeakContextTokens: sess.HasPeakContextTokens,
	}
}

// TestClaudeIncrementalSplitMatchesFullParse is the equivalence guard for
// the split path: syncing a transcript in two pieces must store exactly
// the rows and session aggregates a single full parse of the finished
// file stores, for cumulative and additive run shapes. Usage-only
// archives drop most user rows, so they must match too.
func TestClaudeIncrementalSplitMatchesFullParse(t *testing.T) {
	archives := []config.ArchiveContent{
		config.ArchiveContentFull, config.ArchiveContentUsage,
	}
	for _, archive := range archives {
		for name, shape := range claudeSplitShapes {
			t.Run(string(archive)+"/"+name, func(t *testing.T) {
				testClaudeSplitMatchesFullParse(t, archive, shape.lines, shape.splitAfter)
			})
		}
	}
}

func testClaudeSplitMatchesFullParse(
	t *testing.T, archive config.ArchiveContent,
	lines []string, splitAfter int,
) {
	t.Helper()
	incrementalEnv, incrementalDir := newClaudeSplitTestEnv(t, claudeSplitFactory(t), archive)
	fullEnv, fullDir := newClaudeSplitTestEnv(t, claudeSplitFactory(t), archive)

	const proj = "proj"
	const file = "split.jsonl"
	initial := strings.Join(lines[:splitAfter], "\n") + "\n"
	rest := strings.Join(lines[splitAfter:], "\n") + "\n"

	// Incremental: store the partial run, then append the rest.
	path := filepath.Join(incrementalDir, proj, file)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(initial), 0o644))
	incrementalEnv.engine.SyncAll(t.Context(), nil)
	appendClaudeSplitLines(t, path, strings.TrimSuffix(rest, "\n"))
	incrementalEnv.engine.SyncPaths([]string{path})

	// Baseline: one full parse of the finished transcript.
	fullPath := filepath.Join(fullDir, proj, file)
	require.NoError(t, os.MkdirAll(filepath.Dir(fullPath), 0o755))
	require.NoError(t, os.WriteFile(fullPath, []byte(initial+rest), 0o644))
	fullEnv.engine.SyncAll(t.Context(), nil)

	sessionID := strings.TrimSuffix(file, ".jsonl")
	assert.Equal(t,
		snapshotSplitMessages(t, fullEnv.db, sessionID),
		snapshotSplitMessages(t, incrementalEnv.db, sessionID),
		"stored rows differ from a full parse",
	)
	assert.Equal(t,
		snapshotSplitSession(t, fullEnv.db, sessionID),
		snapshotSplitSession(t, incrementalEnv.db, sessionID),
		"session aggregates differ from a full parse",
	)
}

// TestClaudeIncrementalSplitReparsesOnlyTheOpenRun covers issue #1963:
// a sync that lands inside one assistant response must not re-parse the
// whole transcript to finish it.
func TestClaudeIncrementalSplitReparsesOnlyTheOpenRun(t *testing.T) {
	counter := &claudeFullParseCounter{inner: claudeSplitFactory(t)}
	env, dir := newClaudeSplitTestEnv(t, counter, config.ArchiveContentFull)

	shape := claudeSplitShapes["cumulative_text"]
	path := filepath.Join(dir, "proj", "split.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(
		path,
		[]byte(strings.Join(shape.lines[:shape.splitAfter], "\n")+"\n"),
		0o644,
	))

	env.engine.SyncAll(t.Context(), nil)
	require.Equal(t, int64(1), counter.full.Load(), "initial sync full-parses once")

	counter.full.Store(0)
	appendClaudeSplitLines(t, path, shape.lines[shape.splitAfter:]...)
	env.engine.SyncPaths([]string{path})

	assert.Zero(t, counter.full.Load(),
		"finishing one response must not re-parse the whole transcript")
	msgs := fetchMessages(t, env.db, "split")
	require.Len(t, msgs, 2)
	assert.Equal(t, "Hello world", msgs[1].Content)
}

// TestClaudeIncrementalSplitStaysOnRunInDAGSession covers a transcript
// whose uuid chain resolves, so the stored verdict is a DAG parse and the
// re-parse runs the fork check. The merged run keeps its last chunk's uuid
// and parent, which chain from the stored tail, so the check passes and
// the sync still re-parses only the open run.
func TestClaudeIncrementalSplitStaysOnRunInDAGSession(t *testing.T) {
	counter := &claudeFullParseCounter{inner: claudeSplitFactory(t)}
	env, dir := newClaudeSplitTestEnv(t, counter, config.ArchiveContentFull)

	lines := []string{
		`{"type":"user","timestamp":"2024-01-01T10:00:00Z","uuid":"u1","message":{"content":"hello"},"cwd":"/tmp"}`,
		`{"type":"assistant","timestamp":"2024-01-01T10:00:01Z","uuid":"a1","parentUuid":"u1","message":{"id":"m","model":"claude-sonnet-4-20250514","content":[{"type":"text","text":"Hello"}],"usage":{"input_tokens":10,"output_tokens":1},"stop_reason":"tool_use"}}`,
		`{"type":"assistant","timestamp":"2024-01-01T10:00:02Z","uuid":"a2","parentUuid":"a1","message":{"id":"m","model":"claude-sonnet-4-20250514","content":[{"type":"text","text":"Hello world"}],"usage":{"input_tokens":10,"output_tokens":2},"stop_reason":"end_turn"}}`,
	}
	path := filepath.Join(dir, "proj", "dag.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(
		path, []byte(strings.Join(lines[:2], "\n")+"\n"), 0o644,
	))
	env.engine.SyncAll(t.Context(), nil)
	var linear bool
	require.NoError(t, env.db.Reader().QueryRow(t.Context(),
		`SELECT claude_linear_parse FROM sessions WHERE id = ?`, "dag",
	).Scan(&linear))
	require.False(t, linear, "the fixture must be stored as a DAG parse")

	counter.full.Store(0)
	appendClaudeSplitLines(t, path, lines[2:]...)
	env.engine.SyncPaths([]string{path})

	assert.Zero(t, counter.full.Load(),
		"a DAG session must not re-parse the whole transcript")
	msgs := fetchMessages(t, env.db, "dag")
	require.Len(t, msgs, 2)
	assert.Equal(t, "Hello world", msgs[1].Content)
}

// TestClaudeIncrementalSameIDAfterToolResultAppends covers parallel tool
// calls: the appended record shares the stored tail's message.id, but a
// tool_result sits between them, so the full parser keeps them apart and
// the sync must append without re-parsing the whole transcript.
func TestClaudeIncrementalSameIDAfterToolResultAppends(t *testing.T) {
	counter := &claudeFullParseCounter{inner: claudeSplitFactory(t)}
	env, dir := newClaudeSplitTestEnv(t, counter, config.ArchiveContentFull)

	shape := claudeParallelToolShape
	path := filepath.Join(dir, "proj", "parallel.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(
		path,
		[]byte(strings.Join(shape.lines[:shape.splitAfter], "\n")+"\n"),
		0o644,
	))
	env.engine.SyncAll(t.Context(), nil)

	counter.full.Store(0)
	appendClaudeSplitLines(t, path, shape.lines[shape.splitAfter:]...)
	env.engine.SyncPaths([]string{path})

	assert.Zero(t, counter.full.Load(),
		"a same-id record after a tool_result is a plain append")
	msgs := fetchMessages(t, env.db, "parallel")
	require.Len(t, msgs, 3, "the two tool calls stay separate messages")
	assert.Equal(t, "m", msgs[1].ClaudeMessageID)
	assert.Equal(t, "m", msgs[2].ClaudeMessageID)
}

// TestClaudeIncrementalSplitLeavesEarlierRowsInPlace checks that finishing
// a split response rewrites only the rows from the run onward: earlier
// rows keep their row identity and their pins.
func TestClaudeIncrementalSplitLeavesEarlierRowsInPlace(t *testing.T) {
	counter := &claudeFullParseCounter{inner: claudeSplitFactory(t)}
	env, dir := newClaudeSplitTestEnv(t, counter, config.ArchiveContentFull)

	shape := claudeSplitShapes["earlier_turn_kept"]
	path := filepath.Join(dir, "proj", "kept.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(
		path,
		[]byte(strings.Join(shape.lines[:shape.splitAfter], "\n")+"\n"),
		0o644,
	))
	env.engine.SyncAll(t.Context(), nil)

	before := fetchMessages(t, env.db, "kept")
	require.Len(t, before, 3)
	_, err := env.db.PinMessage(t.Context(), "kept", before[1].ID, nil)
	require.NoError(t, err)

	counter.full.Store(0)
	appendClaudeSplitLines(t, path, shape.lines[shape.splitAfter:]...)
	env.engine.SyncPaths([]string{path})

	assert.Zero(t, counter.full.Load(),
		"finishing one response must not re-parse the whole transcript")
	after := fetchMessages(t, env.db, "kept")
	require.Len(t, after, 3)
	for i := range 2 {
		assert.Equal(t, before[i].ID, after[i].ID,
			"row at ordinal %d below the run was rewritten", before[i].Ordinal)
	}
	assert.Equal(t, "Reading done", after[2].Content)
	pins, err := env.db.ListPinnedMessages(t.Context(), "kept", "")
	require.NoError(t, err)
	require.Len(t, pins, 1)
	assert.Equal(t, before[1].ID, pins[0].MessageID)
}

// TestClaudeIncrementalToolUseRunIsNotReparsedEverySync checks that a
// completed tool_use run advances the stored cursor, so later syncs of
// the same file neither re-parse it nor leave it unarchived.
func TestClaudeIncrementalToolUseRunIsNotReparsedEverySync(t *testing.T) {
	counter := &claudeFullParseCounter{inner: claudeSplitFactory(t)}
	env, dir := newClaudeSplitTestEnv(t, counter, config.ArchiveContentFull)

	path := filepath.Join(dir, "proj", "tool-use.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(strings.Join([]string{
		`{"type":"user","timestamp":"2024-01-01T10:00:00Z","uuid":"u1","message":{"content":"hello"},"cwd":"/tmp"}`,
		`{"type":"assistant","timestamp":"2024-01-01T10:00:01Z","uuid":"a1","parentUuid":"u1","message":{"id":"m","model":"claude-sonnet-4-20250514","content":[{"type":"text","text":"Work"}],"usage":{"input_tokens":10,"output_tokens":1},"stop_reason":"tool_use"}}`,
	}, "\n")+"\n"), 0o644))

	env.engine.SyncAll(t.Context(), nil)

	counter.full.Store(0)
	appendClaudeSplitLines(t, path,
		`{"type":"assistant","timestamp":"2024-01-01T10:00:02Z","uuid":"a2","parentUuid":"a1","message":{"id":"m","model":"claude-sonnet-4-20250514","content":[{"type":"text","text":"Working"},{"type":"tool_use","id":"t1","name":"Read","input":{"file_path":"/tmp/x"}}],"usage":{"input_tokens":10,"output_tokens":2},"stop_reason":"tool_use"}}`,
	)
	env.engine.SyncPaths([]string{path})
	assert.Zero(t, counter.full.Load(), "finishing the run stays incremental")

	// The stored cursor must now cover the completed run, so an unchanged
	// re-sync neither re-detects the split nor re-reads the run.
	env.engine.SyncPaths([]string{path})
	assert.Zero(t, counter.full.Load(), "unchanged re-sync stays incremental")

	// The run is archived and a later tool result is applied to its call.
	appendClaudeSplitLines(t, path,
		`{"type":"user","timestamp":"2024-01-01T10:00:03Z","uuid":"r1","parentUuid":"a2","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}}`,
	)
	env.engine.SyncPaths([]string{path})
	assert.Zero(t, counter.full.Load(), "later append stays incremental")

	msgs := fetchMessages(t, env.db, "tool-use")
	require.Len(t, msgs, 2, "user plus the merged tool_use run")
	assert.Contains(t, msgs[1].Content, "Working")
	require.Len(t, msgs[1].ToolCalls, 1)
	assert.Equal(t, "ok", msgs[1].ToolCalls[0].ResultContent)
}

// TestClaudeIncrementalSplitPropagatesReParseFailure pins the difference
// between the parser's declared whole-parse fallbacks and a real read
// failure. The fallbacks mean "use the whole-transcript path"; a real
// failure surfaces and leaves the stored rows alone instead of being
// swallowed as a fallback.
func TestClaudeIncrementalSplitPropagatesReParseFailure(t *testing.T) {
	factory := &claudeIncrementalErrorFactory{
		inner: claudeSplitFactory(t),
		// Call 1 parses the appended window, call 2 re-parses the run.
		failAt: 2,
	}
	env, dir := newClaudeSplitTestEnv(t, factory, config.ArchiveContentFull)

	shape := claudeSplitShapes["cumulative_text"]
	path := filepath.Join(dir, "proj", "split.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(
		path,
		[]byte(strings.Join(shape.lines[:shape.splitAfter], "\n")+"\n"),
		0o644,
	))
	env.engine.SyncAll(t.Context(), nil)

	appendClaudeSplitLines(t, path, shape.lines[shape.splitAfter:]...)
	env.engine.SyncPaths([]string{path})
	assert.Equal(t, int64(2), factory.calls.Load(),
		"the window parse and the run re-parse both ran")

	msgs := fetchMessages(t, env.db, "split")
	require.Len(t, msgs, 2)
	assert.Equal(t, "Hello", msgs[1].Content,
		"a failed re-parse must not advance the stored run")
}
