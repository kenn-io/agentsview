package sync

import (
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/friction"
	"go.kenn.io/agentsview/internal/nanoclaw"
	"go.kenn.io/agentsview/internal/nanoclaw/nanoclawtest"
	"go.kenn.io/agentsview/internal/parser"
)

type nanoClawFixture struct {
	db     *db.DB
	engine *Engine
	data   string
}

// newNanoClawFixture archives cell transcripts through the real Claude
// parser. Each agent's .claude-shared/projects directory is a Claude root,
// which is how a user points agentsview at a cell (spec §11.1).
func newNanoClawFixture(t *testing.T, filter nanoclaw.Filter, seatPatterns []string, agents ...string) *nanoClawFixture {
	t.Helper()
	data := t.TempDir()
	roots := make([]string, 0, len(agents))
	for _, agent := range agents {
		root := nanoclawtest.ProjectsRoot(data, agent)
		require.NoError(t, os.MkdirAll(root, 0o755))
		roots = append(roots, root)
	}
	patterns, err := friction.CompileSeatPatterns(seatPatterns)
	require.NoError(t, err)
	database := openTestDB(t)
	engine := NewEngine(t.Context(), database, EngineConfig{
		AgentDirs:    map[parser.AgentType][]string{parser.AgentClaude: roots},
		Machine:      "local",
		FrictionDims: NewFrictionDims(nanoclaw.NewResolver(data, "", filter), patterns),
	})
	t.Cleanup(engine.Close)
	return &nanoClawFixture{db: database, engine: engine, data: data}
}

func (fx *nanoClawFixture) syncOne(t *testing.T, path string) string {
	t.Helper()
	fx.engine.SyncAll(t.Context(), nil)
	ids, err := fx.db.ListSessionIDsByFilePath(t.Context(), path, "claude")
	require.NoError(t, err)
	require.Len(t, ids, 1, "archived session for %s", path)
	return ids[0]
}

// input rebuilds the detector input from the archive with PR 3's mapper.
func (fx *nanoClawFixture) input(t *testing.T, id string, unwrap bool) friction.SessionInput {
	t.Helper()
	msgs, err := fx.db.GetAllMessages(t.Context(), id)
	require.NoError(t, err)
	raw, calls := frictionRawInput(msgs)
	return friction.BuildSessionInput(id, friction.Dims{Persona: "helper"}, false, raw, calls,
		friction.BuildOptions{UnwrapNanoClawEnvelope: unwrap})
}

func roles(msgs []friction.Message) []string {
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, m.Role)
	}
	return out
}

func findingsWithDetector(findings []db.FrictionFinding, detector string) []db.FrictionFinding {
	var out []db.FrictionFinding
	for _, f := range findings {
		if f.Detector == detector {
			out = append(out, f)
		}
	}
	return out
}

