package storetest

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/db/bunmodel"
)

const (
	mutationArchiveSalt       = "mutation-contract-salt"
	mutationGeneration        = "mutation-contract-generation"
	mutationVibeFallbackID    = "vibe:session_20260803_contract"
	mutationAdditionalAliasID = "mutation-delete-additional-alias"
)

// MutationStore is the Task 6 shared session-management method family.
type MutationStore interface {
	GetSession(context.Context, string) (*db.Session, error)
	GetSessionFull(context.Context, string) (*db.Session, error)
	RenameSession(string, *string) error
	SoftDeleteSession(string) error
	SoftDeleteSessions([]string) (int, error)
	RestoreSession(string) (int64, error)
	DeleteSessionIfTrashed(string) (int64, error)
	ListTrashedSessions(context.Context) ([]db.Session, error)
	EmptyTrash() (int, error)
}

// MutationFixture identifies the literal rows used by the session mutation
// contract. Each behavior opens a fresh fixture so mutations cannot mask one
// another.
type MutationFixture struct {
	Rename              string
	SoftActive          string
	SoftSourceMissing   string
	BatchActive         string
	BatchSourceMissing  string
	BatchAlreadyTrashed string
	RestoreUserTrash    string
	RestoreSource       string
	DeleteCanonical     string
	DeleteAlias         string
	DeleteVibe          string
	EmptyA              string
	EmptyB              string
	ListNewest          string
	ListOlder           string
	ListSourceMissing   string
	Keep                string
}

// MutationHarness exposes persisted boundary state that is intentionally not
// part of db.Store, such as exclusion rows and adapter-owned metadata.
type MutationHarness struct {
	Store MutationStore
	Rows  MutationFixture

	IsExcluded             func(*testing.T, string) bool
	RestoreBaselinePresent func(*testing.T, string) bool
	OperationalTouchAfter  func(*testing.T, string) bool
}

// MutationBackend registers one embedded BunStore and its fixture setup.
// extraTrashRows is non-zero only for the 500-row list cap case.
type MutationBackend struct {
	Name     string
	Open     func(*testing.T, int) MutationHarness
	Writable bool
}

