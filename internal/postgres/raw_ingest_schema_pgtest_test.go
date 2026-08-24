//go:build pgtest

package postgres

import (
	"database/sql"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEnsureSchemaCreatesRawIngestCustodyTables(t *testing.T) {
	pgURL := testPGURL(t)
	cleanSchemaTestPG(t, pgURL)
	t.Cleanup(func() { cleanSchemaTestPG(t, pgURL) })
	pg, err := Open(pgURL, schemaTestSchema, true)
	require.NoError(t, err)
	defer pg.Close()

	require.NoError(t, EnsureSchema(t.Context(), pg, schemaTestSchema))
	require.NoError(t, EnsureSchema(t.Context(), pg, schemaTestSchema))

	for _, table := range []string{
		"raw_devices",
		"raw_device_tokens",
		"raw_upload_sessions",
		"raw_objects",
		"raw_manifests",
		"raw_manifest_entries",
		"raw_manifest_objects",
		"raw_source_heads",
		"raw_ingest_jobs",
	} {
		var exists bool
		err := pg.QueryRowContext(t.Context(), `
			SELECT EXISTS (
				SELECT 1 FROM information_schema.tables
				WHERE table_schema = $1 AND table_name = $2
			)`, schemaTestSchema, table).Scan(&exists)
		require.NoError(t, err)
		assert.True(t, exists, table)
	}

	for _, index := range []string{
		"idx_raw_device_tokens_expiry",
		"idx_raw_upload_sessions_expiry",
		"idx_raw_upload_sessions_open_object",
		"idx_raw_ingest_jobs_ready",
		"idx_raw_ingest_jobs_lease",
	} {
		var exists bool
		err := pg.QueryRowContext(t.Context(), `
			SELECT EXISTS (
				SELECT 1 FROM pg_indexes
				WHERE schemaname = $1 AND indexname = $2
			)`, schemaTestSchema, index).Scan(&exists)
		require.NoError(t, err)
		assert.True(t, exists, index)
	}
}

func TestRawIngestSchemaUsesTenantScopedKeys(t *testing.T) {
	pgURL := testPGURL(t)
	cleanSchemaTestPG(t, pgURL)
	t.Cleanup(func() { cleanSchemaTestPG(t, pgURL) })
	pg, err := Open(pgURL, schemaTestSchema, true)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, pg.Close()) })
	require.NoError(t, EnsureSchema(t.Context(), pg, schemaTestSchema))

	wantDefinitions := map[string][]string{
		"raw_devices": {
			"PRIMARY KEY (device_id)",
			"UNIQUE (tenant_id, device_id)",
		},
		"raw_device_tokens": {
			"PRIMARY KEY (token_sha256)",
			"FOREIGN KEY (tenant_id, device_id)",
		},
		"raw_upload_sessions": {
			"PRIMARY KEY (upload_id)",
			"FOREIGN KEY (tenant_id, device_id)",
		},
		"raw_objects": {
			"PRIMARY KEY (tenant_id, sha256)",
		},
		"raw_manifests": {
			"PRIMARY KEY (tenant_id, manifest_id)",
			"UNIQUE (tenant_id, device_id, provider, configured_root_id, source_key_sha256, generation)",
			"UNIQUE (tenant_id, device_id, provider, configured_root_id, source_key_sha256, capture_id)",
		},
		"raw_manifest_entries": {
			"PRIMARY KEY (tenant_id, manifest_id, entry_index)",
			"UNIQUE (tenant_id, manifest_id, path_sha256)",
			"FOREIGN KEY (tenant_id, manifest_id)",
		},
		"raw_manifest_objects": {
			"PRIMARY KEY (tenant_id, manifest_id, entry_index, object_index)",
			"FOREIGN KEY (tenant_id, manifest_id, entry_index)",
			"FOREIGN KEY (tenant_id, sha256, size_bytes)",
		},
		"raw_source_heads": {
			"PRIMARY KEY (tenant_id, device_id, provider, configured_root_id, source_key_sha256)",
			"FOREIGN KEY (tenant_id, manifest_id)",
		},
		"raw_ingest_jobs": {
			"UNIQUE (tenant_id, manifest_id, stage, processing_version)",
			"FOREIGN KEY (tenant_id, manifest_id)",
		},
	}
	for table, wanted := range wantDefinitions {
		rows, err := pg.QueryContext(t.Context(), `
			SELECT pg_get_constraintdef(oid)
			FROM pg_constraint
			WHERE conrelid = $1::regclass`, schemaTestSchema+"."+table)
		require.NoError(t, err)
		var definitions []string
		for rows.Next() {
			var definition string
			require.NoError(t, rows.Scan(&definition))
			definitions = append(definitions, definition)
		}
		require.NoError(t, rows.Err())
		require.NoError(t, rows.Close())
		joined := strings.Join(definitions, "\n")
		for _, definition := range wanted {
			assert.Contains(t, joined, definition, table)
		}
	}
}

