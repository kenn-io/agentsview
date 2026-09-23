package friction_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/friction"
	"go.kenn.io/agentsview/internal/parser"
)

const sid = "7d3f0e91-2c4b-4e2a-9c1b-44a7f3d8a1e2"

var base = time.Date(2026, 9, 16, 9, 0, 0, 0, time.UTC)

func ts(minutes int) string {
	return base.Add(time.Duration(minutes) * time.Minute).Format(time.RFC3339)
}

// claudeAssistant builds an assistant row exactly as the Claude parser
// stores it: content, thinking text and tool calls from
// parser.ExtractTextContent over Anthropic content blocks.
func claudeAssistant(t *testing.T, ord int, blocks string, results ...db.ToolResultEvent) db.Message {
	t.Helper()
	content, thinking, hasThinking, hasToolUse, calls, _ := parser.ExtractTextContent(t.Context(), gjson.Parse(blocks))
	m := dbtest.AsstMsg(sid, ord, content)
	m.ThinkingText, m.HasThinking, m.HasToolUse = thinking, hasThinking, hasToolUse
	m.Timestamp = ts(ord)
	for i, c := range calls {
		tc := db.ToolCall{
			SessionID: sid, ToolName: c.ToolName, Category: c.Category,
			ToolUseID: c.ToolUseID, InputJSON: c.InputJSON, CallIndex: i,
		}
		if i < len(results) {
			tc.ResultContent = results[i].Content
			tc.ResultEvents = []db.ToolResultEvent{results[i]}
		}
		m.ToolCalls = append(m.ToolCalls, tc)
	}
	return m
}

func userRow(ord int, content string) db.Message {
	m := dbtest.UserMsg(sid, ord, content)
	m.Timestamp = ts(ord)
	return m
}

func systemUserRow(ord int, subtype, content string) db.Message {
	m := userRow(ord, content)
	m.IsSystem, m.SourceType, m.SourceSubtype = true, "system", subtype
	return m
}

func compactRow(ord int, summary string) db.Message {
	m := dbtest.AsstMsg(sid, ord, summary)
	m.Timestamp = ts(ord)
	m.IsSystem, m.IsCompactBoundary = true, true
	m.SourceType, m.SourceSubtype = "system", "compact_boundary"
	return m
}

// rawFromDB is the row mapping PR 3's friction_compute performs.
func rawFromDB(t *testing.T, msgs []db.Message) ([]friction.RawMessage, []friction.RawToolCall) {
	t.Helper()
	var rm []friction.RawMessage
	var rc []friction.RawToolCall
	for _, m := range msgs {
		at, err := time.Parse(time.RFC3339Nano, m.Timestamp)
		require.NoError(t, err)
		rm = append(rm, friction.RawMessage{
			Ordinal: m.Ordinal, Role: m.Role, Content: m.Content,
			ThinkingText: m.ThinkingText, IsSystem: m.IsSystem,
			IsCompactBoundary: m.IsCompactBoundary, SourceSubtype: m.SourceSubtype,
			Timestamp: at, ContextTokens: m.ContextTokens, HasContextTokens: m.HasContextTokens,
		})
		for i, tc := range m.ToolCalls {
			c := friction.RawToolCall{
				MessageOrdinal: m.Ordinal, CallIndex: i,
				ToolName: tc.ToolName, Category: tc.Category,
				InputJSON: tc.InputJSON, ResultContent: tc.ResultContent,
				Timestamp: at,
			}
			if n := len(tc.ResultEvents); n > 0 {
				c.LastEventContent = tc.ResultEvents[n-1].Content
				c.EventStatus = tc.ResultEvents[n-1].Status
			}
			rc = append(rc, c)
		}
	}
	return rm, rc
}

func reviewArchived(t *testing.T, rows ...db.Message) []friction.Signal {
	t.Helper()
	d := dbtest.OpenTestDB(t)
	dbtest.SeedSession(t, d, sid, "app", dbtest.WithMessageCount(len(rows)))
	dbtest.SeedMessages(t, d, rows...)
	stored, err := d.GetAllMessages(t.Context(), sid)
	require.NoError(t, err)
	msgs, calls := rawFromDB(t, stored)
	in := friction.BuildSessionInput(sid, friction.Dims{Agent: "claude"}, false,
		msgs, calls, friction.BuildOptions{})
	return friction.Review(in)
}

func detectors(sigs []friction.Signal) []string {
	var out []string
	for _, s := range sigs {
		out = append(out, s.Detector)
	}
	return out
}

