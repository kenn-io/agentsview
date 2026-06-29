package sync_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/sync"
)

// TestProviderAuthoritativeUnchangedSessionSkipsOnResync verifies that a
// provider-authoritative agent whose source file is unchanged is skipped on a
// second full sync rather than reparsed and rewritten. Before the generic
// providerSourceUnchangedInDB freshness check, only Claude and Cowork had a
// pre-parse DB skip in processProviderFile, so the other migrated agents
// (OpenHands, Cursor, Hermes, Vibe) fell through to provider.Parse + writeBatch
// and rewrote unchanged sessions on every full/periodic sync. Vibe is used as a
// representative of that group.
func TestProviderAuthoritativeUnchangedSessionSkipsOnResync(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	vibeDir := t.TempDir()
	testDB := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(testDB, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentVibe: {vibeDir},
		},
		Machine: "local",
	})

	sessionID := "abc123def-0000-0000-0000-000000000000"
	writeVibeSyncFixture(
		t, vibeDir, "session_20260616_083518_abc123", sessionID, "Title",
	)

	ctx := context.Background()
	first := engine.SyncAll(ctx, nil)
	require.Equal(t, 1, first.Synced, "first sync parses and stores the session")

	// Source files are untouched, so the second full sync must skip the session
	// at the DB-freshness check instead of reparsing and rewriting it.
	second := engine.SyncAll(ctx, nil)
	assert.Equal(t, 0, second.Synced,
		"an unchanged provider-authoritative session must not be re-synced")
	assert.GreaterOrEqual(t, second.Skipped, 1,
		"the unchanged session must be counted as skipped")
}