func TestRawIngestSchemaEnforcesCaptureAndObjectCustody(t *testing.T) {
	pgURL := testPGURL(t)
	cleanSchemaTestPG(t, pgURL)
	t.Cleanup(func() { cleanSchemaTestPG(t, pgURL) })
	pg, err := Open(pgURL, schemaTestSchema, true)
	require.NoError(t, err)
	defer pg.Close()
	require.NoError(t, EnsureSchema(t.Context(), pg, schemaTestSchema))

	insertManifest := `
		INSERT INTO raw_manifests (
			tenant_id, manifest_id, device_id, provider, configured_root_id,
			source_key, source_key_sha256, capture_id, parent_receipt, receipt,
			generation, kind, captured_at, canonical_json
		) VALUES ($1, $2, 'device-a', 'codex', 'root-a', 'source-a', $6,
			'capture-a', '', $3, 1, 'snapshot', $4, $5)`
	firstManifest := strings.Repeat("a", 64)
	sourceKeyDigest := rawIngestKeyDigest("source-a")
	_, err = pg.ExecContext(t.Context(), insertManifest,
		"tenant-a", firstManifest, repeatedHex("b"),
		time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC), []byte("{}"), sourceKeyDigest,
	)
	require.NoError(t, err)

	_, duplicateErr := pg.ExecContext(t.Context(), insertManifest,
		"tenant-a", repeatedHex("c"), repeatedHex("d"),
		time.Date(2026, 8, 13, 12, 0, 1, 0, time.UTC), []byte("{}"), sourceKeyDigest,
	)
	assert.Error(t, duplicateErr,
		"one authenticated source capture id must identify only one manifest")

	_, err = pg.ExecContext(t.Context(), `
		INSERT INTO raw_manifest_entries (
			tenant_id, manifest_id, entry_index, path, path_sha256, entry_type,
			size_bytes
		) VALUES ($1, $2, 0, 'session.jsonl', $3, 'file', 7)`,
		"tenant-a", firstManifest, rawIngestKeyDigest("session.jsonl"))
	require.NoError(t, err)
	_, err = pg.ExecContext(t.Context(), `
		INSERT INTO raw_manifest_objects (
			tenant_id, manifest_id, entry_index, object_index, sha256, size_bytes
		) VALUES ($1, $2, 0, 0, $3, 7)`,
		"tenant-a", firstManifest, repeatedHex("e"))
	assert.Error(t, err,
		"an accepted manifest reference cannot point at an unverified raw object")
}

