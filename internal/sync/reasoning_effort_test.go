package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

func TestReasoningEffortNativeConversionAndDiff(t *testing.T) {
	rows := toDBMessages(pendingWrite{
		sess: parser.ParsedSession{ID: "sync-session"},
		msgs: []parser.ParsedMessage{
			{
				Ordinal:         0,
				Role:            parser.RoleAssistant,
				Content:         "answer",
				Model:           "model-test",
				ReasoningEffort: "high",
			},
			{
				Ordinal: 1,
				Role:    parser.RoleUser,
				Content: "question",
			},
		},
	}, nil)
	require.Len(t, rows, 2)
	assert.Equal(t, "high", rows[0].ReasoningEffort)
	assert.Empty(t, rows[1].ReasoningEffort)

	changed := rows[0]
	changed.ReasoningEffort = "medium"
	assert.NotEqual(t,
		messageTokenFingerprintTwin(rows),
		messageTokenFingerprintTwin([]db.Message{changed, rows[1]}),
	)
	assert.Equal(t, "reasoning_effort differs",
		messageMetadataDiff(rows[0], changed))
}
