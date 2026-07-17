//go:build !(windows && arm64)

package duckdb

import (
	"context"
	"database/sql"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/export"
)

func TestOpenCreatesLocalDuckDBFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agentsview.duckdb")

	db, err := Open(path)
	require.NoError(t, err, "Open")
	t.Cleanup(func() {
		require.NoError(t, db.Close(), "close DuckDB")
	})

	require.NoError(t, db.PingContext(context.Background()))
	assert.FileExists(t, path)
}

func TestOpenRejectsEmptyPath(t *testing.T) {
	db, err := Open("")
	require.Error(t, err)
	assert.Nil(t, db)
	assert.Contains(t, err.Error(), "duckdb path is required")
}

func TestEnsureSchemaCreatesRequiredMirrorTables(t *testing.T) {
	ctx := context.Background()
	db := openTestDuckDB(t)

	require.NoError(t, EnsureSchema(ctx, db), "EnsureSchema")

	for _, table := range []string{
		"sync_metadata",
		"sessions",
		"messages",
		"usage_events",
		"cursor_usage_events",
		"model_pricing",
		"tool_calls",
		"tool_result_events",
		"secret_findings",
		"starred_sessions",
		"pinned_messages",
	} {
		assert.True(t, tableExists(t, db, table), "missing table %s", table)
	}

	var version string
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT value FROM sync_metadata WHERE key = ?`,
		schemaVersionMetadataKey,
	).Scan(&version))
	assert.Equal(t, strconv.Itoa(SchemaVersion), version)
	var repaired string
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT value FROM sync_metadata WHERE key = ?`,
		defaultRepairMetadataKey,
	).Scan(&repaired))
	assert.Equal(t, "1", repaired)
	var dedupIndex string
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT value FROM sync_metadata WHERE key = ?`,
		usageDedupIndexMetadataKey,
	).Scan(&dedupIndex))
	assert.Equal(t, "1", dedupIndex)
	var remoteScrubbed string
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT value FROM sync_metadata WHERE key = ?`,
		projectIdentityRemoteScrubMetadataKey,
	).Scan(&remoteScrubbed))
	assert.Equal(t, "1", remoteScrubbed)
	var codexPayloadsRepaired string
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT value FROM sync_metadata WHERE key = ?`,
		codexEncryptedPayloadRepairMetadataKey,
	).Scan(&codexPayloadsRepaired))
	assert.Equal(t, "1", codexPayloadsRepaired)
}

func TestSessionReadDefaultsNullableCertificationFromOlderWriter(t *testing.T) {
	ctx := context.Background()
	database := openTestDuckDB(t)
	require.NoError(t, EnsureSchema(ctx, database), "initial EnsureSchema")

	for _, index := range []string{
		"idx_sessions_ended",
		"idx_sessions_project",
		"idx_sessions_machine",
		"idx_sessions_parent",
		"idx_sessions_started",
		"idx_sessions_agent",
		"idx_sessions_agent_data_version",
		"idx_sessions_termination_status",
	} {
		_, err := database.ExecContext(ctx, "DROP INDEX "+index)
		require.NoError(t, err)
	}
	for _, column := range []string{
		"codex_payload_certified_revision",
		"codex_payload_certification_version",
	} {
		_, err := database.ExecContext(ctx,
			"ALTER TABLE sessions DROP COLUMN "+column,
		)
		require.NoError(t, err)
	}
	require.NoError(t, EnsureSchema(ctx, database), "additive EnsureSchema")

	// An older compatible writer does not name certification columns. DuckDB
	// therefore stores NULL after the additive migration added them without
	// defaults for Quack compatibility.
	_, err := database.ExecContext(ctx, `
		INSERT INTO sessions (id, project)
		VALUES ('older-writer-session', 'project')`)
	require.NoError(t, err)

	got, err := NewStoreFromDB(database).GetSession(ctx, "older-writer-session")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Empty(t, got.CodexPayloadCertifiedRevision)
	assert.Zero(t, got.CodexPayloadCertificationVersion)
}

func TestEnsureSchemaRepairsOldCodexPayloads(t *testing.T) {
	const fernet = "gAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=="
	ctx := context.Background()
	database := openTestDuckDB(t)
	require.NoError(t, EnsureSchema(ctx, database), "initial EnsureSchema")

	_, err := database.ExecContext(ctx, `
		INSERT INTO sessions (
			id, project, machine, agent, first_message, relationship_type,
			data_version, transcript_revision, quality_signal_version,
			health_score, health_grade,
			short_prompt_count, unstructured_start,
			missing_success_criteria_count, missing_verification_count,
			duplicate_prompt_count, no_code_context_count,
			runaway_tool_loop_count, secret_leak_count, secrets_rules_version
		) VALUES (
			'codex-old', 'project', 'machine', 'codex', ?, 'subagent',
			64, '7', 2, 17, 'D', 3, TRUE, 4, 5, 6, 7, 8, 1, 'stale-rules'
		)`, fernet)
	require.NoError(t, err, "insert stale DuckDB Codex session")
	toolContent := "[Task: spawn_agent]\n" + fernet
	// The collab row is authoritative even when an older writer left the
	// cached message flag false.
	_, err = database.ExecContext(ctx, `
		INSERT INTO messages (
			id, session_id, ordinal, role, content, has_tool_use, content_length
		) VALUES
			(1, 'codex-old', 0, 'assistant', ?, FALSE, ?),
			(2, 'codex-old', 1, 'user', ?, FALSE, ?)`,
		toolContent, len(toolContent), fernet, len(fernet))
	require.NoError(t, err, "insert stale DuckDB Codex messages")
	_, err = database.ExecContext(ctx, `
		INSERT INTO tool_calls (
			message_id, session_id, tool_name, category, call_index, input_json
		) VALUES (1, 'codex-old', 'spawn_agent', 'Task', 0, ?)`,
		`{"task_name":"worker","message":"`+fernet+`"}`)
	require.NoError(t, err, "insert stale DuckDB Codex tool call without an id")
	_, err = database.ExecContext(ctx, `
		INSERT INTO secret_findings (
			id, session_id, rule_name, confidence, location_kind,
			message_ordinal, match_start, match_end, match_index,
			redacted_match, rules_version
		) VALUES (
			1, 'codex-old', 'test-secret', 'definite', 'message',
			1, 0, 8, 0, '[redacted]', 'stale-rules'
		)`)
	require.NoError(t, err, "insert stale DuckDB Codex secret finding")

	err = CheckSchemaCompat(ctx, database)
	require.ErrorIs(t, err, ErrCodexEncryptedPayloadRepairRequired,
		"a post-marker legacy push must fail the read compatibility gate")

	require.NoError(t, EnsureSchema(ctx, database), "repair EnsureSchema")
	require.NoError(t, CheckSchemaCompat(ctx, database),
		"repaired DuckDB mirror must pass compatibility")

	var gotToolContent, gotInbound, gotInput, gotPreview string
	var gotToolLength, gotInboundLength int
	require.NoError(t, database.QueryRowContext(ctx, `
		SELECT content, content_length FROM messages
		 WHERE session_id = 'codex-old' AND ordinal = 0`,
	).Scan(&gotToolContent, &gotToolLength))
	require.NoError(t, database.QueryRowContext(ctx, `
		SELECT content, content_length FROM messages
		 WHERE session_id = 'codex-old' AND ordinal = 1`,
	).Scan(&gotInbound, &gotInboundLength))
	require.NoError(t, database.QueryRowContext(ctx, `
		SELECT input_json FROM tool_calls
		 WHERE session_id = 'codex-old' AND message_id = 1 AND call_index = 0`,
	).Scan(&gotInput))
	require.NoError(t, database.QueryRowContext(ctx, `
		SELECT first_message FROM sessions WHERE id = 'codex-old'`,
	).Scan(&gotPreview))
	assert.Equal(t, "[Task: spawn_agent]\n[encrypted]", gotToolContent)
	assert.Equal(t, len(gotToolContent), gotToolLength)
	assert.Equal(t, "[encrypted]", gotInbound)
	assert.Equal(t, len(gotInbound), gotInboundLength)
	assert.Equal(t,
		`{"task_name":"worker","message":"[encrypted]"}`, gotInput)
	assert.Equal(t, "[encrypted]", gotPreview)

	var qualityVersion, shortPrompts, missingSuccess, missingVerification int
	var duplicatePrompts, noCodeContext, runawayLoops, secretLeakCount int
	var unstructuredStart bool
	var healthScore sql.NullInt64
	var healthGrade sql.NullString
	var rulesVersion, transcriptRevision string
	require.NoError(t, database.QueryRowContext(ctx, `
		SELECT quality_signal_version, health_score, health_grade,
		       short_prompt_count, unstructured_start,
		       missing_success_criteria_count, missing_verification_count,
		       duplicate_prompt_count, no_code_context_count,
		       runaway_tool_loop_count, secret_leak_count,
		       secrets_rules_version, transcript_revision
		  FROM sessions WHERE id = 'codex-old'`,
	).Scan(
		&qualityVersion, &healthScore, &healthGrade,
		&shortPrompts, &unstructuredStart,
		&missingSuccess, &missingVerification, &duplicatePrompts,
		&noCodeContext, &runawayLoops, &secretLeakCount,
		&rulesVersion, &transcriptRevision,
	))
	assert.Equal(t, []int{0, 0, 0, 0, 0, 0, 0, 0}, []int{
		qualityVersion, shortPrompts, missingSuccess, missingVerification,
		duplicatePrompts, noCodeContext, runawayLoops, secretLeakCount,
	})
	assert.False(t, unstructuredStart)
	assert.False(t, healthScore.Valid)
	assert.False(t, healthGrade.Valid)
	assert.Empty(t, rulesVersion)
	var dataVersion int
	require.NoError(t, database.QueryRowContext(ctx,
		`SELECT data_version FROM sessions WHERE id = 'codex-old'`,
	).Scan(&dataVersion))
	assert.Equal(t, codexEncryptedPayloadDataVersion, dataVersion)
	assert.Equal(t, "8", transcriptRevision,
		"the repaired DuckDB transcript must advance exactly once")
	var findingCount int
	require.NoError(t, database.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM secret_findings WHERE session_id = 'codex-old'`,
	).Scan(&findingCount))
	assert.Zero(t, findingCount)
}

