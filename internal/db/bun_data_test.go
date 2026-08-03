package db

import (
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/sqlitedialect"
	"go.kenn.io/agentsview/internal/db/bunmodel"
)

func TestBunProjectRuleSessionsHydrateOnlyEnabledArchiveMachineScopes(t *testing.T) {
	raw, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	raw.SetMaxOpenConns(1)
	t.Cleanup(func() { require.NoError(t, raw.Close()) })
	store := bun.NewDB(raw, sqlitedialect.New())
	require.NoError(t, CreateCommonSchema(t.Context(), store))
	for _, archive := range []bunmodel.SourceArchive{
		{SourceArchiveID: "archive-a", SourceArchiveSalt: "salt-a"},
		{SourceArchiveID: "archive-b", SourceArchiveSalt: "salt-b"},
	} {
		_, err = store.NewInsert().Model(&archive).Exec(t.Context())
		require.NoError(t, err)
	}
	now := bunmodel.NewTimestamp(time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC))
	_, err = store.NewInsert().Model(&bunmodel.SourceWorktreeProjectMapping{
		SourceArchiveID: "archive-a", Machine: "wanted-machine",
		PathPrefix: "/wanted", Layout: WorktreeMappingLayoutExplicit,
		Project: "wanted", Enabled: true, CreatedAt: now, UpdatedAt: now,
	}).Exec(t.Context())
	require.NoError(t, err)

	sessions := []bunmodel.Session{
		{
			ID: "wanted", Project: "wanted", Machine: "wanted-machine",
			Agent: "codex", Cwd: "/wanted/repo", CreatedAt: now,
			SourceArchiveID: "archive-a", SourceDatabaseGeneration: "generation-a",
		},
		{
			ID: "wrong-archive", Project: "other", Machine: "wanted-machine",
			Agent: "codex", Cwd: "/wanted/repo", CreatedAt: now,
			SourceArchiveID: "archive-b", SourceDatabaseGeneration: "generation-b",
		},
	}
	for i := range 500 {
		sessions = append(sessions, bunmodel.Session{
			ID: fmt.Sprintf("unrelated-%03d", i), Project: "unrelated",
			Machine: "unrelated-machine", Agent: "codex", Cwd: "/elsewhere",
			CreatedAt: now, SourceArchiveID: "archive-a",
			SourceDatabaseGeneration: "generation-a",
		})
	}
	_, err = store.NewInsert().Model(&sessions).Exec(t.Context())
	require.NoError(t, err)

	machine := "wanted-machine"
	rows, err := listBunGovernanceSessions(t.Context(), store, &machine)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "wanted", rows[0].ID)
	assert.Equal(t, "archive-a", rows[0].SourceArchiveID)
}

func TestSQLiteConsistentViewKeepsOneReadSnapshot(t *testing.T) {
	database := testDB(t)
	backend := &sqliteBunBackend{store: database}
	archiveID, err := database.GetArchiveID(t.Context())
	require.NoError(t, err)
	databaseID, err := database.GetDatabaseID(t.Context())
	require.NoError(t, err)
	inserted := make(chan error, 1)

	err = backend.ConsistentView(t.Context(), func(store bun.IDB) error {
		var before int
		if err := store.NewSelect().Table("sessions").ColumnExpr("COUNT(*)").
			Scan(t.Context(), &before); err != nil {
			return err
		}
		go func() {
			_, insertErr := database.getWriter().ExecContext(t.Context(), `
				INSERT INTO sessions (
					id, project, machine, agent, created_at,
					source_archive_id, source_database_generation
				) VALUES ('concurrent', 'p', 'm', 'a',
					'2026-08-03T12:00:00Z', ?, ?)`,
				archiveID, databaseID,
			)
			inserted <- insertErr
		}()
		var after int
		if err := store.NewSelect().Table("sessions").ColumnExpr("COUNT(*)").
			Scan(t.Context(), &after); err != nil {
			return err
		}
		assert.Equal(t, before, after)
		return nil
	})
	require.NoError(t, err)
	require.NoError(t, <-inserted)
}
