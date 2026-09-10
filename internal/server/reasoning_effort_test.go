package server

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
)

func TestReasoningEffortUploadConversion(t *testing.T) {
	result := sessionBatchWriteFromParsed(
		parser.ParsedSession{ID: "upload-session"},
		[]parser.ParsedMessage{{
			Ordinal:         0,
			Role:            parser.RoleAssistant,
			Content:         "answer",
			Model:           "model-test",
			ReasoningEffort: "high",
		}},
	)
	require.Len(t, result.Messages, 1)
	require.Equal(t, "high", result.Messages[0].ReasoningEffort)
}
