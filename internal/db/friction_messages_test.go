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
		})
	}
}

func TestFrictionInputBudgetRejectsChangedTranscript(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	insertSession(t, d, "changed", "project-a")
	require.NoError(t, d.ReplaceSessionMessages(ctx, "changed", []Message{
		{SessionID: "changed", Ordinal: 0, Role: "user", Content: strings.Repeat("x", frictionMaxBytes+1)},
	}))
	before, err := d.GetSessionFull(ctx, "changed")
	require.NoError(t, err)
	require.NotNil(t, before)
	require.NotNil(t, before.TranscriptRevision)
	require.NoError(t, d.ReplaceSessionMessages(ctx, "changed", []Message{
		{SessionID: "changed", Ordinal: 0, Role: "user", Content: "short replacement"},
	}))
	ok, err := d.FrictionInputWithinBudget(ctx, "changed", *before.TranscriptRevision)
	require.NoError(t, err)
	assert.False(t, ok, "a smaller replacement must not authorize detection on older oversized input")
	after, err := d.GetSessionFull(ctx, "changed")
	require.NoError(t, err)
	require.NotNil(t, after)
	require.NotNil(t, after.TranscriptRevision)
	ok, err = d.FrictionInputWithinBudget(ctx, "changed", *after.TranscriptRevision)
	require.NoError(t, err)
	assert.True(t, ok)
}