func TestArchivedClaudeSession(t *testing.T) {
	text := func(s string) string { return `[{"type":"text","text":"` + s + `"}]` }

	t.Run("real correction fires as control", func(t *testing.T) {
		got := reviewArchived(t,
			claudeAssistant(t, 0, text("I changed the config.")),
			userRow(1, "no, revert the config change first"),
			claudeAssistant(t, 2, text("Reverted.")),
		)
		require.Len(t, got, 1)
		assert.Equal(t, friction.DetectorCorrectionCoding, got[0].Detector)
		assert.Equal(t, "no, revert the config change first", got[0].Text)
		require.NotNil(t, got[0].Ordinal)
		assert.Equal(t, 1, *got[0].Ordinal)
	})

	t.Run("no_correction_from_system_compact_or_echo_rows", func(t *testing.T) {
		got := reviewArchived(t,
			claudeAssistant(t, 0, text("Working on it.")),
			systemUserRow(1, "continuation", "This session is being continued from a previous conversation."),
			claudeAssistant(t, 2, text("Continuing.")),
			systemUserRow(3, "interrupted", "[Request interrupted by user for tool use]"),
			claudeAssistant(t, 4, text("Stopped.")),
			compactRow(5, "Summary: the user asked to stop doing that and fix it"),
			userRow(6, "short"),
			claudeAssistant(t, 7, text("Next.")),
			systemUserRow(8, "stop_hook", "Stop hook feedback: tests must pass first"),
			claudeAssistant(t, 9, text("Done.")),
		)
		// interrupted_row_one_finding_never_in_correction_window: the
		// interrupted row at ordinal 3 sits between assistant turns 2 and 4
		// and is long enough to be a correction, but it yields exactly one
		// interruption finding (spec §6.8) and no correction.
		assert.Equal(t, []string{friction.DetectorInterruption}, detectors(got))
		require.Len(t, got, 1)
		require.NotNil(t, got[0].Ordinal)
		assert.Equal(t, 3, *got[0].Ordinal)

		// tool_result-subtype rows are the provider fallback echo shape.
		echo := userRow(11, "total 12 files listed in the output here")
		echo.SourceSubtype = parser.SourceSubtypeToolResult
		assert.Empty(t, reviewArchived(t,
			claudeAssistant(t, 10, text("Listing.")), echo,
			claudeAssistant(t, 12, text("Listed."))))
	})

	t.Run("compact_boundary_cannot_create_correction_window", func(t *testing.T) {
		// If the compact summary survives as an assistant turn, it makes
		// ordinal 3 look like a correction between two assistant turns.
		// Dropping it leaves two adjacent user turns and no correction.
		got := reviewArchived(t,
			claudeAssistant(t, 0, text("Working on the config.")),
			userRow(1, "ok"),
			compactRow(2, "Summary: the config work continues"),
			userRow(3, "no, revert the config change first"),
			claudeAssistant(t, 4, text("Reverted.")),
		)
		assert.Empty(t, got)

		// A regular assistant row in the same position creates the
		// correction, so this case is sensitive to the boundary filter.
		retained := reviewArchived(t,
			claudeAssistant(t, 0, text("Working on the config.")),
			userRow(1, "ok"),
			claudeAssistant(t, 2, text("Summary: the config work continues")),
			userRow(3, "no, revert the config change first"),
			claudeAssistant(t, 4, text("Reverted.")),
		)
		require.Len(t, retained, 1)
		assert.Equal(t, friction.DetectorCorrectionCoding, retained[0].Detector)
		require.NotNil(t, retained[0].Ordinal)
		assert.Equal(t, 3, *retained[0].Ordinal)
	})

	t.Run("no_workaround_or_deferral_from_thinking_or_todowrite", func(t *testing.T) {
		blocks := `[
			{"type":"thinking","thinking":"I'll leave this for now and come back to it next session"},
			{"type":"text","text":"Updated the plan."},
			{"type":"tool_use","id":"toolu_01","name":"TodoWrite","input":{"todos":[
				{"content":"Remove the temporary hack","status":"pending","activeForm":"Removing"}]}}
		]`
		got := reviewArchived(t,
			userRow(0, "plan the refactor"),
			claudeAssistant(t, 1, blocks, db.ToolResultEvent{Status: "completed", Content: "Todos updated"}),
		)
		assert.Empty(t, got, "workarounds_skip_tool_use_blocks extended with TodoWrite and thinking")
	})

	t.Run("errors_for_errored_and_cancelled_calls", func(t *testing.T) {
		blocks := `[
			{"type":"text","text":"Running both."},
			{"type":"tool_use","id":"toolu_a","name":"Bash","input":{"command":"cargo test"}},
			{"type":"tool_use","id":"toolu_b","name":"Read","input":{"file_path":"/home/user/app/a.rs"}}
		]`
		got := reviewArchived(t,
			userRow(0, "run the tests"),
			claudeAssistant(t, 1, blocks,
				db.ToolResultEvent{Status: "errored", Content: "error[E0308]: mismatched types"},
				db.ToolResultEvent{Status: "cancelled", Content: "The user doesn't want to proceed"},
			),
		)
		require.Len(t, got, 2)
		assert.Equal(t, []string{friction.DetectorError, friction.DetectorError}, detectors(got))
		assert.Equal(t, "Bash", got[0].ToolName)
		assert.Equal(t, "error[E0308]: mismatched types", got[0].Text)
		assert.Equal(t, "Read", got[1].ToolName)
		require.NotNil(t, got[1].CallIndex)
		assert.Equal(t, 1, *got[1].CallIndex)
	})

	t.Run("bash_bare_timeout_suppressed", func(t *testing.T) {
		blocks := `[{"type":"tool_use","id":"toolu_t","name":"Bash","input":{"command":"sleep 999"}}]`
		got := reviewArchived(t,
			userRow(0, "wait for it"),
			claudeAssistant(t, 1, blocks,
				db.ToolResultEvent{Status: "errored", Content: "Command timed out after 120 seconds"}),
		)
		assert.Empty(t, got)
	})

	t.Run("workaround in real text still fires", func(t *testing.T) {
		got := reviewArchived(t,
			userRow(0, "make it work"),
			claudeAssistant(t, 1, text("Using a hardcoded value for now.")),
		)
		assert.Equal(t, []string{friction.DetectorWorkaround}, detectors(got))
		require.Len(t, got, 1)
		assert.Equal(t, "for now", got[0].Label)
	})
}