func TestNanoClawCellThroughSync(t *testing.T) {
	t.Run("stored_correction_uses_unwrapped_length", func(t *testing.T) {
		fx := newNanoClawFixture(t, nanoclaw.Filter{}, nil, "ag-1")
		nanoclawtest.WriteV2DB(t, fx.data)
		envelope := `<message id="2" reply-to="` + strings.Repeat("x", 220) + `">no helper, don&#39;t answer in that channel</message>`
		body := `{"type":"assistant","uuid":"a1","timestamp":"2026-07-08T10:00:00.000Z","message":{"role":"assistant","content":[{"type":"text","text":"I will answer in that channel."}]},"sessionId":"s-correction"}` + "\n" +
			`{"type":"user","uuid":"u1","timestamp":"2026-07-08T10:00:01.000Z","message":{"role":"user","content":` + strconv.Quote(envelope) + `},"sessionId":"s-correction"}` + "\n" +
			`{"type":"assistant","uuid":"a2","timestamp":"2026-07-08T10:00:02.000Z","message":{"role":"assistant","content":[{"type":"text","text":"Understood."}]},"sessionId":"s-correction"}` + "\n"
		id := fx.syncOne(t, nanoclawtest.WriteSession(t, fx.data, "ag-1", "s-correction", body))
		findings, err := fx.db.SessionFrictionFindings(t.Context(), id)
		require.NoError(t, err)
		corrections := findingsWithDetector(findings, "correction.chat")
		require.Len(t, corrections, 1)
		assert.Equal(t, "no helper, don't answer in that channel", corrections[0].Text)
	})

	t.Run("nanoclaw_load_unwraps_envelope_and_maps_tool_results", func(t *testing.T) {
		fx := newNanoClawFixture(t, nanoclaw.Filter{}, nil, "ag-1")
		nanoclawtest.WriteV2DB(t, fx.data)
		id := fx.syncOne(t, nanoclawtest.WriteSession(t, fx.data, "ag-1", "s-1", nanoclawtest.SessionBody))

		in := fx.input(t, id, true)
		assert.Equal(t, []string{"user", "assistant", "tool", "assistant"}, roles(in.Messages))
		require.Len(t, in.Messages, 4)
		assert.Equal(t, "no helper, don't answer in that channel", in.Messages[0].Text,
			"envelope XML stripped, entities unescaped")
		assert.Equal(t, "Bash", in.Messages[2].ToolName, "failing tool_result named via the tool_use id")
		assert.JSONEq(t, `{"error":"command not found","success":false}`, in.Messages[2].Text)

		dims, err := fx.db.SessionFrictionDims(t.Context(), id)
		require.NoError(t, err)
		require.NotNil(t, dims)
		assert.Equal(t, db.FrictionSessionDims{
			SessionID: id, Persona: "helper", Channel: "general", DimsSource: "nanoclaw",
		}, *dims)

		findings, err := fx.db.SessionFrictionFindings(t.Context(), id)
		require.NoError(t, err)
		errs := findingsWithDetector(findings, "error")
		require.Len(t, errs, 1)
		assert.Equal(t, "Bash", errs[0].ToolName)
		assert.Equal(t, "command not found", errs[0].Text)
	})

	t.Run("nanoclaw_events_skip_queue_ops_and_tool_echoes", func(t *testing.T) {
		fx := newNanoClawFixture(t, nanoclaw.Filter{}, nil, "ag-1")
		nanoclawtest.WriteV2DB(t, fx.data)
		id := fx.syncOne(t, nanoclawtest.WriteSession(t, fx.data, "ag-1", "s-1", nanoclawtest.SessionBody))

		in := fx.input(t, id, true)
		// The enqueue line and the tool_result echo are not user activity.
		assert.Len(t, in.Patterns.UserOrdinals, 1)
		require.Len(t, in.Patterns.Calls, 1)
		assert.Equal(t, "Bash", in.Patterns.Calls[0].ToolName)
		assert.JSONEq(t, `{"command":"true"}`, in.Patterns.Calls[0].InputJSON)
	})

	t.Run("nanoclaw_stats_sum_tokens_without_cost", func(t *testing.T) {
		fx := newNanoClawFixture(t, nanoclaw.Filter{}, nil, "ag-1")
		nanoclawtest.WriteV2DB(t, fx.data)
		id := fx.syncOne(t, nanoclawtest.WriteSession(t, fx.data, "ag-1", "s-1", nanoclawtest.SessionBody))

		usage, err := fx.db.FrictionUsageForSessions(t.Context(), []string{id})
		require.NoError(t, err)
		// Input side: (100+200+300) + (10+0+600); output: 42 + 7. Cost is
		// not asserted: agentsview prices known models, jilog cannot
		// (Spec gaps 9).
		assert.Equal(t, uint64(1210), usage[id].InputTokens)
		assert.Equal(t, uint64(49), usage[id].OutputTokens)
	})

	t.Run("nanoclaw_split_multiblock_response_counts_usage_once", func(t *testing.T) {
		fx := newNanoClawFixture(t, nanoclaw.Filter{}, nil, "ag-1")
		nanoclawtest.WriteV2DB(t, fx.data)
		body := `{"type":"assistant","uuid":"a1","timestamp":"2026-07-08T10:00:00.000Z","message":{"id":"msg_01","role":"assistant","model":"claude-opus-4-8","content":[{"type":"text","text":"part one"}],"usage":{"input_tokens":10,"cache_creation_input_tokens":20,"cache_read_input_tokens":5000,"output_tokens":30}},"sessionId":"s-s"}
{"type":"assistant","uuid":"a2","timestamp":"2026-07-08T10:00:00.500Z","message":{"id":"msg_01","role":"assistant","model":"claude-opus-4-8","content":[{"type":"tool_use","id":"toolu_02","name":"Bash","input":{"command":"ls"}}],"usage":{"input_tokens":10,"cache_creation_input_tokens":20,"cache_read_input_tokens":5000,"output_tokens":30}},"sessionId":"s-s"}
{"type":"assistant","uuid":"a3","timestamp":"2026-07-08T10:01:00.000Z","message":{"id":"msg_02","role":"assistant","model":"claude-opus-4-8","content":[{"type":"text","text":"second response"}],"usage":{"input_tokens":1,"cache_creation_input_tokens":2,"cache_read_input_tokens":3,"output_tokens":4}},"sessionId":"s-s"}
`
		id := fx.syncOne(t, nanoclawtest.WriteSession(t, fx.data, "ag-1", "s-s", body))

		usage, err := fx.db.FrictionUsageForSessions(t.Context(), []string{id})
		require.NoError(t, err)
		// msg_01 counted once: (10+20+5000) + msg_02 (1+2+3); out 30 + 4.
		assert.Equal(t, uint64(5036), usage[id].InputTokens)
		assert.Equal(t, uint64(34), usage[id].OutputTokens)

		in := fx.input(t, id, true)
		assert.Len(t, in.Patterns.Calls, 1, "tool_use block still counted once")
		// agentsview merges same-id chunks into one message where jilog
		// keeps one per line; the text of both responses survives either way.
		var texts []string
		for _, m := range in.Messages {
			if m.Role == "assistant" && strings.TrimSpace(m.Text) != "" {
				texts = append(texts, m.Text)
			}
		}
		assert.Equal(t, []string{"part one", "second response"}, texts)
	})

	t.Run("nanoclaw_load_skips_compact_summary_and_meta_lines", func(t *testing.T) {
		fx := newNanoClawFixture(t, nanoclaw.Filter{}, nil, "ag-1")
		nanoclawtest.WriteV2DB(t, fx.data)
		body := `{"type":"user","isCompactSummary":true,"timestamp":"2026-07-08T10:00:00.000Z","message":{"role":"user","content":"Summary: user said don't post there; assistant used a workaround for now."},"sessionId":"s-m"}
{"type":"user","isMeta":true,"timestamp":"2026-07-08T10:00:01.000Z","message":{"role":"user","content":"runtime-injected meta line"},"sessionId":"s-m"}
{"type":"user","uuid":"u1","timestamp":"2026-07-08T10:00:02.000Z","message":{"role":"user","content":"<message id=\"1\" from=\"mg\">a real user message</message>"},"sessionId":"s-m"}
`
		id := fx.syncOne(t, nanoclawtest.WriteSession(t, fx.data, "ag-1", "s-m", body))

		in := fx.input(t, id, true)
		require.Len(t, in.Messages, 1, "only the real user message survives")
		assert.Equal(t, "a real user message", in.Messages[0].Text)
		assert.Len(t, in.Patterns.CompactBoundaries, 1)
		assert.Len(t, in.Patterns.UserOrdinals, 1, "no user activity for the isMeta line")

		findings, err := fx.db.SessionFrictionFindings(t.Context(), id)
		require.NoError(t, err)
		assert.Empty(t, findingsWithDetector(findings, "workaround"),
			"the summary's 'for now' must not reach the detectors")
	})

	t.Run("nanoclaw_compact_summary_becomes_compaction_event", func(t *testing.T) {
		fx := newNanoClawFixture(t, nanoclaw.Filter{}, nil, "ag-1")
		nanoclawtest.WriteV2DB(t, fx.data)
		body := `{"type":"user","isCompactSummary":true,"timestamp":"2026-07-08T10:00:00.000Z","message":{"role":"user","content":"This session is being continued from a previous conversation..."},"sessionId":"s-c"}
{"type":"assistant","timestamp":"2026-07-08T10:00:05.000Z","message":{"role":"assistant","content":[{"type":"text","text":"Continuing."}]},"sessionId":"s-c"}
`
		id := fx.syncOne(t, nanoclawtest.WriteSession(t, fx.data, "ag-1", "s-c", body))

		in := fx.input(t, id, true)
		assert.Len(t, in.Patterns.CompactBoundaries, 1)
		assert.Equal(t, []string{"assistant"}, roles(in.Messages))
		assert.Empty(t, in.Patterns.UserOrdinals)
	})

	t.Run("excluded_session_has_no_findings_and_no_names", func(t *testing.T) {
		fx := newNanoClawFixture(t, nanoclaw.Filter{Exclude: []string{"reviewer"}}, nil, "ag-2")
		nanoclawtest.WriteV2DB(t, fx.data)
		id := fx.syncOne(t, nanoclawtest.WriteSession(t, fx.data, "ag-2", "s-2", nanoclawtest.SessionBody))

		findings, err := fx.db.SessionFrictionFindings(t.Context(), id)
		require.NoError(t, err)
		assert.Empty(t, findings)
		dims, err := fx.db.SessionFrictionDims(t.Context(), id)
		require.NoError(t, err)
		require.NotNil(t, dims)
		assert.Equal(t, db.FrictionSessionDims{SessionID: id, DimsSource: "nanoclaw", ReviewExcluded: true}, *dims)
	})
}

func TestNoFleetConfigWritesNoDimsRow(t *testing.T) {
	fx := newEngineFixture(t) // EngineConfig without FrictionDims
	path := fx.writeClaudeSession(t, "proj", "plain.jsonl", "please use the staging database instead")
	fx.engine.SyncAll(t.Context(), nil)
	ids, err := fx.db.ListSessionIDsByFilePath(t.Context(), path, "claude")
	require.NoError(t, err)
	require.Len(t, ids, 1)
	dims, err := fx.db.SessionFrictionDims(t.Context(), ids[0])
	require.NoError(t, err)
	assert.Nil(t, dims)
}
