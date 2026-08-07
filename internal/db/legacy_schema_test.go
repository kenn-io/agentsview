package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/export"
)

func priorCommonSQLiteSchema() string {
	return strings.NewReplacer(
		"    source_archive_id TEXT NOT NULL DEFAULT '',\n"+
			"    source_database_generation TEXT NOT NULL DEFAULT '',\n", "",
		"    call_index INTEGER,\n    message_ordinal INTEGER\n",
		"    call_index INTEGER\n",
		"    ordinal     INTEGER NOT NULL,\n"+
			"    source_uuid TEXT NOT NULL DEFAULT '',\n",
		"    ordinal     INTEGER NOT NULL,\n",
		"CREATE TABLE IF NOT EXISTS pricing_metadata (\n"+
			"    key        TEXT PRIMARY KEY,\n"+
			"    value      TEXT NOT NULL,\n"+
			"    updated_at TEXT NOT NULL\n"+
			"        DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))\n"+
			");\n\n",
		"",
	).Replace(schemaSQL)
}

const priorCommonSQLiteRows = `
INSERT INTO sessions (
    id, project, machine, agent, first_message, message_count, created_at
) VALUES (
    'common-legacy-session', 'legacy-project', 'legacy-machine', 'claude',
    'named legacy session', 1, '2026-08-01T12:00:00Z'
);
INSERT INTO messages (
    id, session_id, ordinal, role, content, has_tool_use, content_length
) VALUES (
    41, 'common-legacy-session', 7, 'assistant', 'legacy answer', 1, 13
);
INSERT INTO tool_calls (
    id, message_id, session_id, tool_name, category, call_index
) VALUES (
    51, 41, 'common-legacy-session', 'Read', 'Read', 0
);
INSERT INTO pinned_messages (
    id, session_id, message_id, ordinal, note, created_at
) VALUES (
    61, 'common-legacy-session', 41, 7, 'keep this pin',
    '2026-08-01T12:01:00Z'
);
INSERT INTO project_identity_observations (
    session_id, project, machine, root_path, git_remote, observed_at,
    normalized_remote, key_source, key
) VALUES (
    'common-legacy-session', 'legacy-project', 'legacy-machine',
    '/work/legacy', 'https://example.invalid/legacy.git',
    '2026-08-01T12:02:00Z', 'example.invalid/legacy', 'git', 'legacy-key'
);
INSERT OR REPLACE INTO session_project_identity_snapshots (
    session_id, project, machine, root_path, git_remote, observed_at,
    normalized_remote, key_source, key
) VALUES (
    'common-legacy-session', 'legacy-project', 'legacy-machine',
    '/work/legacy', 'https://example.invalid/legacy.git',
    '2026-08-01T12:02:00Z', 'example.invalid/legacy', 'git', 'legacy-key'
);
INSERT INTO worktree_project_mappings (
    id, machine, path_prefix, layout, project, enabled
) VALUES (
    71, 'legacy-machine', '/work/legacy', 'explicit', 'legacy-project', 1
);
INSERT INTO model_pricing (
    model_pattern, input_microdollars_per_mtok,
    output_microdollars_per_mtok, updated_at
) VALUES
    ('_fallback_version', 0, 0, 'legacy-v42'),
    ('_private-model', 1250000, 2500000, '2026-08-05T12:00:00Z');`

func createPriorCommonSQLiteArchive(t *testing.T, path string) {
	t.Helper()
	conn, err := sql.Open("sqlite3", makeDSN(path, false))
	require.NoError(t, err)
	conn.SetMaxOpenConns(1)
	_, err = conn.Exec(priorCommonSQLiteSchema())
	require.NoError(t, err)
	_, err = conn.Exec(priorCommonSQLiteRows)
	require.NoError(t, err)
	_, err = conn.Exec(fmt.Sprintf("PRAGMA user_version = %d", dataVersion))
	require.NoError(t, err)
	require.NoError(t, conn.Close())
}

