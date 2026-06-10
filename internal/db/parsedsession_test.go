package db_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

func TestParsedSessionName(t *testing.T) {
	t.Run("no name extracted returns nil", func(t *testing.T) {
		name := db.ParsedSessionName(parser.ParsedSession{})
		require.Nil(t, name)
	})
	t.Run("empty SessionName returns nil", func(t *testing.T) {
		name := db.ParsedSessionName(parser.ParsedSession{SessionName: ""})
		require.Nil(t, name)
	})
	t.Run("non-empty SessionName returns pointer", func(t *testing.T) {
		name := db.ParsedSessionName(parser.ParsedSession{SessionName: "My Session"})
		require.NotNil(t, name)
		assert.Equal(t, "My Session", *name)
	})
}
