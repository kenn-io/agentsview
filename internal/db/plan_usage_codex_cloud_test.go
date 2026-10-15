package db_test

import (
	"database/sql"
	"encoding/json/jsontext"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/codexcloud/mirror"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/sync"
)

const unpricedChatID = "codex-cloud:thread-unpriced"

// writeUnpricedChat writes the mirror file of a Codex cloud chat that live
// capture never saw and that has no plan usage record yet, and returns the
// mirror root and the file's path.
func writeUnpricedChat(t *testing.T) (string, string) {
	t.Helper()
	fetched := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	record := func(kind mirror.Kind, data string) mirror.Record {
		return mirror.Record{Kind: kind, FetchedAt: fetched, Data: jsontext.Value(data)}
	}
	root := t.TempDir()
	path := filepath.Join(root, "account-key", "thread-unpriced.jsonl.zstd")
	_, err := mirror.Append(path, 0, []mirror.Record{
		record(mirror.KindThread,
			`{"id":"thread-unpriced","createdAt":1759320000,"updatedAt":1759320060,"model":"gpt-6","cwd":"/workspace/project-a"}`),
		record(mirror.KindItem,
			`{"turnId":"turn-1","startedAtMs":1759320000000,"item":{"type":"userMessage","id":"u1","content":[{"type":"text","text":"Fix the build"}]}}`),
		record(mirror.KindItem,
			`{"turnId":"turn-1","startedAtMs":1759320030000,"item":{"type":"agentMessage","id":"a1","text":"Fixed."}}`),
	})
	require.NoError(t, err)
	return root, path
}

func syncCodexCloud(t *testing.T, database *db.DB, root string) sync.SyncStats {
	t.Helper()
	engine := sync.NewEngine(t.Context(), database, sync.EngineConfig{
		Machine:   "local",
		AgentDirs: map[parser.AgentType][]string{parser.AgentCodexCloud: {root}},
	})
	defer engine.Close()
	return engine.SyncAll(t.Context(), nil)
}

// A chat with neither token counts nor a plan usage answer is left out of
// the token totals, so the "billed as plan usage" count must include it,
// and session details must say plan usage is not checked yet.
func TestUnpricedCodexCloudChatCountsAsPlanUsage(t *testing.T) {
	database := dbtest.OpenTestDB(t)
	root, _ := writeUnpricedChat(t)
	require.Equal(t, 1, syncCodexCloud(t, database, root).Synced)

	counts, err := database.GetPlanUsageSessionCounts(t.Context(), db.UsageFilter{})
	require.NoError(t, err)
	assert.Equal(t, 1, counts.PlanOnly)
	assert.Zero(t, counts.PartlyCaptured)

	row, err := database.GetSessionPlanUsage(t.Context(), unpricedChatID)
	require.NoError(t, err)
	assert.Equal(t, &db.PlanUsageView{Groups: []db.SessionPlanUsageGroup{}}, row.PlanUsageView(),
		"a view with no figures, outcome, or times reads as not checked yet")
	assert.Nil(t, row.CaptureCoverageView())
}

// The upgrade that adds the empty plan usage row reparses Codex cloud
// sources in place, keeping the session's identity.
func TestUnpricedChatUpgradeReparsesCodexCloudInPlace(t *testing.T) {
	scope := db.ScopedDataVersionProviders(138, 139)
	assert.Equal(t, []parser.AgentType{parser.AgentCodexCloud}, scope)
	version := db.CurrentDataVersion()
	t.Cleanup(db.InstallDataVersionScopesForTest(map[int][]parser.AgentType{version: scope}))
	ctx := t.Context()
	database := dbtest.OpenTestDB(t)
	root, source := writeUnpricedChat(t)
	require.Equal(t, 1, syncCodexCloud(t, database, root).Synced)

	// Reproduce an archive from the version before the upgrade, whose
	// parser wrote no plan usage row for this chat.
	require.NoError(t, database.Update(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, fmt.Sprintf(
			"DELETE FROM session_plan_usage; UPDATE sessions SET data_version=%d; PRAGMA user_version=%d",
			version-1, version-1))
		return err
	}))
	path := database.Path()
	require.NoError(t, database.Close())
	database, err := db.OpenIsolated(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	needsSync, fullRebuild, err := db.ArchiveResyncState(ctx, path)
	require.NoError(t, err)
	assert.True(t, needsSync)
	assert.False(t, fullRebuild, "the upgrade must not rebuild the archive")

	assert.Equal(t, 1, syncCodexCloud(t, database, root).Synced)
	counts, err := database.GetPlanUsageSessionCounts(ctx, db.UsageFilter{})
	require.NoError(t, err)
	assert.Equal(t, 1, counts.PlanOnly)
	session, err := database.GetSessionFull(ctx, unpricedChatID)
	require.NoError(t, err)
	require.NotNil(t, session)
	require.NotNil(t, session.FilePath)
	assert.Equal(t, source, *session.FilePath)
	assert.Equal(t, version, session.DataVersion)
	var sessions int
	require.NoError(t, database.Reader().QueryRow(ctx, "SELECT count(*) FROM sessions").Scan(&sessions))
	assert.Equal(t, 1, sessions, "the reparse must keep the existing session identity")
}