func TestLegacySchemaCommonConvergenceRetainsRowsAndBackfillsProvenance(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-common.db")
	createPriorCommonSQLiteArchive(t, path)

	database, err := Open(path)
	require.NoError(t, err)
	defer database.Close()

	session := requireSessionExists(t, database, "common-legacy-session")
	assert.Equal(t, "named legacy session", *session.FirstMessage)
	var archiveID, databaseGeneration string
	require.NoError(t, database.getReader().QueryRowContext(t.Context(), `
		SELECT source_archive_id, source_database_generation
		FROM sessions WHERE id = 'common-legacy-session'`,
	).Scan(&archiveID, &databaseGeneration))
	assert.NotEmpty(t, archiveID)
	assert.NotEmpty(t, databaseGeneration)

	var toolOrdinal, pinOrdinal int
	var pinNote, pinSourceUUID string
	require.NoError(t, database.getReader().QueryRowContext(t.Context(), `
		SELECT message_ordinal FROM tool_calls WHERE id = 51`,
	).Scan(&toolOrdinal))
	assert.Equal(t, 7, toolOrdinal)
	require.NoError(t, database.getReader().QueryRowContext(t.Context(), `
		SELECT ordinal, note, source_uuid FROM pinned_messages WHERE id = 61`,
	).Scan(&pinOrdinal, &pinNote, &pinSourceUUID))
	assert.Equal(t, 7, pinOrdinal)
	assert.Equal(t, "keep this pin", pinNote)
	assert.Empty(t, pinSourceUUID)

	var observationArchive, observationSalt string
	require.NoError(t, database.getReader().QueryRowContext(t.Context(), `
		SELECT source_archive_id, source_archive_salt
		FROM source_project_identity_observations
		WHERE project = 'legacy-project'`,
	).Scan(&observationArchive, &observationSalt))
	assert.Equal(t, archiveID, observationArchive)
	assert.NotEmpty(t, observationSalt)

	var snapshotArchive, snapshotGeneration string
	require.NoError(t, database.getReader().QueryRowContext(t.Context(), `
		SELECT source_archive_id, source_database_generation
		FROM source_session_project_identity_snapshots
		WHERE source_session_id = 'common-legacy-session'`,
	).Scan(&snapshotArchive, &snapshotGeneration))
	assert.Equal(t, archiveID, snapshotArchive)
	assert.Equal(t, databaseGeneration, snapshotGeneration)

	var mappingArchive string
	require.NoError(t, database.getReader().QueryRowContext(t.Context(), `
		SELECT source_archive_id FROM source_worktree_project_mappings
		WHERE machine = 'legacy-machine' AND path_prefix = '/work/legacy'`,
	).Scan(&mappingArchive))
	assert.Equal(t, archiveID, mappingArchive)

	var stamp string
	require.NoError(t, database.getReader().QueryRowContext(t.Context(), `
		SELECT value FROM archive_metadata WHERE key = ?`,
		CommonSchemaCompatibilityMetadataKey,
	).Scan(&stamp))
	assert.Equal(t, "1", stamp)

	var pricingMetadataValue, pricingMetadataUpdatedAt string
	require.NoError(t, database.getReader().QueryRowContext(t.Context(), `
		SELECT value, updated_at FROM pricing_metadata
		WHERE key = '_fallback_version'`,
	).Scan(&pricingMetadataValue, &pricingMetadataUpdatedAt))
	assert.Equal(t, "legacy-v42", pricingMetadataValue)
	_, err = time.Parse(time.RFC3339Nano, pricingMetadataUpdatedAt)
	require.NoError(t, err)
	var pricingSentinelCount int
	require.NoError(t, database.getReader().QueryRowContext(t.Context(), `
		SELECT count(*) FROM model_pricing
		WHERE model_pattern = '_fallback_version'`,
	).Scan(&pricingSentinelCount))
	assert.Zero(t, pricingSentinelCount)
	var privateInput, privateOutput int64
	require.NoError(t, database.getReader().QueryRowContext(t.Context(), `
		SELECT input_microdollars_per_mtok, output_microdollars_per_mtok
		FROM model_pricing WHERE model_pattern = '_private-model'`,
	).Scan(&privateInput, &privateOutput))
	assert.Equal(t, int64(1250000), privateInput)
	assert.Equal(t, int64(2500000), privateOutput)
}

