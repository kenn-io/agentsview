package sync

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/friction"
	"go.kenn.io/agentsview/internal/friction/review"
	"go.kenn.io/agentsview/internal/nanoclaw"
	"go.kenn.io/agentsview/internal/nanoclaw/nanoclawtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

// cellPipelineBody is reader_pipeline.rs:186-196, scrubbed: a chat
// correction after an assistant turn, then the same bash call four times.
// A model is set so agentsview records usage rows.
func cellPipelineBody(session string) string {
	lines := []string{
		`{"type":"assistant","uuid":"a-pre","timestamp":"2026-07-01T08:59:00.000Z","message":{"id":"msg_pre","role":"assistant","model":"claude-sonnet-4-5","content":[{"type":"text","text":"Posting the summary here."}]},"sessionId":"` + session + `"}`,
		`{"type":"user","uuid":"u1","timestamp":"2026-07-01T09:00:00.000Z","message":{"role":"user","content":"<message id=\"1\" from=\"mg-1\">no helper, don't answer in that channel</message>"},"sessionId":"` + session + `"}`,
	}
	for i := range 4 {
		lines = append(lines, fmt.Sprintf(`{"type":"assistant","uuid":"a%d","timestamp":"2026-07-01T09:%02d:00.000Z","message":{"id":"msg_%d","role":"assistant","model":"claude-sonnet-4-5","content":[{"type":"text","text":"retrying"},{"type":"tool_use","id":"toolu_%d","name":"bash","input":{"command":"cargo build"}}],"usage":{"input_tokens":10,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":5}},"sessionId":"%s"}`,
			i, 10+i, i, i, session))
	}
	return strings.Join(lines, "\n") + "\n"
}

func fleetRunner(store review.Store) *review.Runner {
	return &review.Runner{
		Store:        store,
		Loc:          time.UTC,
		Now:          func() time.Time { return time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC) },
		BackfillDays: 7,
	}
}

func buildDigest(t *testing.T, store review.Store, date string) (review.Report, string) {
	t.Helper()
	rep, err := fleetRunner(store).BuildDate(t.Context(), date, review.BuildOptions{})
	require.NoError(t, err)
	require.True(t, rep.Written)
	stored, err := store.GetFrictionDigest(t.Context(), date)
	require.NoError(t, err)
	require.NotNil(t, stored)
	return rep, string(stored.Markdown)
}

// digestLine matches one digest bullet, allowing the agent:/machine:
// segments PR 5 stamps between the persona key and the session id.
func digestLine(prefix, rest string) *regexp.Regexp {
	return regexp.MustCompile(`(?m)^- ` + regexp.QuoteMeta(prefix) + "(`[^`\n]+` )*" + regexp.QuoteMeta(rest) + `$`)
}

func TestNanoClawCellDigest(t *testing.T) {
	t.Run("nanoclaw_cell_fixture_produces_dims_and_pattern_section", func(t *testing.T) {
		fx := newNanoClawFixture(t, nanoclaw.Filter{}, nil, "ag-1")
		nanoclawtest.WriteV2DB(t, fx.data)
		fx.syncOne(t, nanoclawtest.WriteSession(t, fx.data, "ag-1", "sess-cell", cellPipelineBody("sess-cell")))

		_, md := buildDigest(t, fx.db, "2026-07-01")
		assert.Contains(t, md, "## Patterns")
		assert.Regexp(t, digestLine("`helper@general` ", "`sess-cell` kind=`retry_loop`: `bash` x4 identical arguments 09:10-09:13"), md)
		// Chat correction stamped and prefixed.
		assert.Regexp(t, digestLine("`helper@general` ", "`sess-cell` — 'no helper, don\\'t answer in that channel'"), md)
		assert.Contains(t, md, "## Personas")
		assert.Contains(t, md, "- `helper@general`: 1 corrections, 0 errors, 0 workarounds, 0 deferrals, 1 patterns (1 session(s))")
		assert.Contains(t, md, "- **Tokens**: 40 in / 20 out")
	})

	t.Run("hostile_channel_names_are_sanitized", func(t *testing.T) {
		fx := newNanoClawFixture(t, nanoclaw.Filter{}, nil, "ag-1")
		dbPath := nanoclawtest.WriteV2DB(t, fx.data)
		nanoclawtest.Exec(t, dbPath, "UPDATE messaging_groups SET name = ? WHERE id = 'mg-1'", "gen`eral\n## Injected")
		fx.syncOne(t, nanoclawtest.WriteSession(t, fx.data, "ag-1", "sess-cell", cellPipelineBody("sess-cell")))

		_, md := buildDigest(t, fx.db, "2026-07-01")
		assert.Contains(t, md, "- `helper@gen'eral ## Injected`: 1 corrections")
		assert.NotContains(t, md, "\n## Injected")
	})

	t.Run("excluded_agents_never_reach_the_digest", func(t *testing.T) {
		fx := newNanoClawFixture(t, nanoclaw.Filter{Exclude: []string{"reviewer"}}, nil, "ag-1", "ag-2")
		nanoclawtest.WriteV2DB(t, fx.data)
		fx.syncOne(t, nanoclawtest.WriteSession(t, fx.data, "ag-1", "sess-cell", cellPipelineBody("sess-cell")))
		fx.syncOne(t, nanoclawtest.WriteSession(t, fx.data, "ag-2", "sess-private", cellPipelineBody("sess-private")))

		rep, md := buildDigest(t, fx.db, "2026-07-01")
		assert.Equal(t, 1, rep.Snapshot.SessionsScanned)
		assert.NotContains(t, md, "sess-private")
		assert.NotContains(t, md, "reviewer")
		assert.NotContains(t, md, "events team")
	})
}

