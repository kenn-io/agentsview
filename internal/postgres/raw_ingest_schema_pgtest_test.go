//go:build pgtest

package postgres

import (
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

func repeatedHex(value string) string {
	return strings.Repeat(value, 64)
}