// RunMutationContract verifies the shared rename/trash/restore/delete behavior
// and operation-scoped write policy against one embedded BunStore.
func RunMutationContract(t *testing.T, backend MutationBackend) {
	t.Helper()

	t.Run(backend.Name+"/list_trash_filters_orders_and_caps", func(t *testing.T) {
		harness := backend.Open(t, 500)
		trashed, err := harness.Store.ListTrashedSessions(t.Context())
		require.NoError(t, err)
		require.Len(t, trashed, 500)
		assert.Equal(t, harness.Rows.ListNewest, trashed[0].ID)
		assert.Equal(t, harness.Rows.ListOlder, trashed[1].ID)
		assert.NotContains(t, mutationSessionIDs(trashed), harness.Rows.ListSourceMissing)
		assert.NotContains(t, mutationSessionIDs(trashed), harness.Rows.RestoreSource)
	})

	if !backend.Writable {
		t.Run(backend.Name+"/writes_are_rejected_without_mutation", func(t *testing.T) {
			harness := backend.Open(t, 0)
			name := "forbidden rename"
			require.ErrorIs(t, harness.Store.RenameSession(harness.Rows.Rename, &name), db.ErrReadOnly)
			require.ErrorIs(t, harness.Store.SoftDeleteSession(harness.Rows.SoftActive), db.ErrReadOnly)
			count, err := harness.Store.SoftDeleteSessions([]string{harness.Rows.BatchActive})
			assert.Zero(t, count)
			require.ErrorIs(t, err, db.ErrReadOnly)
			restored, err := harness.Store.RestoreSession(harness.Rows.RestoreUserTrash)
			assert.Zero(t, restored)
			require.ErrorIs(t, err, db.ErrReadOnly)
			deleted, err := harness.Store.DeleteSessionIfTrashed(harness.Rows.DeleteCanonical)
			assert.Zero(t, deleted)
			require.ErrorIs(t, err, db.ErrReadOnly)
			emptied, err := harness.Store.EmptyTrash()
			assert.Zero(t, emptied)
			require.ErrorIs(t, err, db.ErrReadOnly)

			rename, err := harness.Store.GetSession(t.Context(), harness.Rows.Rename)
			require.NoError(t, err)
			require.NotNil(t, rename)
			require.NotNil(t, rename.DisplayName)
			assert.Equal(t, "Agent-provided title", *rename.DisplayName)
			active, err := harness.Store.GetSession(t.Context(), harness.Rows.SoftActive)
			require.NoError(t, err)
			require.NotNil(t, active)
		})
		return
	}

	t.Run(backend.Name+"/rename_and_clear", func(t *testing.T) {
		harness := backend.Open(t, 0)
		name := "User-visible title"
		require.NoError(t, harness.Store.RenameSession(harness.Rows.Rename, &name))
		renamed, err := harness.Store.GetSessionFull(t.Context(), harness.Rows.Rename)
		require.NoError(t, err)
		require.NotNil(t, renamed)
		require.NotNil(t, renamed.DisplayName)
		assert.Equal(t, name, *renamed.DisplayName)
		assert.NotNil(t, renamed.LocalModifiedAt)
		if harness.OperationalTouchAfter != nil {
			assert.True(t, harness.OperationalTouchAfter(t, harness.Rows.Rename))
		}

		require.NoError(t, harness.Store.RenameSession(harness.Rows.Rename, nil))
		cleared, err := harness.Store.GetSession(t.Context(), harness.Rows.Rename)
		require.NoError(t, err)
		require.NotNil(t, cleared)
		require.NotNil(t, cleared.DisplayName)
		assert.Equal(t, "Agent-provided title", *cleared.DisplayName)
	})

	t.Run(backend.Name+"/soft_delete_converts_source_tombstones", func(t *testing.T) {
		harness := backend.Open(t, 0)
		require.NoError(t, harness.Store.SoftDeleteSession(harness.Rows.SoftActive))
		require.NoError(t, harness.Store.SoftDeleteSession(harness.Rows.SoftSourceMissing))
		count, err := harness.Store.SoftDeleteSessions([]string{
			harness.Rows.BatchActive,
			harness.Rows.BatchSourceMissing,
			harness.Rows.BatchAlreadyTrashed,
			"mutation-missing-session",
		})
		require.NoError(t, err)
		assert.Equal(t, 2, count)
		count, err = harness.Store.SoftDeleteSessions(nil)
		require.NoError(t, err)
		assert.Zero(t, count)

		for _, id := range []string{
			harness.Rows.SoftActive,
			harness.Rows.SoftSourceMissing,
			harness.Rows.BatchActive,
			harness.Rows.BatchSourceMissing,
		} {
			visible, readErr := harness.Store.GetSession(t.Context(), id)
			require.NoError(t, readErr)
			assert.Nil(t, visible)
			full, readErr := harness.Store.GetSessionFull(t.Context(), id)
			require.NoError(t, readErr)
			require.NotNil(t, full)
			assert.NotNil(t, full.DeletedAt)
			assert.Nil(t, full.DeletionCause)
		}
	})

	t.Run(backend.Name+"/restore_refuses_source_tombstones", func(t *testing.T) {
		harness := backend.Open(t, 0)
		restored, err := harness.Store.RestoreSession(harness.Rows.RestoreSource)
		require.NoError(t, err)
		assert.Zero(t, restored)
		restored, err = harness.Store.RestoreSession(harness.Rows.RestoreUserTrash)
		require.NoError(t, err)
		assert.EqualValues(t, 1, restored)
		visible, err := harness.Store.GetSession(t.Context(), harness.Rows.RestoreUserTrash)
		require.NoError(t, err)
		require.NotNil(t, visible)
		if harness.RestoreBaselinePresent != nil {
			assert.False(t, harness.RestoreBaselinePresent(t, harness.Rows.RestoreUserTrash))
		}
	})

	t.Run(backend.Name+"/permanent_delete_excludes_canonical_and_alias_ids", func(t *testing.T) {
		harness := backend.Open(t, 0)
		deleted, err := harness.Store.DeleteSessionIfTrashed(harness.Rows.Keep)
		require.NoError(t, err)
		assert.Zero(t, deleted)
		deleted, err = harness.Store.DeleteSessionIfTrashed(harness.Rows.RestoreSource)
		require.NoError(t, err)
		assert.Zero(t, deleted)

		deleted, err = harness.Store.DeleteSessionIfTrashed(harness.Rows.DeleteCanonical)
		require.NoError(t, err)
		assert.EqualValues(t, 1, deleted)
		for _, id := range []string{
			harness.Rows.DeleteCanonical,
			harness.Rows.DeleteAlias,
			mutationAdditionalAliasID,
		} {
			assert.True(t, harness.IsExcluded(t, id), "excluded id %s", id)
		}
		aliasRow, err := harness.Store.GetSessionFull(t.Context(), harness.Rows.DeleteAlias)
		require.NoError(t, err)
		assert.Nil(t, aliasRow)

		deleted, err = harness.Store.DeleteSessionIfTrashed(harness.Rows.DeleteVibe)
		require.NoError(t, err)
		assert.EqualValues(t, 1, deleted)
		assert.True(t, harness.IsExcluded(t, harness.Rows.DeleteVibe))
		assert.True(t, harness.IsExcluded(t, mutationVibeFallbackID))
	})

	t.Run(backend.Name+"/empty_trash_preserves_source_tombstones", func(t *testing.T) {
		harness := backend.Open(t, 0)
		count, err := harness.Store.EmptyTrash()
		require.NoError(t, err)
		assert.Equal(t, 8, count)
		trashed, err := harness.Store.ListTrashedSessions(t.Context())
		require.NoError(t, err)
		assert.Empty(t, trashed)
		sourceRow, err := harness.Store.GetSessionFull(t.Context(), harness.Rows.RestoreSource)
		require.NoError(t, err)
		require.NotNil(t, sourceRow)
		require.NotNil(t, sourceRow.DeletionCause)
		assert.Equal(t, "source_missing", *sourceRow.DeletionCause)
		kept, err := harness.Store.GetSession(t.Context(), harness.Rows.Keep)
		require.NoError(t, err)
		require.NotNil(t, kept)
		assert.True(t, harness.IsExcluded(t, harness.Rows.EmptyA))
		assert.True(t, harness.IsExcluded(t, harness.Rows.EmptyB))
	})
}