// Rewrites reader_pipeline.rs pooled_codex_seat_reaches_signals_without_chat_heuristics
// for configured seat patterns (D33): the seat reaches the dims, findings
// and digest, and never selects the chat detector.
func TestConfiguredSeatNeverSelectsChatDetector(t *testing.T) {
	const sessionUUID = "0f0e0d0c-0b0a-4908-8706-050403020100"
	root := filepath.Join(t.TempDir(), "profiles", "seat-17", "sessions")
	path := filepath.Join(root, "2026", "07", "01", "rollout-2026-07-01T10-00-00-"+sessionUUID+".jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	body := testjsonl.NewSessionBuilder().
		AddCodexMeta("2026-07-01T10:00:00Z", sessionUUID, "/workspace/project-a", "codex_cli_rs").
		AddCodexMessage("2026-07-01T10:00:01Z", "user", "Set up the deploy script.").
		AddCodexMessage("2026-07-01T10:00:02Z", "assistant", "Working on it.").
		// No chat marker: only the coding detector accepts this correction.
		AddCodexMessage("2026-07-01T10:00:03Z", "user", "please use the staging database instead").
		AddCodexMessage("2026-07-01T10:00:04Z", "assistant", "A temporary workaround is in place.").
		String()
	require.NoError(t, os.WriteFile(path, []byte(body), 0o644))

	patterns, err := friction.CompileSeatPatterns([]string{"*/profiles/{seat}/sessions/*"})
	require.NoError(t, err)
	database := openTestDB(t)
	engine := NewEngine(t.Context(), database, EngineConfig{
		AgentDirs:    map[parser.AgentType][]string{parser.AgentCodex: {root}},
		Machine:      "local",
		FrictionDims: NewFrictionDims(nil, patterns),
	})
	t.Cleanup(engine.Close)
	engine.SyncAll(t.Context(), nil)
	ids, err := database.ListSessionIDsByFilePath(t.Context(), path, "codex")
	require.NoError(t, err)
	require.Len(t, ids, 1)
	id := ids[0]

	dims, err := database.SessionFrictionDims(t.Context(), id)
	require.NoError(t, err)
	require.NotNil(t, dims)
	assert.Equal(t, "seat-17", dims.Seat)
	assert.Equal(t, "seat_pattern", dims.DimsSource)
	assert.Empty(t, dims.Persona)

	findings, err := database.SessionFrictionFindings(t.Context(), id)
	require.NoError(t, err)
	assert.Len(t, findingsWithDetector(findings, "correction.coding"), 1)
	assert.Empty(t, findingsWithDetector(findings, "correction.chat"))
	workarounds := findingsWithDetector(findings, "workaround")
	require.Len(t, workarounds, 1)
	assert.Equal(t, "temporary", workarounds[0].Label)

	rep, md := buildDigest(t, database, "2026-07-01")
	assert.Contains(t, md, "`seat:seat-17` ")
	assert.Empty(t, rep.Snapshot.Personas)

	// Reviewed once: building the same date again is a no-op (jilog's second
	// run scanned 0 sessions).
	again, err := fleetRunner(database).BuildDate(t.Context(), "2026-07-01", review.BuildOptions{})
	require.NoError(t, err)
	assert.False(t, again.Written)
}
