package sync_test

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/agentsview/internal/testjsonl"
)

// Parser version 147 did not read the summary store, so an archive it wrote
// holds Antigravity sessions with no parent although their sources are
// unchanged. Remote mirrors in that state see no change at all. The scoped
// upgrade must reparse those sources in place.
func TestAntigravitySummaryUpgradeReparsesUnchangedSources(t *testing.T) {
	const previousVersion = 147
	for name, tc := range antigravitySummaryCases() {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			tc.write(t, root)
			testjsonl.WriteAntigravitySummaries(t, root, map[string]testjsonl.AntigravitySummary{
				tc.id: {WorkspaceURIs: `["file:///tmp/ws-proj"]`, ParentID: antigravitySummaryParent},
			})
			id := string(tc.agent) + ":" + tc.id
			wantParent := string(tc.agent) + ":" + antigravitySummaryParent

			archivePath := filepath.Join(t.TempDir(), "archive.db")
			database, err := db.Open(t.Context(), archivePath)
			require.NoError(t, err)
			config := sync.EngineConfig{AgentDirs: map[parser.AgentType][]string{tc.agent: {root}}, Machine: "local"}
			engine := sync.NewEngine(t.Context(), database, config)
			require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
			before, err := database.GetSessionFull(t.Context(), id)
			require.NoError(t, err)
			require.Equal(t, &wantParent, before.ParentSessionID)
			hashBefore, ok := database.GetSessionFileHash(t.Context(), id)
			require.True(t, ok)
			engine.Close()
			require.NoError(t, database.Close())

			// Model the archive version 147 wrote: no parent, same file hash.
			archive, err := sql.Open("sqlite3", archivePath)
			require.NoError(t, err)
			_, err = archive.ExecContext(t.Context(), `UPDATE sessions SET parent_session_id = NULL,
				parser_parent_session_id = NULL, relationship_type = '', parser_relationship_type = NULL,
				data_version = ? WHERE id = ?`, previousVersion, id)
			require.NoError(t, err)
			_, err = archive.ExecContext(t.Context(), "PRAGMA user_version = 147")
			require.NoError(t, err)
			require.NoError(t, archive.Close())

			assert.Equal(t,
				[]parser.AgentType{parser.AgentAntigravity, parser.AgentAntigravityCLI},
				db.ScopedDataVersionProviders(previousVersion, previousVersion+1),
				"the upgrade must reparse only Antigravity sources in place")
			reopened, err := db.Open(t.Context(), archivePath)
			require.NoError(t, err)
			defer func() { require.NoError(t, reopened.Close()) }()
			upgraded := sync.NewEngine(t.Context(), reopened, config)
			defer upgraded.Close()
			require.Equal(t, 1, upgraded.SyncAll(t.Context(), nil).Synced,
				"the version bump must bypass freshness checks on unchanged sources")

			after, err := reopened.GetSessionFull(t.Context(), id)
			require.NoError(t, err)
			assert.Equal(t, &wantParent, after.ParentSessionID)
			assert.Equal(t, before.ID, after.ID)
			assert.Equal(t, before.FilePath, after.FilePath)
			hashAfter, ok := reopened.GetSessionFileHash(t.Context(), id)
			require.True(t, ok)
			assert.Equal(t, hashBefore, hashAfter)
			assert.Equal(t, db.CurrentDataVersion(), reopened.GetSessionDataVersion(t.Context(), id))
			counter, err := sql.Open("sqlite3", "file:"+archivePath+"?mode=ro")
			require.NoError(t, err)
			defer counter.Close()
			var sessions int
			require.NoError(t, counter.QueryRowContext(t.Context(), "SELECT count(*) FROM sessions").Scan(&sessions))
			assert.Equal(t, 1, sessions, "the upgrade must not create a duplicate session")
		})
	}
}