// InsertBunMutationFixture inserts canonical rows for generated PostgreSQL and
// DuckDB schemas.
func InsertBunMutationFixture(
	ctx context.Context,
	store bun.IDB,
	archiveID string,
	generation string,
	extraTrashRows int,
) (MutationFixture, error) {
	if _, err := store.NewInsert().Model(&bunmodel.SourceArchive{
		SourceArchiveID: archiveID, SourceArchiveSalt: mutationArchiveSalt,
	}).Exec(ctx); err != nil {
		return MutationFixture{}, fmt.Errorf("inserting mutation source archive: %w", err)
	}
	fixture, rows := mutationFixtureRows(archiveID, generation, extraTrashRows)
	for index := range rows {
		if _, err := store.NewInsert().Model(&rows[index]).Exec(ctx); err != nil {
			return MutationFixture{}, fmt.Errorf("inserting mutation session %s: %w", rows[index].ID, err)
		}
	}
	created := mutationTimestamp("2000-01-01T00:00:00Z")
	aliases := []bunmodel.SessionAlias{
		{SessionID: fixture.DeleteCanonical, AliasID: fixture.DeleteAlias, CreatedAt: created},
		{SessionID: fixture.DeleteCanonical, AliasID: mutationAdditionalAliasID, CreatedAt: created},
	}
	if _, err := store.NewInsert().Model(&aliases).Exec(ctx); err != nil {
		return MutationFixture{}, fmt.Errorf("inserting mutation aliases: %w", err)
	}
	return fixture, nil
}