func TestEnsureSchemaRepairsLegacyCodexEncryptedHeader(t *testing.T) {
	const (
		fernet            = "gAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=="
		legacyDataVersion = 67
	)
	ctx := context.Background()
	database := openTestDuckDB(t)
	require.NoError(t, EnsureSchema(ctx, database), "initial EnsureSchema")
	content := "[Task: " + fernet + "]\nRun the task"
	_, err := database.ExecContext(ctx, `
		INSERT INTO sessions (
			id, project, machine, agent, relationship_type, data_version
		) VALUES ('encrypted-header', 'project', 'machine', 'codex', '', ?)`,
		legacyDataVersion)
	require.NoError(t, err, "seed legacy session")
	_, err = database.ExecContext(ctx, `
		INSERT INTO messages (
			id, session_id, ordinal, role, content, has_tool_use, content_length
		) VALUES (104, 'encrypted-header', 0, 'assistant', ?, TRUE, ?)`,
		content, len(content))
	require.NoError(t, err, "seed encrypted collaboration header")
	_, err = database.ExecContext(ctx, `
		INSERT INTO tool_calls (
			message_id, session_id, tool_name, category, call_index, input_json
		) VALUES (
			104, 'encrypted-header', 'spawn_agent', 'Task', 0,
			'{"task_name":"[encrypted]","message":"Run the task"}'
		)`)
	require.NoError(t, err, "seed current redacted tool input")

	err = CheckSchemaCompat(ctx, database)
	require.ErrorIs(t, err, ErrCodexEncryptedPayloadRepairRequired,
		"legacy header ciphertext must fail closed before repair")
	require.NoError(t, EnsureSchema(ctx, database), "repair legacy header")
	require.NoError(t, CheckSchemaCompat(ctx, database),
		"repaired header must pass DuckDB compatibility")

	var gotContent string
	var gotLength, gotVersion int
	require.NoError(t, database.QueryRowContext(ctx, `
		SELECT m.content, m.content_length, s.data_version
		  FROM messages m
		  JOIN sessions s ON s.id = m.session_id
		 WHERE s.id = 'encrypted-header' AND m.ordinal = 0`,
	).Scan(&gotContent, &gotLength, &gotVersion))
	want := "[Task: [encrypted]]\nRun the task"
	assert.Equal(t, want, gotContent)
	assert.Equal(t, len(want), gotLength)
	assert.Equal(t, codexEncryptedPayloadDataVersion, gotVersion)
}

