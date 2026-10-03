package db_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
)

func TestNativeSearchSources(t *testing.T) {
	archive := dbtest.OpenTestDB(t)
	dbtest.SeedNativeSearch(t, archive)
	dbtest.CheckNativeSearch(t, archive, true)
}

func TestNativeSearchMidThinkingWindowKeepsSourceAndSecretBoundary(t *testing.T) {
	archive := dbtest.OpenTestDB(t)
	thinking := "-----BEGIN RSA PRIVATE KEY-----\n" +
		strings.Repeat("PRIVATEKEYMATERIAL0123456789\n", 10) + "middleneedle\n" +
		strings.Repeat("PRIVATEKEYMATERIAL0123456789\n", 10) +
		"-----END RSA PRIVATE KEY-----"
	dbtest.SeedSessionWithMessages(t, archive, "long-thinking", "project", []db.Message{
		{SessionID: "long-thinking", Ordinal: 0, Role: "user", Content: "question"},
		{SessionID: "long-thinking", Ordinal: 1, Role: "assistant", Content: "answer", ThinkingText: thinking, HasThinking: true},
	}, dbtest.WithMessageCounts(2, 2))
	for _, mode := range []string{"substring", "regex", "fts", "terms"} {
		page, err := archive.SearchContent(t.Context(), db.ContentSearchFilter{
			SessionID: "long-thinking", Pattern: "middleneedle", Mode: mode, Sources: []string{"messages"},
		})
		require.NoError(t, err)
		assert.Empty(t, page.Matches, mode)
	}
	for _, mode := range []string{"substring", "regex"} {
		filter := db.ContentSearchFilter{SessionID: "long-thinking", Pattern: "middleneedle", Mode: mode, Sources: []string{"thinking"}}
		page, err := archive.SearchContent(t.Context(), filter)
		require.NoError(t, err)
		require.Len(t, page.Matches, 1)
		assert.Equal(t, "thinking", page.Matches[0].Location)
		assert.Contains(t, page.Matches[0].Snippet, "[redacted private key block]")
		assert.NotContains(t, page.Matches[0].Snippet, "PRIVATEKEYMATERIAL")
		filter.RevealSecrets = true
		page, err = archive.SearchContent(t.Context(), filter)
		require.NoError(t, err)
		require.Len(t, page.Matches, 1)
		assert.Contains(t, page.Matches[0].Snippet, "middleneedle")
		assert.Contains(t, page.Matches[0].Snippet, "PRIVATEKEYMATERIAL")
		assert.NotContains(t, page.Matches[0].Snippet, "BEGIN RSA", "fixture proves the window omits the delimiter")
	}
}

func TestNativeDialogueUnitRangesExcludeWorkAndLegacy(t *testing.T) {
	archive := dbtest.OpenTestDB(t)
	dbtest.SeedNativeSearch(t, archive)
	for _, source := range []struct {
		name, needle string
		ordinal      int
	}{
		{"messages", "dialogueneedle", 1}, {"thinking", "reasononlyneedle", 7},
	} {
		page, err := archive.SearchContent(t.Context(), db.ContentSearchFilter{
			SessionID: "native-search", Pattern: source.needle, Sources: []string{source.name},
		})
		require.NoError(t, err)
		require.Len(t, page.Matches, 1)
		assert.Equal(t, [2]int{source.ordinal, source.ordinal}, page.Matches[0].OrdinalRange)
	}
}