func TestLegacySchemaCommonCutoverWritesCanonicalRowsAndDoesNotReplay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-common-cutover.db")
	createPriorCommonSQLiteArchive(t, path)

	database, err := Open(path)
	require.NoError(t, err)
	observation := export.ProjectIdentityObservation{
		SessionID: "common-legacy-session", Project: "runtime-project",
		Machine: "legacy-machine", RootPath: "/work/runtime",
		GitRemote:  "https://example.invalid/runtime.git",
		ObservedAt: time.Date(2026, 8, 2, 13, 0, 0, 0, time.UTC),
	}
	require.NoError(t, database.UpsertProjectIdentityObservation(
		t.Context(), observation,
	))
	_, err = database.CreateWorktreeProjectMapping(t.Context(), WorktreeProjectMapping{
		Machine: "legacy-machine", PathPrefix: "/work/runtime",
		Layout: WorktreeMappingLayoutExplicit, Project: "runtime-project",
		Enabled: true,
	})
	require.NoError(t, err)

	for table, want := range map[string]int{
		"project_identity_observations":             1,
		"session_project_identity_snapshots":        1,
		"worktree_project_mappings":                 1,
		"source_project_identity_observations":      2,
		"source_session_project_identity_snapshots": 1,
		"source_worktree_project_mappings":          2,
	} {
		var count int
		require.NoError(t, database.getReader().QueryRowContext(t.Context(),
			"SELECT count(*) FROM "+table,
		).Scan(&count))
		assert.Equal(t, want, count, table)
	}
	require.NoError(t, database.Close())

	conn, err := sql.Open("sqlite3", makeDSN(path, false))
	require.NoError(t, err)
	conn.SetMaxOpenConns(1)
	_, err = conn.ExecContext(t.Context(), `
		UPDATE project_identity_observations
		SET key = 'stale-legacy'
		WHERE project = 'legacy-project';
		UPDATE source_project_identity_observations
		SET key = 'canonical-after-cutover'
		WHERE project = 'legacy-project';
		UPDATE worktree_project_mappings
		SET project = 'stale-legacy'
		WHERE path_prefix = '/work/legacy';
		UPDATE source_worktree_project_mappings
		SET project = 'canonical-after-cutover'
		WHERE path_prefix = '/work/legacy';
	`)
	require.NoError(t, err)
	require.NoError(t, conn.Close())

	database, err = Open(path)
	require.NoError(t, err)
	defer database.Close()
	var identityKey, mappingProject string
	require.NoError(t, database.getReader().QueryRowContext(t.Context(), `
		SELECT key FROM source_project_identity_observations
		WHERE project = 'legacy-project'`,
	).Scan(&identityKey))
	assert.Equal(t, "canonical-after-cutover", identityKey)
	require.NoError(t, database.getReader().QueryRowContext(t.Context(), `
		SELECT project FROM source_worktree_project_mappings
		WHERE path_prefix = '/work/legacy'`,
	).Scan(&mappingProject))
	assert.Equal(t, "canonical-after-cutover", mappingProject)
}

func TestLegacySchemaCommonConvergenceAddsCompleteMappingShape(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-common-mapping-shape.db")
	createPriorCommonSQLiteArchive(t, path)
	conn, err := sql.Open("sqlite3", makeDSN(path, false))
	require.NoError(t, err)
	conn.SetMaxOpenConns(1)
	_, err = conn.ExecContext(t.Context(), `
		CREATE TABLE source_worktree_project_mappings (
			source_archive_id TEXT NOT NULL,
			machine TEXT NOT NULL,
			path_prefix TEXT NOT NULL,
			layout TEXT NOT NULL DEFAULT 'explicit',
			project TEXT NOT NULL DEFAULT '',
			original_project TEXT NOT NULL DEFAULT '',
			enabled INTEGER NOT NULL DEFAULT 1,
			updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
			PRIMARY KEY (source_archive_id, machine, path_prefix)
		);
		INSERT INTO worktree_project_mappings (
			id, machine, path_prefix, layout, project, enabled
		) VALUES (
			72, 'legacy-machine', '/work/legacy-second',
			'explicit', 'legacy-project', 1
		)`)
	require.NoError(t, err)
	require.NoError(t, conn.Close())

	database, err := Open(path)
	require.NoError(t, err)
	require.NoError(t, database.Close())

	conn, err = sql.Open("sqlite3", makeDSN(path, true))
	require.NoError(t, err)
	defer conn.Close()
	for _, column := range []string{"id", "created_at"} {
		var count int
		require.NoError(t, conn.QueryRowContext(t.Context(), `
			SELECT count(*) FROM pragma_table_info('source_worktree_project_mappings')
			WHERE name = ?`, column).Scan(&count))
		assert.Equal(t, 1, count, column)
	}
	rows, err := conn.QueryContext(t.Context(), `
		SELECT id, path_prefix
		FROM source_worktree_project_mappings
		WHERE machine = 'legacy-machine'
		ORDER BY id`)
	require.NoError(t, err)
	defer rows.Close()
	type mappingIdentity struct {
		id     int64
		prefix string
	}
	var mappings []mappingIdentity
	for rows.Next() {
		var mapping mappingIdentity
		require.NoError(t, rows.Scan(&mapping.id, &mapping.prefix))
		mappings = append(mappings, mapping)
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, []mappingIdentity{
		{id: 71, prefix: "/work/legacy"},
		{id: 72, prefix: "/work/legacy-second"},
	}, mappings)
}