// InsertSQLiteMutationFixture inserts the same rows through SQLite's shipped
// archive schema and seeds its local restore-baseline side effect.
func InsertSQLiteMutationFixture(
	ctx context.Context,
	tx *sql.Tx,
	archiveID string,
	generation string,
	extraTrashRows int,
) (MutationFixture, error) {
	fixture, rows := mutationFixtureRows(archiveID, generation, extraTrashRows)
	for index := range rows {
		row := rows[index]
		_, err := tx.ExecContext(ctx, `
			INSERT INTO sessions (
				id, project, machine, agent, display_name, session_name,
				created_at, deleted_at, deletion_cause, file_path,
				source_archive_id, source_database_generation
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			row.ID, row.Project, row.Machine, row.Agent, row.DisplayName,
			row.SessionName, mutationTimestampValue(row.CreatedAt),
			mutationTimestampPointerValue(row.DeletedAt), row.DeletionCause,
			row.FilePath, archiveID, generation,
		)
		if err != nil {
			return MutationFixture{}, fmt.Errorf("inserting SQLite mutation session %s: %w", row.ID, err)
		}
	}
	for _, aliasID := range []string{fixture.DeleteAlias, mutationAdditionalAliasID} {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO session_aliases (session_id, alias_id, created_at)
			VALUES (?, ?, ?)`,
			fixture.DeleteCanonical, aliasID, "2000-01-01T00:00:00Z",
		); err != nil {
			return MutationFixture{}, fmt.Errorf("inserting SQLite mutation alias %s: %w", aliasID, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO local_session_source_baselines
			(session_id, machine, agent, file_path)
		VALUES (?, 'mutation-machine', 'claude', '/tmp/restore-user.jsonl')`,
		fixture.RestoreUserTrash,
	); err != nil {
		return MutationFixture{}, fmt.Errorf("inserting SQLite restore baseline: %w", err)
	}
	return fixture, nil
}

func mutationFixtureRows(
	archiveID string, generation string, extraTrashRows int,
) (MutationFixture, []bunmodel.Session) {
	fixture := MutationFixture{
		Rename:              "mutation-rename",
		SoftActive:          "mutation-soft-active",
		SoftSourceMissing:   "mutation-soft-source",
		BatchActive:         "mutation-batch-active",
		BatchSourceMissing:  "mutation-batch-source",
		BatchAlreadyTrashed: "mutation-batch-trashed",
		RestoreUserTrash:    "mutation-restore-user",
		RestoreSource:       "mutation-restore-source",
		DeleteCanonical:     "mutation-delete-canonical",
		DeleteAlias:         "mutation-delete-alias",
		DeleteVibe:          "vibe:00000000-0000-0000-0000-000000000001",
		EmptyA:              "mutation-empty-a",
		EmptyB:              "mutation-empty-b",
		ListNewest:          "mutation-list-newest",
		ListOlder:           "mutation-list-older",
		ListSourceMissing:   "mutation-list-source",
		Keep:                "mutation-keep",
	}
	sourceMissing := "source_missing"
	userTrash := mutationTimestampPointer("2026-08-03T10:00:00Z")
	sourceTrash := mutationTimestampPointer("2200-01-01T00:00:00Z")
	agentTitle := "Agent-provided title"
	rows := []bunmodel.Session{
		mutationSessionRow(fixture.Rename, archiveID, generation, nil, nil),
		mutationSessionRow(fixture.SoftActive, archiveID, generation, nil, nil),
		mutationSessionRow(fixture.SoftSourceMissing, archiveID, generation, sourceTrash, &sourceMissing),
		mutationSessionRow(fixture.BatchActive, archiveID, generation, nil, nil),
		mutationSessionRow(fixture.BatchSourceMissing, archiveID, generation, sourceTrash, &sourceMissing),
		mutationSessionRow(fixture.BatchAlreadyTrashed, archiveID, generation, userTrash, nil),
		mutationSessionRow(fixture.RestoreUserTrash, archiveID, generation, userTrash, nil),
		mutationSessionRow(fixture.RestoreSource, archiveID, generation, sourceTrash, &sourceMissing),
		mutationSessionRow(fixture.DeleteCanonical, archiveID, generation, userTrash, nil),
		mutationSessionRow(fixture.DeleteAlias, archiveID, generation, nil, nil),
		mutationSessionRow(fixture.DeleteVibe, archiveID, generation, userTrash, nil),
		mutationSessionRow(fixture.EmptyA, archiveID, generation, userTrash, nil),
		mutationSessionRow(fixture.EmptyB, archiveID, generation, userTrash, nil),
		mutationSessionRow(fixture.ListNewest, archiveID, generation, mutationTimestampPointer("2100-01-01T00:00:00Z"), nil),
		mutationSessionRow(fixture.ListOlder, archiveID, generation, mutationTimestampPointer("2099-01-01T00:00:00Z"), nil),
		mutationSessionRow(fixture.ListSourceMissing, archiveID, generation, sourceTrash, &sourceMissing),
		mutationSessionRow(fixture.Keep, archiveID, generation, nil, nil),
	}
	rows[0].SessionName = &agentTitle
	rows[10].Agent = "vibe"
	vibePath := "/tmp/vibe/session_20260803_contract/messages.jsonl"
	rows[10].FilePath = &vibePath
	restorePath := "/tmp/restore-user.jsonl"
	rows[6].FilePath = &restorePath

	for index := range extraTrashRows {
		id := fmt.Sprintf("mutation-extra-%03d", index)
		deletedAt := bunmodel.NewTimestamp(time.Date(2000, 1, 1, 0, index, 0, 0, time.UTC))
		rows = append(rows, mutationSessionRow(
			id, archiveID, generation, &deletedAt, nil,
		))
	}
	return fixture, rows
}

func mutationSessionRow(
	id string,
	archiveID string,
	generation string,
	deletedAt *bunmodel.Timestamp,
	deletionCause *string,
) bunmodel.Session {
	return bunmodel.Session{
		ID: id, Project: "mutation-project", Machine: "mutation-machine",
		Agent: "claude", CreatedAt: mutationTimestamp("2000-01-01T00:00:00Z"),
		DeletedAt: deletedAt, DeletionCause: deletionCause,
		SourceArchiveID: archiveID, SourceDatabaseGeneration: generation,
	}
}

func mutationTimestamp(value string) bunmodel.Timestamp {
	parsed, err := bunmodel.ParseTimestamp(value)
	if err != nil {
		panic(err)
	}
	return parsed
}

func mutationTimestampPointer(value string) *bunmodel.Timestamp {
	parsed := mutationTimestamp(value)
	return &parsed
}

func mutationTimestampValue(value bunmodel.Timestamp) string {
	return value.Time.UTC().Format(time.RFC3339Nano)
}

func mutationTimestampPointerValue(value *bunmodel.Timestamp) any {
	if value == nil {
		return nil
	}
	return mutationTimestampValue(*value)
}

func mutationSessionIDs(rows []db.Session) []string {
	ids := make([]string, len(rows))
	for index := range rows {
		ids[index] = rows[index].ID
	}
	return ids
}