func TestEnsureSchemaWithholdsWatermarkFromUncertifiedLegacyCodexPayloads(
	t *testing.T,
) {
	const (
		fernet            = "gAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=="
		legacyDataVersion = 67
	)
	ctx := context.Background()
	database := openTestDuckDB(t)
	require.NoError(t, EnsureSchema(ctx, database), "initial EnsureSchema")
	_, err := database.ExecContext(ctx, `
		INSERT INTO sessions (
			id, project, machine, agent, relationship_type, data_version
		) VALUES
			('unlinked-child', 'project', 'machine', 'codex', '', ?),
			('lost-tool-call', 'project', 'machine', 'codex', '', ?),
			('stale-missing-tool-call', 'project', 'machine', 'codex', '', ?)`,
		legacyDataVersion, legacyDataVersion, legacyDataVersion)
	require.NoError(t, err, "seed falsely certified legacy sessions")
	formatted := "[Task: spawn_agent]\n" + fernet
	_, err = database.ExecContext(ctx, `
		INSERT INTO messages (
			id, session_id, ordinal, role, content, has_tool_use, content_length
		) VALUES
			(101, 'unlinked-child', 0, 'user', ?, FALSE, ?),
			(102, 'lost-tool-call', 0, 'assistant', ?, TRUE, ?),
			(103, 'stale-missing-tool-call', 0, 'assistant', ?, FALSE, ?)`,
		fernet, len(fernet), formatted, len(formatted),
		formatted, len(formatted))
	require.NoError(t, err, "seed metadata-independent ciphertext shapes")
	_, err = database.ExecContext(ctx, `
		INSERT INTO tool_calls (
			message_id, session_id, tool_name, category, call_index, input_json
		) VALUES (102, 'lost-tool-call', 'Bash', 'Bash', 0, '{}')`)
	require.NoError(t, err,
		"leave a non-collab tool row beside the lost collaboration call")

	err = CheckSchemaCompat(ctx, database)
	require.ErrorIs(t, err, ErrCodexEncryptedPayloadRepairRequired,
		"legacy rows must fail closed until the stricter recertification runs")
	require.NoError(t, EnsureSchema(ctx, database), "recheck legacy sessions")
	err = CheckSchemaCompat(ctx, database)
	require.ErrorIs(t, err, ErrCodexEncryptedPayloadRepairRequired,
		"uncertified rows must keep DuckDB reads gated after the repair pass")

	rows, err := database.QueryContext(ctx, `
		SELECT s.id, s.data_version, m.content
		  FROM sessions s
		  JOIN messages m ON m.session_id = s.id
		 WHERE s.id IN (
			'unlinked-child', 'lost-tool-call', 'stale-missing-tool-call'
		 )
		 ORDER BY s.id`)
	require.NoError(t, err, "query recertified sessions")
	defer rows.Close()
	type repairedRow struct {
		id      string
		version int
		content string
	}
	var got []repairedRow
	for rows.Next() {
		var row repairedRow
		require.NoError(t, rows.Scan(&row.id, &row.version, &row.content))
		got = append(got, row)
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, []repairedRow{
		{id: "lost-tool-call", version: legacyDataVersion, content: formatted},
		{id: "stale-missing-tool-call", version: legacyDataVersion, content: formatted},
		{id: "unlinked-child", version: legacyDataVersion, content: fernet},
	}, got)
}