func TestLegacySchemaStampedCommonSchemaRejectsTriggerDriftWithoutRepair(
	t *testing.T,
) {
	path := filepath.Join(t.TempDir(), "stamped-common-trigger-drift.db")
	database, err := Open(path)
	require.NoError(t, err)
	require.NoError(t, database.Close())

	const triggerName = "trg_source_project_identity_observations_revision_insert"
	conn, err := sql.Open("sqlite3", makeDSN(path, false))
	require.NoError(t, err)
	conn.SetMaxOpenConns(1)
	_, err = conn.ExecContext(t.Context(), `
		DROP TRIGGER `+triggerName+`;
		CREATE TRIGGER `+triggerName+`
		AFTER INSERT ON source_project_identity_observations BEGIN
			SELECT 1;
		END`)
	require.NoError(t, err)
	var driftedSQL string
	require.NoError(t, conn.QueryRowContext(t.Context(), `
		SELECT sql FROM sqlite_master WHERE type = 'trigger' AND name = ?`,
		triggerName,
	).Scan(&driftedSQL))
	require.NoError(t, conn.Close())

	database, err = Open(path)
	require.Error(t, err)
	assert.Nil(t, database)
	assert.Contains(t, err.Error(), "canonical SQLite trigger")

	conn, err = sql.Open("sqlite3", makeDSN(path, true))
	require.NoError(t, err)
	defer conn.Close()
	var afterSQL string
	require.NoError(t, conn.QueryRowContext(t.Context(), `
		SELECT sql FROM sqlite_master WHERE type = 'trigger' AND name = ?`,
		triggerName,
	).Scan(&afterSQL))
	assert.Equal(t, driftedSQL, afterSQL)
}

func TestLegacySchemaCommonConvergenceRollsBackDDLDataAndStamp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-common-rollback.db")
	createPriorCommonSQLiteArchive(t, path)

	database, err := openAndInit(path, false)
	require.NoError(t, err)
	_, err = database.GetOrCreateDatabaseID(t.Context())
	require.NoError(t, err)
	_, err = database.GetOrCreateArchiveID(t.Context())
	require.NoError(t, err)
	_, err = database.GetOrCreateArchiveSalt(t.Context())
	require.NoError(t, err)

	injected := errors.New("injected common convergence failure")
	database.mu.Lock()
	err = database.convergeSQLiteCommonSchemaLocked(t.Context(), func() error {
		return injected
	})
	database.mu.Unlock()
	require.ErrorIs(t, err, injected)
	require.NoError(t, database.Close())

	conn, err := sql.Open("sqlite3", makeDSN(path, false))
	require.NoError(t, err)
	conn.SetMaxOpenConns(1)
	defer conn.Close()
	for table, column := range map[string]string{
		"sessions":        "source_archive_id",
		"tool_calls":      "message_ordinal",
		"pinned_messages": "source_uuid",
	} {
		var count int
		require.NoError(t, conn.QueryRow(`
			SELECT count(*) FROM pragma_table_info(?) WHERE name = ?`,
			table, column,
		).Scan(&count))
		assert.Zero(t, count, "%s.%s", table, column)
	}
	var sourceTableCount, stampCount int
	require.NoError(t, conn.QueryRow(`
		SELECT count(*) FROM sqlite_master
		WHERE type = 'table' AND name = 'source_archives'`,
	).Scan(&sourceTableCount))
	assert.Zero(t, sourceTableCount)
	require.NoError(t, conn.QueryRow(`
		SELECT count(*) FROM archive_metadata WHERE key = ?`,
		CommonSchemaCompatibilityMetadataKey,
	).Scan(&stampCount))
	assert.Zero(t, stampCount)
	var sessionCount int
	require.NoError(t, conn.QueryRow(`
		SELECT count(*) FROM sessions WHERE id = 'common-legacy-session'`,
	).Scan(&sessionCount))
	assert.Equal(t, 1, sessionCount)
}

