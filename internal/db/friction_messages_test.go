package db

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetFrictionMessagesBoundsCompleteHistory(t *testing.T) {
	for _, field := range []string{"content", "thinking", "input", "result", "event", "rows"} {
		t.Run(field, func(t *testing.T) {
			d := testDB(t)
			require.NoError(t, d.UpsertSession(t.Context(), Session{ID: "bounded", Agent: "codex", Project: "test", Machine: "local"}))
			msgs := []Message{{SessionID: "bounded", Role: "assistant", Content: "response", ToolCalls: []ToolCall{{ToolName: "Bash", ToolUseID: "call", InputJSON: "{}", ResultContent: "failed", ResultEvents: []ToolResultEvent{{Source: "tool", Status: "errored", Content: "failed"}}}}}}
			require.NoError(t, d.ReplaceSessionMessages(t.Context(), "bounded", msgs))
			want, err := d.GetAllMessages(t.Context(), "bounded")
			require.NoError(t, err)
			got, ok, err := d.GetFrictionMessages(t.Context(), "bounded")
			require.NoError(t, err)
			require.True(t, ok)
			assert.Equal(t, want, got, "within budget, all evidence is preserved")
			assert.True(t, FrictionInputFits(want), "loaded rows pass the same budget")
			large := strings.Repeat("x", frictionMaxBytes+1)
			switch field {
			case "content":
				msgs[0].Content = large
			case "thinking":
				msgs[0].ThinkingText = large
			case "input":
				msgs[0].ToolCalls[0].InputJSON = `{"command":"` + large + `"}`
			case "result":
				msgs[0].ToolCalls[0].ResultContent = large
			case "event":
				msgs[0].ToolCalls[0].ResultEvents[0].Content = large
			case "rows":
				msgs = make([]Message, frictionMaxRows+1)
				for i := range msgs {
					msgs[i] = Message{SessionID: "bounded", Ordinal: i, Role: "user", Content: "hello"}
				}
			}
			require.NoError(t, d.ReplaceSessionMessages(t.Context(), "bounded", msgs))
			got, ok, err = d.GetFrictionMessages(t.Context(), "bounded")
			require.NoError(t, err)
			assert.False(t, ok, "reject oversized input before hydrating it")
			assert.Nil(t, got, "never return partial evidence")
			assert.False(t, FrictionInputFits(msgs), "in-memory rows fail the same budget")
		})
	}
}

func TestFrictionBudgetCountsDeduplicatedResultOnce(t *testing.T) {
	d := testDB(t)
	require.NoError(t, d.UpsertSession(t.Context(), Session{ID: "dedup", Agent: "codex", Project: "test", Machine: "local"}))
	output := strings.Repeat("x", 3<<20)
	msgs := []Message{{SessionID: "dedup", Role: "assistant", Content: "running", ToolCalls: []ToolCall{{ToolName: "Bash", ToolUseID: "call", InputJSON: "{}", ResultContent: output, ResultEvents: []ToolResultEvent{{Source: "tool", Status: "errored", Content: output}}}}}}
	require.NoError(t, d.ReplaceSessionMessages(t.Context(), "dedup", msgs))
	got, ok, err := d.GetFrictionMessages(t.Context(), "dedup")
	require.NoError(t, err)
	require.True(t, ok, "storage keeps one copy of a sole result, which fits")
	require.Equal(t, output, got[0].ToolCalls[0].ResultContent, "loading restores the summary")
	assert.True(t, FrictionInputFits(got), "the restored summary must not be counted twice")
}
