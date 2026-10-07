package rawcheckpoint

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawsync"
)

// downgradeToVersionNine strips what version 10 added so the next Open
// replays that migration against otherwise current state.
func downgradeToVersionNine(t *testing.T, store *Store) {
	t.Helper()
	for _, statement := range []string{
		`DROP INDEX raw_source_base_objects_object_idx`,
		`PRAGMA user_version = 9`,
	} {
		_, err := store.db.ExecContext(t.Context(), statement)
		require.NoError(t, err)
	}
}

// openCheckpointForTest closes the store when the test ends. Close is
// idempotent, so a test may close it earlier to reopen the same path.
func openCheckpointForTest(t *testing.T, path string) *Store {
	t.Helper()
	store, err := Open(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	return store
}

func queryPlanDetails(t *testing.T, db *sql.DB, query string, args ...any) []string {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), `EXPLAIN QUERY PLAN `+query, args...)
	require.NoError(t, err)
	defer rows.Close()
	var details []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		require.NoError(t, rows.Scan(&id, &parent, &unused, &detail))
		details = append(details, detail)
	}
	require.NoError(t, rows.Err())
	return details
}

// checkpointRows renders every row of every table so a migration can be
// shown to leave transport state untouched.
func checkpointRows(t *testing.T, db *sql.DB) map[string][]string {
	t.Helper()
	tables := queryStrings(t, db,
		`SELECT name FROM sqlite_master WHERE type = 'table' ORDER BY name`)
	dump := make(map[string][]string, len(tables))
	for _, table := range tables {
		dump[table] = tableRows(t, db, table)
	}
	return dump
}

func tableRows(t *testing.T, db *sql.DB, table string) []string {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), `SELECT * FROM `+table+` ORDER BY rowid`)
	require.NoError(t, err)
	defer rows.Close()
	columns, err := rows.Columns()
	require.NoError(t, err)
	rendered := []string{}
	for rows.Next() {
		values := make([]any, len(columns))
		targets := make([]any, len(columns))
		for i := range values {
			targets[i] = &values[i]
		}
		require.NoError(t, rows.Scan(targets...))
		rendered = append(rendered, fmt.Sprint(values...))
	}
	require.NoError(t, rows.Err())
	return rendered
}

func queryStrings(t *testing.T, db *sql.DB, query string) []string {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), query)
	require.NoError(t, err)
	defer rows.Close()
	var values []string
	for rows.Next() {
		var value string
		require.NoError(t, rows.Scan(&value))
		values = append(values, value)
	}
	require.NoError(t, rows.Err())
	return values
}

// schemaDefinitions maps each schema object to its whitespace-normalized SQL
// so checkpoints built by different paths can be compared definition by
// definition.
func schemaDefinitions(t *testing.T, db *sql.DB) map[string]string {
	t.Helper()
	rows, err := db.QueryContext(t.Context(),
		`SELECT type, name, sql FROM sqlite_master WHERE sql IS NOT NULL`)
	require.NoError(t, err)
	defer rows.Close()
	objects := map[string]string{}
	for rows.Next() {
		var kind, name, definition string
		require.NoError(t, rows.Scan(&kind, &name, &definition))
		normalized := strings.Join(strings.Fields(definition), " ")
		objects[kind+" "+name] = strings.ReplaceAll(normalized, " )", ")")
	}
	require.NoError(t, rows.Err())
	return objects
}

// indexAndTriggerDefinitions keeps only the index and trigger definitions.
func indexAndTriggerDefinitions(definitions map[string]string) map[string]string {
	kept := map[string]string{}
	for object, definition := range definitions {
		if strings.HasPrefix(object, "index ") || strings.HasPrefix(object, "trigger ") {
			kept[object] = definition
		}
	}
	return kept
}

func acknowledgeNextTestGeneration(t *testing.T, store *Store, commit rawsync.CommitResult) string {
	t.Helper()
	manifest, found, err := store.FinalizeNextManifest(t.Context(), "device-a")
	require.NoError(t, err)
	require.True(t, found)
	require.NoError(t, store.BindFinalizedCommit(
		t.Context(), "device-a", manifest.CaptureID, commit,
	))
	_, err = store.AcknowledgeGeneration(t.Context(), "device-a", manifest.CaptureID, commit)
	require.NoError(t, err)
	return manifest.CaptureID
}

func commitTestGeneration(
	t *testing.T,
	store *Store,
	sequence int,
	root ConfiguredRoot,
	sourceKey, predecessor string,
	ref rawsync.ObjectRef,
) CapturedGeneration {
	t.Helper()
	generation := testCapturedGeneration(sequence, root, predecessor, ref)
	generation.Source.SourceKey = sourceKey
	reservation, err := store.ReserveSourceCapture(t.Context(), generation.Source, 1793)
	require.NoError(t, err)
	require.NoError(t, store.CommitCapture(t.Context(), reservation.ID, generation))
	return generation
}