const legacyMessagesAndToolCallsSchema = `
CREATE TABLE messages (
    id             INTEGER PRIMARY KEY,
    session_id     TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    ordinal        INTEGER NOT NULL,
    role           TEXT NOT NULL,
    content        TEXT NOT NULL,
    timestamp      TEXT,
    has_thinking   INTEGER NOT NULL DEFAULT 0,
    has_tool_use   INTEGER NOT NULL DEFAULT 0,
    content_length INTEGER NOT NULL DEFAULT 0,
    UNIQUE(session_id, ordinal)
);
CREATE TABLE tool_calls (
    id         INTEGER PRIMARY KEY,
    message_id INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    tool_name  TEXT NOT NULL,
    category   TEXT NOT NULL
);`

const preParentLegacySchema = `
CREATE TABLE sessions (
    id          TEXT PRIMARY KEY,
    project     TEXT NOT NULL,
    machine     TEXT NOT NULL DEFAULT 'local',
    agent       TEXT NOT NULL DEFAULT 'claude',
    first_message TEXT,
    started_at  TEXT,
    ended_at    TEXT,
    message_count INTEGER NOT NULL DEFAULT 0,
    file_path   TEXT,
    file_size   INTEGER,
    file_mtime  INTEGER,
    file_hash   TEXT,
    created_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);` + legacyMessagesAndToolCallsSchema

const v06LegacySchema = `
CREATE TABLE sessions (
    id          TEXT PRIMARY KEY,
    project     TEXT NOT NULL,
    machine     TEXT NOT NULL DEFAULT 'local',
    agent       TEXT NOT NULL DEFAULT 'claude',
    first_message TEXT,
    started_at  TEXT,
    ended_at    TEXT,
    message_count INTEGER NOT NULL DEFAULT 0,
    user_message_count INTEGER NOT NULL DEFAULT 0,
    file_path   TEXT,
    file_size   INTEGER,
    file_mtime  INTEGER,
    file_hash   TEXT,
    parent_session_id TEXT,
    created_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);
CREATE TABLE messages (
    id             INTEGER PRIMARY KEY,
    session_id     TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    ordinal        INTEGER NOT NULL,
    role           TEXT NOT NULL,
    content        TEXT NOT NULL,
    timestamp      TEXT,
    has_thinking   INTEGER NOT NULL DEFAULT 0,
    has_tool_use   INTEGER NOT NULL DEFAULT 0,
    content_length INTEGER NOT NULL DEFAULT 0,
    UNIQUE(session_id, ordinal)
);
CREATE TABLE tool_calls (
    id         INTEGER PRIMARY KEY,
    message_id INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    tool_name  TEXT NOT NULL,
    category   TEXT NOT NULL,
    tool_use_id TEXT,
    input_json TEXT,
    skill_name TEXT,
    result_content_length INTEGER
);
CREATE TABLE insights (
    id          INTEGER PRIMARY KEY,
    type        TEXT NOT NULL,
    date_from   TEXT NOT NULL,
    date_to     TEXT NOT NULL,
    project     TEXT,
    agent       TEXT NOT NULL,
    model       TEXT,
    prompt      TEXT,
    content     TEXT NOT NULL,
    created_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);
CREATE INDEX idx_insights_lookup
    ON insights(type, date_from, project);`

const legacyArchiveRows = `
INSERT INTO sessions (
    id, project, machine, agent, first_message, message_count,
    file_path, file_size, file_mtime, file_hash
) VALUES (
    'legacy-session', 'project-a', 'local', 'claude',
    'archived prompt', 1, '/archive/session.jsonl', 128, 42, 'legacy-hash'
);
INSERT INTO messages (
    id, session_id, ordinal, role, content, has_tool_use, content_length
) VALUES (
    1, 'legacy-session', 0, 'assistant', 'archived answer', 1, 15
);
INSERT INTO tool_calls (
    id, message_id, session_id, tool_name, category
) VALUES (
    1, 1, 'legacy-session', 'Read', 'Read'
);`