func TestRawIngestSchemaMakesAcceptedManifestGraphAppendOnly(t *testing.T) {
	pg, store := newRawIngestTestStore(t)
	identity := rawIngestIdentity(t, "tenant-a")
	object := rawIngestObject(t, "a", 7)
	require.NoError(t, store.RecordVerifiedObject(t.Context(), identity, object))
	manifest := rawIngestManifest(
		t, identity, "capture-a", "", rawIngestCapturedAt(), object,
	)
	_, err := store.CommitManifest(t.Context(), manifest, "parser-data-17")
	require.NoError(t, err)

	mutations := []string{
		`UPDATE raw_manifests SET canonical_json = '{}'`,
		`DELETE FROM raw_manifests`,
		`UPDATE raw_manifest_entries SET path = 'changed.jsonl'`,
		`DELETE FROM raw_manifest_entries`,
		`UPDATE raw_manifest_objects SET object_index = 1`,
		`DELETE FROM raw_manifest_objects`,
	}
	for _, mutation := range mutations {
		_, err := pg.ExecContext(t.Context(), mutation)
		var pgErr *pgconn.PgError
		require.ErrorAs(t, err, &pgErr, mutation)
		assert.Equal(t, "55000", pgErr.Code, mutation)
	}
	assert.Equal(t,
		rawIngestCounts{Manifests: 1, Entries: 1, Objects: 1, Heads: 1, Jobs: 1},
		readRawIngestCounts(t, pg),
	)
}

func TestSyncEnsureSchemaCreatesRawCustodyOnLegacyFastPath(t *testing.T) {
	pgURL := testPGURL(t)
	cleanSchemaTestPG(t, pgURL)
	t.Cleanup(func() { cleanSchemaTestPG(t, pgURL) })
	pg, err := Open(pgURL, schemaTestSchema, true)
	require.NoError(t, err)
	defer pg.Close()
	require.NoError(t, EnsureSchema(t.Context(), pg, schemaTestSchema))

	_, err = pg.ExecContext(t.Context(), `
		DROP TABLE raw_ingest_jobs, raw_source_heads, raw_manifest_objects,
			raw_manifest_entries, raw_manifests, raw_objects`)
	require.NoError(t, err)
	require.True(t, pushSchemaCurrent(t.Context(), pg),
		"fixture must exercise the schema-current sync fast path")

	syncer := &Sync{pg: pg, schema: schemaTestSchema}
	require.NoError(t, syncer.EnsureSchema(t.Context()))

	var exists bool
	require.NoError(t, pg.QueryRowContext(t.Context(), `
		SELECT EXISTS (
			SELECT 1 FROM information_schema.tables
			WHERE table_schema = $1 AND table_name = 'raw_objects'
		)`, schemaTestSchema).Scan(&exists))
	assert.True(t, exists,
		"schema-current sync must still install newly introduced custody tables")
}