func TestBaseObjectReferenceLookupsSeekByObject(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(*testing.T, *Store, string) *Store
	}{
		{
			name:    "fresh checkpoint",
			prepare: func(_ *testing.T, store *Store, _ string) *Store { return store },
		},
		{
			name: "checkpoint upgraded from version 9",
			prepare: func(t *testing.T, store *Store, path string) *Store {
				t.Helper()
				downgradeToVersionNine(t, store)
				require.NoError(t, store.Close())
				return openCheckpointForTest(t, path)
			},
		},
		{
			name: "many sources with planner statistics",
			prepare: func(t *testing.T, store *Store, _ string) *Store {
				t.Helper()
				root, err := store.ResolveConfiguredRoot(
					t.Context(), parser.AgentClaude, t.TempDir(),
				)
				require.NoError(t, err)
				tx, err := store.db.BeginTx(t.Context(), nil)
				require.NoError(t, err)
				defer func() { _ = tx.Rollback() }()
				for source := range 200 {
					key := fmt.Sprintf("source-%03d", source)
					for entry := range 20 {
						digest := fmt.Sprintf("%064x", source*1000+entry)
						_, err := tx.ExecContext(t.Context(), `INSERT INTO raw_source_base_entries
							(provider, configured_root_id, source_key, entry_ordinal, path,
							 length, mod_time_ns, file_identity, prefix_sha256, appendable)
							VALUES ('claude', ?, ?, ?, ?, 1, 0, 'identity', ?, 0)`,
							root.ID, key, entry, fmt.Sprintf("sidecar-%02d.json", entry), digest)
						require.NoError(t, err)
						_, err = tx.ExecContext(t.Context(), `INSERT INTO raw_source_base_objects
							(provider, configured_root_id, source_key, entry_ordinal,
							 object_ordinal, sha256, length) VALUES ('claude', ?, ?, ?, 0, ?, 1)`,
							root.ID, key, entry, digest)
						require.NoError(t, err)
						_, err = tx.ExecContext(t.Context(), `INSERT INTO outbox_objects
							(sha256, length, spool_name, ref_count, state, created_at)
							VALUES (?, 1, ?, 0, 'remote', '2026-08-25T00:00:00Z')`,
							digest, "objects/"+digest)
						require.NoError(t, err)
					}
				}
				require.NoError(t, tx.Commit())
				_, err = store.db.ExecContext(t.Context(), `ANALYZE`)
				require.NoError(t, err)
				return store
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "checkpoint.db")
			store := openCheckpointForTest(t, path)
			store = tt.prepare(t, store, path)

			var version int
			require.NoError(t, store.db.QueryRowContext(
				t.Context(), `PRAGMA user_version`).Scan(&version))
			assert.Equal(t, schemaVersion, version)

			lookups := []struct {
				name, query, table string
				args               []any
			}{
				{
					name:  "acknowledgement prune",
					query: pruneUnreferencedRemoteObjectsSQL, table: "base",
				},
				{
					name:  "garbage collection retention check",
					query: baseReferencesObjectSQL, table: "raw_source_base_objects",
					args: []any{validCheckpointDigest(1), 1},
				},
			}
			for _, lookup := range lookups {
				plan := strings.Join(
					queryPlanDetails(t, store.db, lookup.query, lookup.args...), "\n")
				assert.Contains(t, plan,
					"SEARCH "+lookup.table+" USING COVERING INDEX", lookup.name)
				assert.Contains(t, plan, "(sha256=? AND length=?)", lookup.name)
				assert.NotContains(t, plan, "SCAN "+lookup.table, lookup.name)
			}
		})
	}
}

