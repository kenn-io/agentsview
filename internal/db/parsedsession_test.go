package db_test

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

func TestParsedSessionNameFields(t *testing.T) {
	t.Run("no name extracted returns nil nil", func(t *testing.T) {
		name, src := db.ParsedSessionNameFields(parser.ParsedSession{})
		require.Nil(t, name)
		require.Nil(t, src)
	})
	t.Run("empty DisplayName returns nil nil", func(t *testing.T) {
		name, src := db.ParsedSessionNameFields(parser.ParsedSession{DisplayName: ""})
		require.Nil(t, name)
		require.Nil(t, src)
	})
	t.Run("name extracted sets agent source", func(t *testing.T) {
		name, src := db.ParsedSessionNameFields(parser.ParsedSession{DisplayName: "My Session"})
		require.NotNil(t, name)
		require.Equal(t, "My Session", *name)
		require.NotNil(t, src)
		require.Equal(t, "agent", *src)
	})
}
