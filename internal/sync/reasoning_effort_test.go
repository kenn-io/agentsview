package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

func TestReasoningEffortNativeConversionAndDiff(t *testing.T) {
	assert := assert.New(t)

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
	assert.Equal("high", rows[0].ReasoningEffort)
	assert.Empty(rows[1].ReasoningEffort)

	changed := rows[0]
	changed.ReasoningEffort = "medium"
	assert.NotEqual(messageTokenFingerprintTwin(rows),
		messageTokenFingerprintTwin([]db.Message{changed, rows[1]}),
	)
	assert.Equal(`reasoning_effort "high" -> "medium"`,
		messageMetadataDiff(rows[0], changed))
}