// TestSyncEnsureSchemaFastPathToleratesRestrictedRole pins the restricted-role
// push path: a privileged role provisioned the schema, including the raw
// custody tables, but the push role cannot CREATE. PostgreSQL still checks
// CREATE for CREATE TABLE IF NOT EXISTS, so the fast path must skip raw
// custody DDL on SQLSTATE 42501 rather than fail every push.
func TestSyncEnsureSchemaFastPathToleratesRestrictedRole(t *testing.T) {
	pgURL := testPGURL(t)
	const schema = "agentsview_raw_privilege_test"
	const role = "agentsview_raw_restricted"
	const rolePassword = "agentsview_raw_restricted_pw"

	admin, err := Open(pgURL, schema, true)
	require.NoError(t, err, "Open admin")
	t.Cleanup(func() { _ = admin.Close() })
	_, err = admin.Exec(`DROP SCHEMA IF EXISTS ` + schema + ` CASCADE`)
	require.NoError(t, err, "drop schema")
	require.NoError(t, EnsureSchema(t.Context(), admin, schema))
	require.True(t, pushSchemaCurrent(t.Context(), admin),
		"fixture must exercise the schema-current sync fast path")

	_, _ = admin.Exec(`DROP OWNED BY ` + role)
	_, _ = admin.Exec(`DROP ROLE IF EXISTS ` + role)
	_, err = admin.Exec(`CREATE ROLE ` + role + ` LOGIN PASSWORD '` + rolePassword + `'`)
	require.NoError(t, err, "create restricted role")
	t.Cleanup(func() {
		_, _ = admin.Exec(`DROP SCHEMA IF EXISTS ` + schema + ` CASCADE`)
		_, _ = admin.Exec(`DROP OWNED BY ` + role)
		_, _ = admin.Exec(`DROP ROLE IF EXISTS ` + role)
	})
	for _, grant := range []string{
		`GRANT USAGE ON SCHEMA ` + schema + ` TO ` + role,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA ` + schema + ` TO ` + role,
		`GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA ` + schema + ` TO ` + role,
	} {
		_, err = admin.Exec(grant)
		require.NoError(t, err, grant)
	}

	restrictedURL, err := url.Parse(pgURL)
	require.NoError(t, err)
	restrictedURL.User = url.UserPassword(role, rolePassword)
	restricted, err := Open(restrictedURL.String(), schema, true)
	require.NoError(t, err, "Open restricted")
	t.Cleanup(func() { _ = restricted.Close() })

	syncer := &Sync{pg: restricted, schema: schema}
	require.NoError(t, syncer.EnsureSchema(t.Context()),
		"restricted push role must not fail on raw custody DDL")
	assert.True(t, syncer.schemaDone)
	writable, err := CanWriteRawSyncSchema(t.Context(), restricted, schema)
	require.NoError(t, err)
	assert.True(t, writable,
		"DML-capable role must remain eligible when raw custody DDL is restricted")
}

func TestCanWriteRawSyncSchemaRequiresExactRuntimePrivileges(t *testing.T) {
	pgURL := testPGURL(t)
	const schema = "agentsview_raw_runtime_privilege_test"
	const role = "agentsview_raw_runtime"
	const rolePassword = "agentsview_raw_runtime_pw"

	admin, err := Open(pgURL, schema, true)
	require.NoError(t, err, "Open admin")
	t.Cleanup(func() { _ = admin.Close() })
	_, err = admin.Exec(`DROP SCHEMA IF EXISTS ` + schema + ` CASCADE`)
	require.NoError(t, err, "drop schema")
	require.NoError(t, EnsureSchema(t.Context(), admin, schema))

	_, _ = admin.Exec(`DROP OWNED BY ` + role)
	_, _ = admin.Exec(`DROP ROLE IF EXISTS ` + role)
	_, err = admin.Exec(`CREATE ROLE ` + role + ` LOGIN PASSWORD '` + rolePassword + `'`)
	require.NoError(t, err, "create runtime role")
	t.Cleanup(func() {
		_, _ = admin.Exec(`DROP SCHEMA IF EXISTS ` + schema + ` CASCADE`)
		_, _ = admin.Exec(`DROP OWNED BY ` + role)
		_, _ = admin.Exec(`DROP ROLE IF EXISTS ` + role)
	})
	for _, grant := range []string{
		`GRANT USAGE ON SCHEMA ` + schema + ` TO ` + role,
		`GRANT SELECT ON ` + schema + `.raw_devices TO ` + role,
		`GRANT SELECT, INSERT ON ` + schema + `.raw_device_tokens TO ` + role,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON ` + schema + `.raw_upload_sessions TO ` + role,
		`GRANT SELECT, INSERT, UPDATE ON ` + schema + `.raw_objects TO ` + role,
		`GRANT SELECT, INSERT ON ` + schema + `.raw_manifests TO ` + role,
		`GRANT INSERT ON ` + schema + `.raw_manifest_entries TO ` + role,
		`GRANT INSERT ON ` + schema + `.raw_manifest_objects TO ` + role,
		`GRANT SELECT, INSERT, UPDATE ON ` + schema + `.raw_source_heads TO ` + role,
		`GRANT INSERT ON ` + schema + `.raw_ingest_jobs TO ` + role,
		`GRANT USAGE ON SEQUENCE ` + schema + `.raw_ingest_jobs_id_seq TO ` + role,
	} {
		_, err = admin.Exec(grant)
		require.NoError(t, err, grant)
	}

	restrictedURL, err := url.Parse(pgURL)
	require.NoError(t, err)
	restrictedURL.User = url.UserPassword(role, rolePassword)
	restricted, err := Open(restrictedURL.String(), schema, true)
	require.NoError(t, err, "Open runtime role")
	t.Cleanup(func() { _ = restricted.Close() })

	writable, err := CanWriteRawSyncSchema(t.Context(), restricted, schema)
	require.NoError(t, err)
	assert.True(t, writable)

	requiredTablePrivileges := []struct {
		table     string
		privilege string
	}{
		{"raw_devices", "SELECT"},
		{"raw_device_tokens", "SELECT"},
		{"raw_device_tokens", "INSERT"},
		{"raw_upload_sessions", "SELECT"},
		{"raw_upload_sessions", "INSERT"},
		{"raw_upload_sessions", "UPDATE"},
		{"raw_upload_sessions", "DELETE"},
		{"raw_objects", "SELECT"},
		{"raw_objects", "INSERT"},
		{"raw_objects", "UPDATE"},
		{"raw_manifests", "SELECT"},
		{"raw_manifests", "INSERT"},
		{"raw_manifest_entries", "INSERT"},
		{"raw_manifest_objects", "INSERT"},
		{"raw_source_heads", "SELECT"},
		{"raw_source_heads", "INSERT"},
		{"raw_source_heads", "UPDATE"},
		{"raw_ingest_jobs", "INSERT"},
	}
	for _, required := range requiredTablePrivileges {
		t.Run(required.table+"_"+strings.ToLower(required.privilege), func(t *testing.T) {
			revoke := `REVOKE ` + required.privilege + ` ON ` +
				schema + `.` + required.table + ` FROM ` + role
			grant := `GRANT ` + required.privilege + ` ON ` +
				schema + `.` + required.table + ` TO ` + role
			_, err := admin.Exec(revoke)
			require.NoError(t, err, revoke)
			t.Cleanup(func() {
				_, cleanupErr := admin.Exec(grant)
				require.NoError(t, cleanupErr, grant)
			})

			writable, err := CanWriteRawSyncSchema(t.Context(), restricted, schema)
			require.NoError(t, err)
			assert.False(t, writable,
				"runtime role must have %s on %s", required.privilege, required.table)
		})
	}
}

