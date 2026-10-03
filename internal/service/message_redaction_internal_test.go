package service

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

func TestMalformedNativeContextStillMasksCanonicalSources(t *testing.T) {
	message := db.Message{
		Content: "key AKIA7QHWN2DKR4FYPLJM", ThinkingText: "plan AKIA7QHWN2DKR4FYPLJM",
		ToolResultText: "output AKIA7QHWN2DKR4FYPLJM",
		ContentLayout:  &parser.ContentLayout{Version: 2, Blocks: []parser.ContentBlock{}},
	}
	masked := redactMessageSecrets(message)
	assert.Equal(t, "key AKIA…PLJM", masked.Content)
	assert.Equal(t, "plan AKIA…PLJM", masked.ThinkingText)
	assert.Equal(t, "output AKIA…PLJM", masked.ToolResultText)
	require.NotNil(t, masked.ContentLayout)
	assert.Equal(t, 2, masked.ContentLayout.Version)
}