func TestOpenLegacySchemasPreservesArchiveAndRequestsResync(t *testing.T) {
	tests := []struct {
		name            string
		schema          string
		wantInsightDate string
	}{
		{
			name:   "pre-parent-link archive",
			schema: preParentLegacySchema,
		},
		{
			name:            "v0.6 archive with range insight",
			schema:          v06LegacySchema,
			wantInsightDate: "2026-02-23",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "legacy.db")
			conn, err := sql.Open("sqlite3", makeDSN(path, false))
			require.NoError(t, err)
			conn.SetMaxOpenConns(1)

			_, err = conn.Exec(tc.schema)
			require.NoError(t, err)
			_, err = conn.Exec(legacyArchiveRows)
			require.NoError(t, err)
			if tc.wantInsightDate != "" {
				_, err = conn.Exec(`
					INSERT INTO insights (
						id, type, date_from, date_to, project,
						agent, model, prompt, content
					) VALUES (
						1, 'daily', '2026-02-23', '2026-02-23',
						'project-a', 'claude', 'model-a',
						'summarize', 'archived insight'
					)`)
				require.NoError(t, err)
			}
			_, err = conn.Exec(fmt.Sprintf(
				"PRAGMA user_version = %d", dataVersion,
			))
			require.NoError(t, err)
			require.NoError(t, conn.Close())

			d, err := Open(path)
			require.NoError(t, err)
			assert.True(t, d.NeedsResync())

			session := requireSessionExists(t, d, "legacy-session")
			assert.Equal(t, "project-a", session.Project)
			require.NotNil(t, session.FirstMessage)
			assert.Equal(t, "archived prompt", *session.FirstMessage)

			messages, err := d.GetMessages(
				context.Background(), "legacy-session", 0, 10, true,
			)
			require.NoError(t, err)
			require.Len(t, messages, 1)
			require.Len(t, messages[0].ToolCalls, 1)
			assert.Equal(t, "Read", messages[0].ToolCalls[0].ToolName)

			if tc.wantInsightDate != "" {
				var dateFrom, dateTo string
				err = d.getReader().QueryRow(`
					SELECT date_from, date_to FROM insights WHERE id = 1
				`).Scan(&dateFrom, &dateTo)
				require.NoError(t, err)
				assert.Equal(t, tc.wantInsightDate, dateFrom)
				assert.Equal(t, tc.wantInsightDate, dateTo)

				id, err := d.InsertInsight(Insight{
					Type:     "daily",
					DateFrom: "2026-07-14",
					DateTo:   "2026-07-14",
					Agent:    "claude",
					Content:  "new insight",
				})
				require.NoError(t, err)
				inserted, err := d.GetInsight(context.Background(), id)
				require.NoError(t, err)
				require.NotNil(t, inserted)
				assert.Equal(t, "2026-07-14", inserted.DateFrom)
				assert.Equal(t, "2026-07-14", inserted.DateTo)
				assert.Equal(t, "new insight", inserted.Content)

				requireIndexColumns(t, d, "idx_insights_lookup", []string{
					"type", "date_from", "date_to", "project",
				})
			}

			requireLegacyRepairIndexes(t, d)
			require.NoError(t, d.Close())

			reopened, err := Open(path)
			require.NoError(t, err)
			defer reopened.Close()
			require.True(t, reopened.NeedsResync())
		})
	}
}

func TestParserParentSessionIDMigrationBackfillsCurrentParent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	conn, err := sql.Open("sqlite3", makeDSN(path, false))
	require.NoError(t, err)
	conn.SetMaxOpenConns(1)

	_, err = conn.Exec(v06LegacySchema)
	require.NoError(t, err, "create legacy schema")
	_, err = conn.Exec(`
		INSERT INTO sessions (
			id, project, machine, agent, parent_session_id
		) VALUES (
			'kid', 'project-a', 'local', 'claude', 'parsed-parent'
		)`)
	require.NoError(t, err, "insert legacy child")
	_, err = conn.Exec(fmt.Sprintf(
		"PRAGMA user_version = %d", dataVersion,
	))
	require.NoError(t, err, "set current data version")
	require.NoError(t, conn.Close(), "close legacy database")

	d, err := Open(path)
	require.NoError(t, err, "open migrated database")
	defer d.Close()

	var got sql.NullString
	err = d.getReader().QueryRow(`
		SELECT parser_parent_session_id FROM sessions WHERE id = 'kid'
	`).Scan(&got)
	require.NoError(t, err, "query migrated parser parent")
	require.True(t, got.Valid, "migrated parser parent must be set")
	assert.Equal(t, "parsed-parent", got.String, "migrated parser parent")
}

