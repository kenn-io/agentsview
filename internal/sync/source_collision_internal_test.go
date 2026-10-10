package sync

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

// A result moved to a derived id keeps its retry flag, so an incomplete
// parse is not stamped current under the new id.
func TestSourceCollisionKeepsRetryFlag(t *testing.T) {
	root := t.TempDir()
	chats := filepath.Join(root, "tmp", "hash", "chats")
	require.NoError(t, os.MkdirAll(chats, 0o755))
	owner := filepath.Join(chats, "session-2026-01-01T09-00-owner.json")
	other := filepath.Join(chats, "session-2026-01-01T10-00-other.json")
	for _, path := range []string{owner, other} {
		require.NoError(t, os.WriteFile(path, []byte("{}"), 0o644))
	}
	database := openTestDB(t)
	const id = "gemini:shared"
	require.NoError(t, database.UpsertSession(t.Context(), db.Session{
		ID: id, Project: "p", Machine: "local", Agent: string(parser.AgentGemini), FilePath: &owner,
	}))
	provider, ok := parser.NewProvider(parser.AgentGemini, parser.ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	e := NewEngine(t.Context(), database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentGemini: {root}}, Machine: "local",
	})
	t.Cleanup(e.Close)

	res := processResult{
		results: []parser.ParseResult{{Session: parser.ParsedSession{
			ID: id, Agent: parser.AgentGemini, File: parser.FileInfo{Path: other},
			ParentSessionID: "gemini:spawner", RelationshipType: parser.RelSubagent,
		}}},
		retrySessionIDs: map[string]bool{id: true},
	}
	e.applyProviderFilePathPolicies(t.Context(), provider, parser.AgentGemini, other, &res, e.idPrefix)

	require.Len(t, res.results, 1)
	altID := parser.AltSessionID(id, other)
	assert.Equal(t, altID, res.results[0].Session.ID)
	assert.True(t, res.needsRetryForSession(altID))
	// The derived session links to the session it shares an id with, even
	// when its parser recorded another parent.
	assert.Equal(t, id, res.results[0].Session.ParentSessionID)
	assert.Equal(t, parser.RelContinuation, res.results[0].Session.RelationshipType)

	t.Run("local moved owner precedes exact alternate", func(t *testing.T) {
		cursorRoot := t.TempDir()
		stored := filepath.Join(cursorRoot, "project-a", "agent-transcripts", "shared.txt")
		incoming := filepath.Join(cursorRoot, "project-a", "agent-transcripts", "shared", "shared.jsonl")
		require.NoError(t, os.MkdirAll(filepath.Dir(incoming), 0o755))
		require.NoError(t, os.WriteFile(incoming, []byte(`{"role":"user","content":"hello"}`), 0o644))
		const baseID = "cursor:shared"
		alt := parser.AltSessionID(baseID, incoming)
		for id, path := range map[string]string{baseID: stored, alt: incoming} {
			require.NoError(t, database.UpsertSession(t.Context(), db.Session{
				ID: id, Project: "project-a", Machine: "local", Agent: "cursor", FilePath: &path,
			}))
		}
		cursor, ok := parser.NewProvider(parser.AgentCursor, parser.ProviderConfig{Roots: []string{cursorRoot}})
		require.True(t, ok)
		session := parser.ParsedSession{ID: baseID, Agent: parser.AgentCursor}
		id, moved, err := e.sourceCollisionID(t.Context(), cursor, incoming, &session, true, "")
		require.NoError(t, err)
		assert.Equal(t, baseID, id)
		assert.True(t, moved)
	})
}

// A failed ownership lookup skips the source this pass so it retries, rather
// than treating the id as unowned.
func TestSourceCollisionLookupErrorSkipsSource(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "tmp", "hash", "chats", "session-2026-01-01T10-00-a.json")
	provider, ok := parser.NewProvider(parser.AgentGemini, parser.ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	e := NewEngine(t.Context(), openTestDB(t), EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentGemini: {root}}, Machine: "local",
	})
	t.Cleanup(e.Close)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	res := processResult{results: []parser.ParseResult{{Session: parser.ParsedSession{
		ID: "gemini:shared", Agent: parser.AgentGemini, File: parser.FileInfo{Path: path},
	}}}}
	e.applyProviderFilePathPolicies(ctx, provider, parser.AgentGemini, path, &res, e.idPrefix)

	require.Error(t, res.err)
	assert.Contains(t, res.err.Error(), "session path records")
	assert.True(t, res.noCacheSkip)
	assert.Empty(t, res.results)
}

// Every sync pass, watcher-driven ones included, starts without the previous
// pass's claims, so a claim whose write never landed can't push a file onto a
// derived id.
func TestChangedPathSyncResetsSourceClaims(t *testing.T) {
	root := t.TempDir()
	chats := filepath.Join(root, "tmp", "hash", "chats")
	require.NoError(t, os.MkdirAll(chats, 0o755))
	path := filepath.Join(chats, "session-2026-01-01T10-00-a.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"sessionId":"shared","projectHash":"hash","startTime":"2024-01-01T10:00:00Z","lastUpdated":"2024-01-01T10:00:05Z","messages":[{"id":"m1","timestamp":"2024-01-01T10:00:00Z","type":"user","content":"hi"}]}`), 0o644))
	database := openTestDB(t)
	e := NewEngine(t.Context(), database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentGemini: {root}}, Machine: "local",
	})
	t.Cleanup(e.Close)
	stale := filepath.Join(chats, "session-2026-01-01T09-00-stale.json")
	require.NoError(t, os.WriteFile(stale, []byte("{}"), 0o644))
	e.sourceClaims = map[string]string{"gemini:shared": stale}

	require.NoError(t, e.SyncPathsContext(t.Context(), []string{path}))

	stored, err := database.GetSessionFull(t.Context(), "gemini:shared")
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.Equal(t, path, *stored.FilePath)
}