func TestUsageEventsDedupIndexAllowsRepeatedKeys(t *testing.T) {
	ctx := context.Background()
	db := openTestDuckDB(t)
	require.NoError(t, EnsureSchema(ctx, db), "EnsureSchema")

	_, err := db.ExecContext(ctx, `
		INSERT INTO usage_events (id, session_id, source, model, dedup_key)
		VALUES
			(1, 's1', 'hermes', 'claude-test', ''),
			(2, 's1', 'hermes', 'claude-test', '')`)
	require.NoError(t, err)

	_, err = db.ExecContext(ctx, `
		INSERT INTO usage_events (id, session_id, source, model, dedup_key)
		VALUES
			(3, 's1', 'hermes', 'claude-test', 'same-key'),
			(4, 's1', 'hermes', 'claude-test', 'same-key')`)
	require.NoError(t, err)
}

func TestEnsureSchemaIsIdempotent(t *testing.T) {
	ctx := context.Background()
	db := openTestDuckDB(t)

	require.NoError(t, EnsureSchema(ctx, db), "first EnsureSchema")
	_, err := db.ExecContext(ctx, `
		UPDATE sync_metadata SET value = 'already-recorded' WHERE key = ?`,
		codexEncryptedPayloadRepairMetadataKey,
	)
	require.NoError(t, err, "mark Codex repair with observable existing value")
	require.NoError(t, EnsureSchema(ctx, db), "second EnsureSchema")

	assert.True(t, columnExists(t, db, "sessions", "secret_leak_count"))
	assert.True(t, columnExists(t, db, "messages", "thinking_text"))
	var repairMarker string
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT value FROM sync_metadata WHERE key = ?`,
		codexEncryptedPayloadRepairMetadataKey,
	).Scan(&repairMarker))
	assert.Equal(t, "already-recorded", repairMarker,
		"clean startup must not rewrite an existing Codex repair marker")
}

func TestEnsureSchemaAddsMissingColumnsNonDestructively(t *testing.T) {
	ctx := context.Background()
	db := openTestDuckDB(t)
	_, err := db.ExecContext(ctx,
		`CREATE TABLE sessions (
			id TEXT PRIMARY KEY,
			machine TEXT NOT NULL,
			project TEXT NOT NULL,
			agent TEXT NOT NULL,
			message_count INTEGER,
			relationship_type TEXT,
			is_automated BOOLEAN
		)`,
	)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx,
		`INSERT INTO sessions (
			id, machine, project, agent,
			message_count, relationship_type, is_automated
		) VALUES (?, ?, ?, ?, NULL, NULL, NULL)`,
		"kept", "mac", "alpha", "claude",
	)
	require.NoError(t, err)

	require.NoError(t, EnsureSchema(ctx, db), "EnsureSchema")

	assert.True(t, columnExists(t, db, "sessions", "ended_at"))
	var project string
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT project FROM sessions WHERE id = ?`, "kept",
	).Scan(&project))
	assert.Equal(t, "alpha", project)
	var messageCount int
	var relationshipType string
	var isAutomated bool
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT message_count, relationship_type, is_automated
		 FROM sessions WHERE id = ?`, "kept",
	).Scan(&messageCount, &relationshipType, &isAutomated))
	assert.Equal(t, 0, messageCount)
	assert.Equal(t, "", relationshipType)
	assert.False(t, isAutomated)
}

func TestEnsureSchemaMigratesMessagesIDPrimaryKey(t *testing.T) {
	ctx := context.Background()
	db := openTestDuckDB(t)
	_, err := db.ExecContext(ctx, `
		CREATE TABLE messages (
			id BIGINT PRIMARY KEY,
			session_id TEXT NOT NULL,
			ordinal INTEGER NOT NULL,
			role TEXT NOT NULL,
			content TEXT NOT NULL,
			UNIQUE(session_id, ordinal)
		)`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `
		INSERT INTO messages (id, session_id, ordinal, role, content)
		VALUES (1, 'from-other-machine', 0, 'user', 'kept')`)
	require.NoError(t, err)

	require.NoError(t, EnsureSchema(ctx, db), "EnsureSchema")

	hasPrimary, err := tableHasPrimaryKey(ctx, db, "messages")
	require.NoError(t, err)
	assert.False(t, hasPrimary)
	_, err = db.ExecContext(ctx, `
		INSERT INTO messages (id, session_id, ordinal, role, content)
		VALUES (1, 'from-this-machine', 0, 'user', 'same local rowid')`)
	require.NoError(t, err)
	assertDuckDBCountWhere(t, db, "messages", "id = ?", int64(1), 2)
}

func TestCheckSchemaCompatRejectsPendingNonIndexRepairs(t *testing.T) {
	ctx := context.Background()

	t.Run("messages id primary key", func(t *testing.T) {
		db := openTestDuckDB(t)
		require.NoError(t, EnsureSchema(ctx, db), "EnsureSchema")
		recreateMessagesWithIDPrimaryKey(t, ctx, db)

		err := CheckSchemaCompat(ctx, db)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "messages.id primary key")
	})

	t.Run("missing default repair metadata", func(t *testing.T) {
		db := openTestDuckDB(t)
		require.NoError(t, EnsureSchema(ctx, db), "EnsureSchema")
		_, err := db.ExecContext(ctx,
			`DELETE FROM sync_metadata WHERE key = ?`,
			defaultRepairMetadataKey,
		)
		require.NoError(t, err)

		err = CheckSchemaCompat(ctx, db)
		require.Error(t, err)
		assert.Contains(t, err.Error(), defaultRepairMetadataKey)
	})

	t.Run("missing usage repair metadata", func(t *testing.T) {
		db := openTestDuckDB(t)
		require.NoError(t, EnsureSchema(ctx, db), "EnsureSchema")
		_, err := db.ExecContext(ctx,
			`DELETE FROM sync_metadata WHERE key = ?`,
			usageDedupIndexMetadataKey,
		)
		require.NoError(t, err)

		err = CheckSchemaCompat(ctx, db)
		require.Error(t, err)
		assert.Contains(t, err.Error(), usageDedupIndexMetadataKey)
	})

	t.Run("missing project identity remote scrub metadata", func(t *testing.T) {
		db := openTestDuckDB(t)
		require.NoError(t, EnsureSchema(ctx, db), "EnsureSchema")
		_, err := db.ExecContext(ctx,
			`DELETE FROM sync_metadata WHERE key = ?`,
			projectIdentityRemoteScrubMetadataKey,
		)
		require.NoError(t, err)

		err = CheckSchemaCompat(ctx, db)
		require.Error(t, err)
		assert.Contains(t, err.Error(), projectIdentityRemoteScrubMetadataKey)
	})

	t.Run("quack incompatible timestamp default", func(t *testing.T) {
		db := openTestDuckDB(t)
		require.NoError(t, EnsureSchema(ctx, db), "EnsureSchema")
		_, err := db.ExecContext(ctx,
			`ALTER TABLE starred_sessions ALTER COLUMN created_at SET DEFAULT current_timestamp`,
		)
		require.NoError(t, err)

		err = CheckSchemaCompat(ctx, db)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "starred_sessions.created_at")
	})
}

func TestEnsureSchemaScrubsProjectIdentityGitRemoteCredentials(t *testing.T) {
	ctx := context.Background()
	db := openTestDuckDB(t)
	require.NoError(t, EnsureSchema(ctx, db), "initial EnsureSchema")
	_, err := db.ExecContext(ctx,
		`DELETE FROM sync_metadata WHERE key = ?`,
		projectIdentityRemoteScrubMetadataKey,
	)
	require.NoError(t, err)
	rawRemote := "https://" + "user:token@" + "github.com/acme/app.git"
	storedRemote := "https://github.com/acme/app.git"
	_, err = db.ExecContext(ctx, `
		INSERT INTO source_project_identity_observations (
			project, machine, root_path, git_remote, git_remote_name,
			worktree_name, worktree_root_path, observed_at,
			normalized_remote, key_source, key
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"app", "laptop", "/tmp/app", rawRemote, "origin",
		"", "", "2026-07-03T12:00:00Z", "", "", "",
	)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `
		INSERT INTO source_project_identity_observations (
			project, machine, root_path, git_remote, git_remote_name,
			worktree_name, worktree_root_path, observed_at,
			normalized_remote, key_source, key
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"app", "laptop", "/tmp/app", "", "",
		"app", "/tmp/app", "2026-07-03T11:00:00Z", "", "", "",
	)
	require.NoError(t, err)

	require.NoError(t, EnsureSchema(ctx, db), "repair EnsureSchema")

	rows, err := db.QueryContext(ctx, `
		SELECT git_remote, normalized_remote, key_source, key
		FROM source_project_identity_observations
		WHERE project = ? AND machine = ? AND root_path = ?
		ORDER BY git_remote`,
		"app", "laptop", "/tmp/app",
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rows.Close()) })

	var got []export.ProjectIdentityObservation
	for rows.Next() {
		var obs export.ProjectIdentityObservation
		require.NoError(t, rows.Scan(
			&obs.GitRemote,
			&obs.NormalizedRemote,
			&obs.KeySource,
			&obs.Key,
		))
		got = append(got, obs)
	}
	require.NoError(t, rows.Err())
	require.Len(t, got, 1)
	assert.Equal(t, storedRemote, got[0].GitRemote)
	assert.Equal(t, "github.com/acme/app", got[0].NormalizedRemote)
	assert.Equal(t, export.ProjectIdentityKeySourceGitRemote, got[0].KeySource)
	assert.NotEmpty(t, got[0].Key)
}

func TestEnsureSchemaDropsQuackIncompatibleTimestampDefaults(t *testing.T) {
	ctx := context.Background()
	db := openTestDuckDB(t)
	_, err := db.ExecContext(ctx, `
		CREATE TABLE starred_sessions (
			session_id TEXT PRIMARY KEY,
			created_at TIMESTAMP NOT NULL DEFAULT current_timestamp
		)`)
	require.NoError(t, err)

	require.NoError(t, EnsureSchema(ctx, db), "EnsureSchema")

	assert.NotContains(
		t,
		strings.ToLower(columnDefaultValue(t, db, "starred_sessions", "created_at")),
		"current_timestamp",
	)
	_, err = db.ExecContext(ctx,
		`INSERT INTO starred_sessions (session_id, created_at)
		 VALUES (?, current_timestamp)`,
		"kept",
	)
	require.NoError(t, err)
}

func TestCheckSchemaCompatReportsMissingTablesAndColumns(t *testing.T) {
	ctx := context.Background()
	db := openTestDuckDB(t)

	err := CheckSchemaCompat(ctx, db)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing table sessions")

	db = openTestDuckDB(t)
	_, err = db.ExecContext(ctx,
		`CREATE TABLE sessions (
			id TEXT PRIMARY KEY,
			machine TEXT NOT NULL,
			project TEXT NOT NULL,
			agent TEXT NOT NULL
		)`,
	)
	require.NoError(t, err)

	err = CheckSchemaCompat(ctx, db)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sessions.secret_leak_count")
}

func TestCheckSchemaCompatPassesAfterEnsureSchema(t *testing.T) {
	ctx := context.Background()
	db := openTestDuckDB(t)

	require.NoError(t, EnsureSchema(ctx, db), "EnsureSchema")
	require.NoError(t, CheckSchemaCompat(ctx, db), "CheckSchemaCompat")
}

func TestCheckSchemaCompatViaQuackReportsServerBehindOnMissingColumns(
	t *testing.T,
) {
	ctx := context.Background()
	db := openTestDuckDB(t)
	_, err := db.ExecContext(ctx,
		`CREATE TABLE sessions (
			id TEXT PRIMARY KEY,
			machine TEXT NOT NULL,
			project TEXT NOT NULL,
			agent TEXT NOT NULL
		)`,
	)
	require.NoError(t, err, "simulate older server schema")

	err = CheckSchemaCompatViaQuack(ctx, db)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sessions.entrypoint")
	assert.Contains(t, err.Error(), "older AgentsView build",
		"remote incompat must point at the server build")
	assert.Contains(t, err.Error(), "upgrade and restart the DuckDB server",
		"remote incompat must tell the operator the fix")
	assert.NotContains(t, err.Error(),
		"run agentsview duckdb push to migrate",
		"push cannot migrate a remote server schema")
}

func TestCheckSchemaCompatKeepsLocalMigrationHintOnMissingColumns(
	t *testing.T,
) {
	ctx := context.Background()
	db := openTestDuckDB(t)
	_, err := db.ExecContext(ctx,
		`CREATE TABLE sessions (
			id TEXT PRIMARY KEY,
			machine TEXT NOT NULL,
			project TEXT NOT NULL,
			agent TEXT NOT NULL
		)`,
	)
	require.NoError(t, err, "simulate stale local mirror")

	err = CheckSchemaCompat(ctx, db)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sessions.entrypoint")
	assert.Contains(t, err.Error(), "run agentsview duckdb push to migrate",
		"local mirrors migrate through push")
	assert.NotContains(t, err.Error(), "older AgentsView build")
}

func TestCheckSchemaCompatViaQuackReportsServerBehindOnOldVersion(
	t *testing.T,
) {
	ctx := context.Background()
	db := openTestDuckDB(t)
	require.NoError(t, EnsureSchema(ctx, db), "EnsureSchema")
	_, err := db.ExecContext(ctx,
		`UPDATE sync_metadata SET value = '1' WHERE key = ?`,
		schemaVersionMetadataKey,
	)
	require.NoError(t, err, "simulate older server schema version")

	err = CheckSchemaCompatViaQuack(ctx, db)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "older than required")
	assert.Contains(t, err.Error(), "upgrade and restart the DuckDB server",
		"remote version skew must tell the operator the fix")
}

func TestCheckSchemaCompatViaQuackReportsServerBehindOnMissingVersionRow(
	t *testing.T,
) {
	ctx := context.Background()
	db := openTestDuckDB(t)
	require.NoError(t, EnsureSchema(ctx, db), "EnsureSchema")
	_, err := db.ExecContext(ctx,
		`DELETE FROM sync_metadata WHERE key = ?`,
		schemaVersionMetadataKey,
	)
	require.NoError(t, err, "simulate server without a schema version row")

	err = CheckSchemaCompatViaQuack(ctx, db)
	require.Error(t, err)
	assert.Contains(t, err.Error(), schemaVersionMetadataKey)
	assert.Contains(t, err.Error(), "upgrade and restart the DuckDB server",
		"remote missing version row must tell the operator the fix")
}

func TestSchemaRepairErrorPointsRemoteRepairsAtServer(t *testing.T) {
	require.NoError(t, schemaRepairError(nil, remoteSchema))

	err := schemaRepairError([]string{"messages.id primary key"}, remoteSchema)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "messages.id primary key")
	assert.Contains(t, err.Error(), "upgrade and restart the DuckDB server",
		"remote pending repairs must tell the operator the fix")
	assert.NotContains(t, err.Error(),
		"run full DuckDB schema migration on the base database")

	err = schemaRepairError([]string{"messages.id primary key"}, localSchema)
	require.Error(t, err)
	assert.Contains(t, err.Error(),
		"run full DuckDB schema migration on the base database",
		"local pending repairs keep the base-database hint")
}

// TestEnsureSchemaCreatesToolCallsFilePathIndex verifies the DuckDB mirror
// builds idx_tool_calls_file_path, the parity counterpart to SQLite's
// Recent Edits index. DuckDB has no partial indexes, so it omits the
// WHERE file_path IS NOT NULL clause but indexes the same column.
func TestEnsureSchemaCreatesToolCallsFilePathIndex(t *testing.T) {
	ctx := context.Background()
	db := openTestDuckDB(t)
	require.NoError(t, EnsureSchema(ctx, db), "EnsureSchema")

	var count int
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT count(*) FROM duckdb_indexes()
		WHERE table_name = 'tool_calls'
		  AND index_name = 'idx_tool_calls_file_path'`).Scan(&count),
		"query duckdb_indexes")
	assert.Equal(t, 1, count,
		"idx_tool_calls_file_path must exist for Recent Edits parity")
}

func openTestDuckDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := openDuckDB("")
	require.NoError(t, err, "open in-memory DuckDB")
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	require.NoError(t, configureDuckDBThreads(db))
	t.Cleanup(func() {
		require.NoError(t, db.Close(), "close DuckDB")
	})
	return db
}

func recreateMessagesWithIDPrimaryKey(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	_, err := db.ExecContext(ctx, `DROP TABLE messages`)
	require.NoError(t, err)
	create := strings.Replace(
		mirrorTableCreate("messages"),
		"id BIGINT,",
		"id BIGINT PRIMARY KEY,",
		1,
	)
	require.NotEqual(t, mirrorTableCreate("messages"), create)
	_, err = db.ExecContext(ctx, create)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `
		INSERT INTO messages (id, session_id, ordinal, role, content)
		VALUES (1, 'from-other-machine', 0, 'user', 'kept')`)
	require.NoError(t, err)
}

func tableExists(t *testing.T, db *sql.DB, table string) bool {
	t.Helper()
	var exists bool
	require.NoError(t, db.QueryRow(
		`SELECT count(*) > 0
		 FROM information_schema.tables
		 WHERE table_schema = current_schema()
		   AND table_name = ?`,
		strings.ToLower(table),
	).Scan(&exists))
	return exists
}

func columnExists(t *testing.T, db *sql.DB, table, column string) bool {
	t.Helper()
	var exists bool
	require.NoError(t, db.QueryRow(
		`SELECT count(*) > 0
		 FROM information_schema.columns
		 WHERE table_schema = current_schema()
		   AND table_name = ?
		   AND column_name = ?`,
		strings.ToLower(table),
		strings.ToLower(column),
	).Scan(&exists))
	return exists
}

func columnDefaultValue(t *testing.T, db *sql.DB, table, column string) string {
	t.Helper()
	value, err := columnDefault(context.Background(), db, table, column)
	require.NoError(t, err)
	return value
}