func TestParserParentSessionIDMigrationRollsBackWhenBackfillFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	conn, err := sql.Open("sqlite3", makeDSN(path, false))
	require.NoError(t, err)
	conn.SetMaxOpenConns(1)

	_, err = conn.Exec(v06LegacySchema)
	require.NoError(t, err, "create legacy schema")
	_, err = conn.Exec(`
		INSERT INTO sessions (
			id, project, machine, agent, parent_session_id
		) VALUES (
			'kid', 'project-a', 'local', 'claude', 'parsed-parent'
		);
		CREATE TRIGGER fail_parser_parent_backfill
		BEFORE UPDATE OF parser_parent_session_id ON sessions BEGIN
			SELECT RAISE(ABORT, 'injected parser parent backfill failure');
		END;`)
	require.NoError(t, err, "prepare failing legacy migration")
	_, err = conn.Exec(fmt.Sprintf(
		"PRAGMA user_version = %d", dataVersion,
	))
	require.NoError(t, err, "set current data version")
	require.NoError(t, conn.Close(), "close legacy database")

	d, err := Open(path)
	require.ErrorContains(t, err, "injected parser parent backfill failure")
	require.Nil(t, d)

	conn, err = sql.Open("sqlite3", makeDSN(path, false))
	require.NoError(t, err, "reopen failed migration")
	conn.SetMaxOpenConns(1)
	var columnCount int
	err = conn.QueryRow(`
		SELECT count(*) FROM pragma_table_info('sessions')
		WHERE name = 'parser_parent_session_id'
	`).Scan(&columnCount)
	require.NoError(t, err, "inspect schema after failed migration")
	assert.Zero(t, columnCount, "failed migration must roll back added column")
	_, err = conn.Exec(`DROP TRIGGER fail_parser_parent_backfill`)
	require.NoError(t, err, "remove injected migration failure")
	require.NoError(t, conn.Close(), "close failed migration database")

	d, err = Open(path)
	require.NoError(t, err, "retry migration")
	defer d.Close()

	var got sql.NullString
	err = d.getReader().QueryRow(`
		SELECT parser_parent_session_id FROM sessions WHERE id = 'kid'
	`).Scan(&got)
	require.NoError(t, err, "query retried parser parent")
	require.True(t, got.Valid, "retried parser parent must be set")
	assert.Equal(t, "parsed-parent", got.String, "retried parser parent")
}

func TestLegacySchemaAddsArtifactImportAuthorityNonDestructively(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	conn, err := sql.Open("sqlite3", makeDSN(path, false))
	require.NoError(t, err)
	conn.SetMaxOpenConns(1)
	_, err = conn.Exec(preParentLegacySchema)
	require.NoError(t, err)
	_, err = conn.Exec(legacyArchiveRows)
	require.NoError(t, err)
	_, err = conn.Exec(fmt.Sprintf("PRAGMA user_version = %d", dataVersion))
	require.NoError(t, err)
	require.NoError(t, conn.Close())

	database, err := Open(path)
	require.NoError(t, err)
	defer database.Close()
	requireSessionExists(t, database, "legacy-session")

	for _, table := range []string{
		"artifact_import_queue",
		"artifact_import_attempt_generations",
		"artifact_peer_checkpoint_heads",
	} {
		var count int
		err := database.getReader().QueryRow(`
			SELECT count(*) FROM sqlite_master
			WHERE type = 'table' AND name = ?
		`, table).Scan(&count)
		require.NoError(t, err)
		assert.Equal(t, 1, count, "table %s", table)
	}
}

func requireIndexColumns(
	t *testing.T, d *DB, index string, want []string,
) {
	t.Helper()
	rows, err := d.getReader().Query(`
		SELECT name FROM pragma_index_info(?) ORDER BY seqno
	`, index)
	require.NoError(t, err)
	defer rows.Close()

	var got []string
	for rows.Next() {
		var column string
		require.NoError(t, rows.Scan(&column))
		got = append(got, column)
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, want, got)
}

func requireLegacyRepairIndexes(t *testing.T, d *DB) {
	t.Helper()
	for _, name := range []string{
		"idx_sessions_parent",
		"idx_sessions_user_message_count",
		"idx_tool_calls_skill",
		"idx_tool_calls_subagent",
		"idx_insights_lookup",
	} {
		var count int
		err := d.getReader().QueryRow(`
			SELECT count(*) FROM sqlite_master
			WHERE type = 'index' AND name = ?
		`, name).Scan(&count)
		require.NoError(t, err)
		assert.Equal(t, 1, count, "index %s", name)
	}
}
