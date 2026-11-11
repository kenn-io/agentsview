package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSessionPushReservesOrdinaryWriterOwnership(t *testing.T) {
	d := testDB(t)
	receipt, first := sessionPushFixture("reserved")
	_, err := d.ApplySessionPush(t.Context(), receipt, first)
	require.NoError(t, err)
	_, replacement := sessionPushFixture("reserved")
	replacement.Messages[0].Content = "ordinary source replacement"
	_, err = d.WriteSessionBatchAtomic(t.Context(), []SessionBatchWrite{replacement})
	require.ErrorIs(t, err, ErrSessionPushConflict)
	messages, err := d.GetAllMessages(t.Context(), "reserved")
	require.NoError(t, err)
	require.Len(t, messages, 1)
	assert.Equal(t, "first snapshot", messages[0].Content)

}

func TestSessionPushRejectsDirectTranscriptMutation(t *testing.T) {
	for _, mode := range []string{"append", "replace", "usage", "plan"} {
		t.Run(mode, func(t *testing.T) {
			d := testDB(t)
			receipt, write := sessionPushFixture("reserved")
			_, err := d.ApplySessionPush(t.Context(), receipt, write)
			require.NoError(t, err)
			switch mode {
			case "append":
				err = d.InsertMessages(t.Context(), []Message{{SessionID: "reserved", Ordinal: 1, Role: "assistant", Content: "new content"}})
			case "replace":
				err = d.ReplaceSessionMessages(t.Context(), "reserved", []Message{{SessionID: "reserved", Ordinal: 0, Role: "user", Content: "replacement"}})
			case "usage":
				err = d.ReplaceSessionUsageEvents(t.Context(), "reserved", []UsageEvent{{SessionID: "reserved", Source: "session", Model: "model", InputTokens: 999}})
			case "plan":
				err = d.ReplaceSessionPlanUsage(t.Context(), "reserved", &SessionPlanUsage{HasPlanUsage: true, DataStatus: "available"})
			}
			require.ErrorIs(t, err, ErrSessionPushConflict)
		})
	}
}
