package sync

import (
	"os"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestCodexForkResultReviewRestoredOrder(t *testing.T) {
	for _, stage := range []int64{1, 1 << 30} {
		t.Run(strconv.FormatInt(stage, 10), func(t *testing.T) {
			e, d, childPath, _, meta := forkReviewEngine(t, stage, parser.AgentCodex)
			prefix := meta + testjsonl.JoinJSONL(
				testjsonl.CodexTurnContextWithIDJSON("gpt-5", "child-turn", forkReviewTime),
				testjsonl.CodexFunctionCallJSON("exec_command", "", forkReviewTime),
			)
			a := testjsonl.CodexFunctionCallOutputJSON("call_test", "earlier A", "2026-09-01T10:01:00Z")
			b := testjsonl.CodexFunctionCallOutputJSON("call_test", "latest B", "2026-09-01T10:02:00Z")
			// First observation is a shortened snapshot. The stable metadata,
			// turn and call then anchor the restored full result sequence.
			require.NoError(t, os.WriteFile(childPath, []byte(prefix+testjsonl.JoinJSONL(b)), 0o600))
			require.Equal(t, 1, e.SyncAll(t.Context(), nil).Deferred)
			first, err := d.GetAllMessages(t.Context(), "codex:"+forkReviewChild)
			require.NoError(t, err)
			require.Len(t, first, 1)
			require.Len(t, first[0].ToolCalls, 1)
			require.Equal(t, "latest B", first[0].ToolCalls[0].ResultContent)

			require.NoError(t, os.WriteFile(childPath, []byte(prefix+testjsonl.JoinJSONL(a, b)), 0o600))
			require.NoError(t, e.TrySyncSessionWithSubagentsContext(t.Context(), "codex:"+forkReviewChild))
			restored, err := d.GetAllMessages(t.Context(), "codex:"+forkReviewChild)
			require.NoError(t, err)
			require.Len(t, restored, 1)
			require.Len(t, restored[0].ToolCalls, 1)
			call := restored[0].ToolCalls[0]
			require.Len(t, call.ResultEvents, 2)
			t.Logf("restored source [A,B]: stored events=[%q,%q], summary=%q", call.ResultEvents[0].Content, call.ResultEvents[1].Content, call.ResultContent)
			assert.Equal(t, "earlier A", call.ResultEvents[0].Content, "restored source establishes A before shared B")
			assert.Equal(t, "latest B", call.ResultEvents[1].Content)
			assert.Equal(t, "latest B", call.ResultContent, "latest source result remains B")

			// A subsequent shortened snapshot must preserve the now established
			// latest result, even though the result carriers are absent.
			require.NoError(t, os.WriteFile(childPath, []byte(prefix), 0o600))
			require.NoError(t, e.TrySyncSessionWithSubagentsContext(t.Context(), "codex:"+forkReviewChild))
			retained, err := d.GetAllMessages(t.Context(), "codex:"+forkReviewChild)
			require.NoError(t, err)
			require.Len(t, retained, 1)
			require.Len(t, retained[0].ToolCalls, 1)
			t.Logf("later result-free source: summary=%q", retained[0].ToolCalls[0].ResultContent)
			assert.Equal(t, "latest B", retained[0].ToolCalls[0].ResultContent)
		})
	}
}

func TestCodexForkResultReviewSanitizedParticipation(t *testing.T) {
	for _, stage := range []int64{1, 1 << 30} {
		t.Run(strconv.FormatInt(stage, 10), func(t *testing.T) {
			e, d, childPath, _, meta := forkReviewEngine(t, stage, parser.AgentCodex)
			prefix := meta + testjsonl.JoinJSONL(
				testjsonl.CodexTurnContextWithIDJSON("gpt-5", "child-turn", forkReviewTime),
				testjsonl.CodexFunctionCallJSON("exec_command", "", forkReviewTime),
			)
			source := prefix + testjsonl.JoinJSONL(
				testjsonl.CodexFunctionCallOutputJSON("call_test", "older visible output", "2026-09-01T10:01:00Z"),
				testjsonl.CodexFunctionCallOutputJSON("call_test", "\x00\x01", "2026-09-01T10:02:00Z"),
			)
			require.NoError(t, os.WriteFile(childPath, []byte(source), 0o600))
			require.Equal(t, 1, e.SyncAll(t.Context(), nil).Deferred)
			first, err := d.GetAllMessages(t.Context(), "codex:"+forkReviewChild)
			require.NoError(t, err)
			require.Len(t, first, 1)
			require.Len(t, first[0].ToolCalls, 1)
			call := first[0].ToolCalls[0]
			require.Len(t, call.ResultEvents, 2)
			require.Empty(t, call.ResultContent, "latest raw control-only output participates, then sanitizes to empty")
			require.Equal(t, "older visible output", call.ResultEvents[0].Content)
			require.Empty(t, call.ResultEvents[1].Content)
			require.NotNil(t, call.ResultEvents[1].SummaryParticipates)
			require.True(t, *call.ResultEvents[1].SummaryParticipates)
			t.Logf("initial: summary=%q, last content=%q, participates=%v", call.ResultContent, call.ResultEvents[1].Content, *call.ResultEvents[1].SummaryParticipates)

			// The call remains in the source but both carriers disappear, so
			// reconciliation must rebuild from stored sanitized event rows.
			require.NoError(t, os.WriteFile(childPath, []byte(prefix), 0o600))
			require.NoError(t, e.TrySyncSessionWithSubagentsContext(t.Context(), "codex:"+forkReviewChild))
			retained, err := d.GetAllMessages(t.Context(), "codex:"+forkReviewChild)
			require.NoError(t, err)
			require.Len(t, retained, 1)
			require.Len(t, retained[0].ToolCalls, 1)
			call = retained[0].ToolCalls[0]
			require.Len(t, call.ResultEvents, 2)
			t.Logf("retained: summary=%q, last content=%q, participates=%v", call.ResultContent, call.ResultEvents[1].Content, *call.ResultEvents[1].SummaryParticipates)
			assert.Empty(t, call.ResultContent, "archived raw-participation metadata keeps the latest sanitized result authoritative")
		})
	}
}