func TestVersionNineUpgradePreservesTransportState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "checkpoint.db")
	store := openCheckpointForTest(t, path)
	require.NoError(t, store.SetDevice(t.Context(), "device-a"))
	root, err := store.ResolveConfiguredRoot(t.Context(), parser.AgentClaude, t.TempDir())
	require.NoError(t, err)
	_, err = store.BeginBackfill(t.Context(), BackfillRunSpec{
		RunID: "run-a", DeviceID: "device-a", Destination: "https://ingest.example",
		Providers: []parser.AgentType{parser.AgentClaude},
		Roots: []BackfillSelection{{
			Provider: parser.AgentClaude, ConfiguredRootID: root.ID,
		}},
	})
	require.NoError(t, err)
	acknowledged := rawsync.ObjectRef{SHA256: validCheckpointDigest(1), Length: 1}
	pending := rawsync.ObjectRef{SHA256: validCheckpointDigest(2), Length: 1}
	installOutboxTestObject(t, store, acknowledged, []byte("a"))
	installOutboxTestObject(t, store, pending, []byte("p"))
	uploaded := commitTestGeneration(t, store, 1, root, "source-uploaded", "", acknowledged)
	receipt := rawsync.CommitResult{
		ManifestID: validCheckpointDigest(3), Receipt: validCheckpointDigest(4),
		Generation: 1, Created: true,
	}
	acknowledgeNextTestGeneration(t, store, receipt)
	queued := commitTestGeneration(t, store, 2, root, "source-queued", "", pending)
	before := checkpointRows(t, store.db)
	require.NotEmpty(t, before["raw_source_base_objects"])
	require.NotEmpty(t, before["outbox_entry_objects"])
	require.NotEmpty(t, before["backfill_runs"])

	downgradeToVersionNine(t, store)
	require.NoError(t, store.Close())
	store = openCheckpointForTest(t, path)

	assert.Equal(t, before, checkpointRows(t, store.db))
	device, found, err := store.Device(t.Context())
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "device-a", device)
	head, found, err := store.SourceHead(
		t.Context(), uploaded.Source.Provider, root.ID, uploaded.Source.SourceKey)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, receipt.Receipt, head.Receipt)
	assert.Equal(t, receipt.Generation, head.Generation)
	assert.FileExists(t, store.ObjectPath(pending))
	next := rawsync.CommitResult{
		ManifestID: validCheckpointDigest(5), Receipt: validCheckpointDigest(6),
		Generation: 1, Created: true,
	}
	assert.Equal(t, queued.CaptureID, acknowledgeNextTestGeneration(t, store, next))
}

func TestFreshAndUpgradedCheckpointsDefineTheSameIndexesAndTriggers(t *testing.T) {
	fresh, err := Open(t.Context(), filepath.Join(t.TempDir(), "fresh.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, fresh.Close()) })
	legacyPath := filepath.Join(t.TempDir(), "legacy.db")
	createVersionOneCheckpoint(t, legacyPath)
	upgraded, err := Open(t.Context(), legacyPath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, upgraded.Close()) })

	// Shipped migrations leave out CHECK constraints the fresh tables carry,
	// so only indexes and triggers must match definition for definition.
	freshDefinitions := indexAndTriggerDefinitions(schemaDefinitions(t, fresh.db))
	upgradedDefinitions := indexAndTriggerDefinitions(schemaDefinitions(t, upgraded.db))

	assert.Contains(t, freshDefinitions, "index raw_source_base_objects_object_idx")
	assert.Equal(t, freshDefinitions, upgradedDefinitions)
}

func TestUploadedObjectIsForgottenOnlyAfterLastAcknowledgedBaseDropsIt(t *testing.T) {
	store, root := openOutboxTestStore(t, 1<<20)
	require.NoError(t, store.SetDevice(t.Context(), "device-a"))
	shared := rawsync.ObjectRef{SHA256: validCheckpointDigest(1), Length: 1}
	replacementA := rawsync.ObjectRef{SHA256: validCheckpointDigest(2), Length: 1}
	replacementB := rawsync.ObjectRef{SHA256: validCheckpointDigest(3), Length: 1}
	for i, ref := range []rawsync.ObjectRef{shared, replacementA, replacementB} {
		installOutboxTestObject(t, store, ref, []byte{byte(i)})
	}
	commit := func(sequence int, generation int64) rawsync.CommitResult {
		return rawsync.CommitResult{
			ManifestID: validCheckpointDigest(byte(4 + 2*sequence)),
			Receipt:    validCheckpointDigest(byte(5 + 2*sequence)),
			Generation: generation, Created: true,
		}
	}
	objectState := func() string {
		var state string
		err := store.db.QueryRowContext(t.Context(), `SELECT state FROM outbox_objects
			WHERE sha256 = ? AND length = ?`, shared.SHA256, shared.Length).Scan(&state)
		if errors.Is(err, sql.ErrNoRows) {
			return "forgotten"
		}
		require.NoError(t, err)
		return state
	}
	firstA := commitTestGeneration(t, store, 1, root, "source-a", "", shared)
	firstB := commitTestGeneration(t, store, 2, root, "source-b", "", shared)
	acknowledgeNextTestGeneration(t, store, commit(0, 1))
	assert.Equal(t, "live", objectState(), "source-b has not uploaded its reference yet")
	acknowledgeNextTestGeneration(t, store, commit(1, 1))
	assert.Equal(t, "remote", objectState())
	assert.NoFileExists(t, store.ObjectPath(shared))

	commitTestGeneration(t, store, 3, root, "source-a", firstA.CaptureID, replacementA)
	acknowledgeNextTestGeneration(t, store, commit(2, 2))

	assert.Equal(t, "remote", objectState(), "source-b's acknowledged base still references it")

	commitTestGeneration(t, store, 4, root, "source-b", firstB.CaptureID, replacementB)
	acknowledgeNextTestGeneration(t, store, commit(3, 2))

	assert.Equal(t, "forgotten", objectState())
}