func TestCanWriteRawSyncSchemaAcceptsSequenceFreeJobIDs(t *testing.T) {
	pgURL := testPGURL(t)
	const schema = "agentsview_raw_sequence_free_test"

	pg, err := Open(pgURL, schema, true)
	require.NoError(t, err)
	t.Cleanup(func() { _ = pg.Close() })
	_, err = pg.Exec(`DROP SCHEMA IF EXISTS ` + schema + ` CASCADE`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = pg.Exec(`DROP SCHEMA IF EXISTS ` + schema + ` CASCADE`)
	})
	require.NoError(t, EnsureSchema(t.Context(), pg, schema))

	var sequenceName sql.NullString
	err = pg.QueryRow(`SELECT pg_get_serial_sequence(
		$1, 'id'
	)`, schema+".raw_ingest_jobs").Scan(&sequenceName)
	require.NoError(t, err)
	if sequenceName.Valid {
		_, err = pg.Exec(`ALTER TABLE ` + schema + `.raw_ingest_jobs
			ALTER COLUMN id SET DEFAULT (
				EXTRACT(EPOCH FROM clock_timestamp()) * 1000000
			)::BIGINT`)
		require.NoError(t, err)
		_, err = pg.Exec(`DROP SEQUENCE ` + schema + `.raw_ingest_jobs_id_seq`)
		require.NoError(t, err)
	}

	writable, err := CanWriteRawSyncSchema(t.Context(), pg, schema)
	require.NoError(t, err)
	assert.True(t, writable)
}

func repeatedHex(value string) string {
	return strings.Repeat(value, 64)
}
