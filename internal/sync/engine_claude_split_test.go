package sync_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	gosync "sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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

func newClaudeSplitTestEnv(
	t *testing.T, counter *claudeFullParseCounter,
) (*testEnv, string) {
	t.Helper()
	var inner parser.ProviderFactory
	for _, f := range parser.ProviderFactories() {
		if f.Definition().Type == parser.AgentClaude {
			inner = f
		}
	}
	require.NotNil(t, inner, "claude provider factory")

	factory := parser.ProviderFactory(inner)
	if counter != nil {
		counter.inner = inner
		factory = counter
	}
	dir := t.TempDir()
	env := &testEnv{claudeDir: dir, db: dbtest.OpenTestDB(t)}
	env.engine = sync.NewEngine(t.Context(), env.db, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentClaude: {dir},
		},
		Machine:           "local",
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
// file stores, for cumulative and additive run shapes.
func TestClaudeIncrementalSplitMatchesFullParse(t *testing.T) {
	for name, shape := range claudeSplitShapes {
		t.Run(name, func(t *testing.T) {
			incrementalEnv, incrementalDir := newClaudeSplitTestEnv(t, nil)
			fullEnv, fullDir := newClaudeSplitTestEnv(t, nil)

			const proj = "proj"
			const file = "split.jsonl"
			initial := strings.Join(shape.lines[:shape.splitAfter], "\n") + "\n"
			rest := strings.Join(shape.lines[shape.splitAfter:], "\n") + "\n"

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
		})
	}
}

// TestClaudeIncrementalSplitReparsesOnlyTheOpenRun covers issue #1963:
// a sync that lands inside one assistant response must not re-parse the
// whole transcript to finish it.
func TestClaudeIncrementalSplitReparsesOnlyTheOpenRun(t *testing.T) {
	counter := &claudeFullParseCounter{}
	env, dir := newClaudeSplitTestEnv(t, counter)

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

// TestClaudeIncrementalSplitFallsBackWhenRunCannotBeLocated covers the
// narrowed fallback: when the stored tail shares the appended message id
// but the run cannot be reconstructed, the engine still re-parses the
// whole transcript rather than storing a duplicate.
func TestClaudeIncrementalSplitFallsBackWhenRunCannotBeLocated(t *testing.T) {
	counter := &claudeFullParseCounter{}
	env, dir := newClaudeSplitTestEnv(t, counter)

	path := filepath.Join(dir, "proj", "interrupted.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(strings.Join([]string{
		`{"type":"user","timestamp":"2024-01-01T10:00:00Z","uuid":"u1","message":{"content":"hello"},"cwd":"/tmp"}`,
		`{"type":"assistant","timestamp":"2024-01-01T10:00:01Z","uuid":"a1","parentUuid":"u1","message":{"id":"m","model":"claude-sonnet-4-20250514","content":[{"type":"text","text":"Hello"}],"usage":{"input_tokens":10,"output_tokens":1},"stop_reason":"end_turn"}}`,
		`{"type":"user","timestamp":"2024-01-01T10:00:02Z","uuid":"u2","parentUuid":"a1","message":{"content":"more"},"cwd":"/tmp"}`,
	}, "\n")+"\n"), 0o644))

	env.engine.SyncAll(t.Context(), nil)

	counter.full.Store(0)
	appendClaudeSplitLines(t, path,
		`{"type":"assistant","timestamp":"2024-01-01T10:00:03Z","uuid":"a2","parentUuid":"u2","message":{"id":"m","model":"claude-sonnet-4-20250514","content":[{"type":"text","text":"Late"}],"usage":{"input_tokens":10,"output_tokens":2},"stop_reason":"end_turn"}}`,
	)
	env.engine.SyncPaths([]string{path})

	assert.Equal(t, int64(1), counter.full.Load(),
		"an unidentifiable run keeps the whole-transcript fallback")
	msgs := fetchMessages(t, env.db, "interrupted")
	require.Len(t, msgs, 4,
		"a user turn between the two assistant records keeps them separate")
	assert.Equal(t, "Late", msgs[3].Content)
}

// TestClaudeIncrementalToolUseRunIsNotReparsedEverySync checks that a
// completed tool_use run advances the stored cursor, so later syncs of
// the same file neither re-parse it nor leave it unarchived.
func TestClaudeIncrementalToolUseRunIsNotReparsedEverySync(t *testing.T) {
	counter := &claudeFullParseCounter{}
	env, dir := newClaudeSplitTestEnv(t, counter)

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
