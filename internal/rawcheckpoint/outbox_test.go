package rawcheckpoint

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawsync"
)

func createVersionOneCheckpoint(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open(checkpointDriverName, checkpointDSN(path, false))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.Exec(`CREATE TABLE device_config (
		id INTEGER PRIMARY KEY CHECK (id = 1),
		device_id TEXT NOT NULL,
		created_at TEXT NOT NULL
	)`)
	require.NoError(t, err)
	_, err = db.Exec(`CREATE TABLE raw_sources (
		provider TEXT NOT NULL,
		configured_root_id TEXT NOT NULL,
		source_key TEXT NOT NULL,
		head_manifest_id TEXT NOT NULL DEFAULT '',
		head_receipt TEXT NOT NULL DEFAULT '',
		head_generation INTEGER NOT NULL DEFAULT 0,
		updated_at TEXT NOT NULL,
		PRIMARY KEY (provider, configured_root_id, source_key)
	)`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO device_config (id, device_id, created_at)
		VALUES (1, 'dev_existing', '2026-08-25T00:00:00Z')`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO raw_sources
		(provider, configured_root_id, source_key, head_manifest_id,
		 head_receipt, head_generation, updated_at)
		VALUES ('claude', 'root-existing', 'source-existing',
		 ?, ?, 1, '2026-08-25T00:00:00Z')`,
		validCheckpointDigest(2), validCheckpointDigest(3))
	require.NoError(t, err)
	_, err = db.Exec(`PRAGMA user_version = 1`)
	require.NoError(t, err)
	require.NoError(t, db.Close())
}

func validCheckpointDigest(value byte) string {
	const hex = "0123456789abcdef"
	result := make([]byte, 64)
	for i := range result {
		result[i] = hex[value%16]
	}
	return string(result)
}

func TestOpenWithOptionsMigratesVersionOneAndEnforcesForeignKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "checkpoint.db")
	createVersionOneCheckpoint(t, path)
	store, err := OpenWithOptions(t.Context(), path, Options{
		SpoolDir:       filepath.Join(t.TempDir(), "spool"),
		MaxOutboxBytes: 1 << 20,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })

	deviceID, ok, err := store.Device(t.Context())
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, "dev_existing", deviceID)
	head, ok, err := store.SourceHead(
		t.Context(), parser.AgentClaude, "root-existing", "source-existing",
	)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, int64(1), head.Generation)
	assert.Equal(t, validCheckpointDigest(2), head.ManifestID)
	assert.Equal(t, validCheckpointDigest(3), head.Receipt)

	var version, foreignKeys int
	require.NoError(t, store.db.QueryRow("PRAGMA user_version").Scan(&version))
	require.NoError(t, store.db.QueryRow("PRAGMA foreign_keys").Scan(&foreignKeys))
	assert.Equal(t, 5, version)
	assert.Equal(t, 1, foreignKeys)

	_, err = store.db.Exec(`INSERT INTO outbox_entries
		(capture_id, entry_ordinal, path, length, mod_time_ns,
		 file_identity, prefix_sha256, appendable)
		VALUES ('missing-capture', 0, 'session.jsonl', 1, 0, '', '', 1)`)
	require.Error(t, err, "foreign keys must reject an orphaned outbox entry")
}

func TestOpenWithOptionsCompactsVersionThreeTerminalGenerations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "checkpoint.db")
	spoolDir := filepath.Join(t.TempDir(), "spool")
	db, err := sql.Open(checkpointDriverName, checkpointDSN(path, false))
	require.NoError(t, err)
	for _, statements := range [][]string{
		versionOneSchemaStatements,
		versionTwoMigrationStatements,
		versionThreeMigrationStatements,
	} {
		for _, statement := range statements {
			_, err = db.Exec(statement)
			require.NoError(t, err)
		}
	}
	const timestamp = "2026-08-25T00:00:00Z"
	_, err = db.Exec(`INSERT INTO configured_roots
		(id, provider, local_root, created_at, updated_at)
		VALUES ('root-a', 'claude', '/capture', ?, ?)`, timestamp, timestamp)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO raw_sources
		(provider, configured_root_id, source_key, head_manifest_id,
		 head_receipt, head_generation, latest_capture_id, updated_at)
		VALUES ('claude', 'root-a', 'source-a', ?, ?, 1,
			'capture-invalid-descendant', ?)`,
		validCheckpointDigest(1), validCheckpointDigest(2), timestamp)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO outbox_generations
		(capture_id, provider, configured_root_id, source_key, captured_at,
		 kind, state, manifest_id, metadata_bytes, created_at, updated_at)
		VALUES ('capture-invalid', 'claude', 'root-a', 'source-a', ?,
		 'snapshot', 'invalid', '', 100, ?, ?),
		('capture-acknowledged', 'claude', 'root-a', 'source-a', ?,
		 'snapshot', 'acknowledged', ?, 200, ?, ?),
		('capture-queued', 'claude', 'root-a', 'source-a', ?,
		 'snapshot', 'queued', '', 300, ?, ?)`,
		timestamp, timestamp, timestamp,
		timestamp, validCheckpointDigest(1), timestamp, timestamp,
		timestamp, timestamp, timestamp)
	require.NoError(t, err)
	invalidOnly := validCheckpointDigest(3)
	acknowledgedOnly := validCheckpointDigest(4)
	shared := validCheckpointDigest(5)
	invalidDescendant := validCheckpointDigest(6)
	_, err = db.Exec(`INSERT INTO outbox_objects
		(sha256, length, spool_name, ref_count, state, created_at)
		VALUES (?, 5, ?, 1, 'live', ?),
		       (?, 7, ?, 1, 'live', ?),
		       (?, 11, ?, 2, 'live', ?),
		       (?, 13, ?, 1, 'live', ?)`,
		invalidOnly, invalidOnly, timestamp,
		acknowledgedOnly, acknowledgedOnly, timestamp,
		shared, shared, timestamp,
		invalidDescendant, invalidDescendant, timestamp)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO outbox_generations
		(capture_id, provider, configured_root_id, source_key,
		 predecessor_capture_id, captured_at, kind, state, metadata_bytes,
		 created_at, updated_at)
		VALUES ('capture-invalid-descendant', 'claude', 'root-a', 'source-a',
		 'capture-invalid', ?, 'snapshot', 'queued', 400, ?, ?)`,
		timestamp, timestamp, timestamp)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO outbox_entries
		(capture_id, entry_ordinal, path, length, mod_time_ns,
		 file_identity, prefix_sha256, appendable)
		VALUES ('capture-invalid', 0, 'invalid.jsonl', 16, 0, 'invalid', ?, 1),
		       ('capture-acknowledged', 0, 'acknowledged.jsonl', 7, 0,
		        'acknowledged', ?, 1),
		       ('capture-queued', 0, 'queued.jsonl', 11, 0, 'queued', ?, 1),
		       ('capture-invalid-descendant', 0, 'descendant.jsonl', 13, 0,
		        'descendant', ?, 1)`,
		invalidOnly, acknowledgedOnly, shared, invalidDescendant)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO outbox_entry_objects
		(capture_id, entry_ordinal, object_ordinal, sha256, length)
		VALUES ('capture-invalid', 0, 0, ?, 5),
		       ('capture-invalid', 0, 1, ?, 11),
		       ('capture-acknowledged', 0, 0, ?, 7),
		       ('capture-queued', 0, 0, ?, 11),
		       ('capture-invalid-descendant', 0, 0, ?, 13)`,
		invalidOnly, shared, acknowledgedOnly, shared, invalidDescendant)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO raw_source_base_entries
		(provider, configured_root_id, source_key, entry_ordinal, path, length,
		 mod_time_ns, file_identity, prefix_sha256, appendable)
		VALUES ('claude', 'root-a', 'source-a', 0, 'acknowledged.jsonl', 7,
		        0, 'acknowledged', ?, 1)`, acknowledgedOnly)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO raw_source_base_objects
		(provider, configured_root_id, source_key, entry_ordinal,
		 object_ordinal, sha256, length)
		VALUES ('claude', 'root-a', 'source-a', 0, 0, ?, 7)`, acknowledgedOnly)
	require.NoError(t, err)
	_, err = db.Exec(`PRAGMA user_version = 3`)
	require.NoError(t, err)
	require.NoError(t, db.Close())
	for _, object := range []struct {
		digest string
		data   string
	}{
		{digest: invalidOnly, data: "12345"},
		{digest: acknowledgedOnly, data: "1234567"},
		{digest: shared, data: "12345678901"},
		{digest: invalidDescendant, data: "1234567890123"},
	} {
		objectDir := filepath.Join(spoolDir, "objects", "sha256", object.digest[:2])
		require.NoError(t, os.MkdirAll(objectDir, 0o700))
		require.NoError(t, os.WriteFile(
			filepath.Join(objectDir, object.digest), []byte(object.data), 0o600,
		))
	}

	store, err := OpenWithOptions(t.Context(), path, Options{
		SpoolDir: spoolDir, MaxOutboxBytes: 1 << 20,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	base, ok, err := store.CaptureBase(t.Context(), SourceIdentity{
		Provider: parser.AgentClaude, ConfiguredRootID: "root-a", SourceKey: "source-a",
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "capture-acknowledged", base.CaptureID)
	require.Len(t, base.Entries, 1)
	assert.Equal(t, "acknowledged.jsonl", base.Entries[0].Path)

	usage, err := store.OutboxUsage(t.Context())
	require.NoError(t, err)
	assert.Equal(t, int64(311), usage.UsedBytes)
	var generations int
	require.NoError(t, store.db.QueryRow(
		`SELECT count(*) FROM outbox_generations`,
	).Scan(&generations))
	assert.Equal(t, 1, generations)

	var sharedReferences int
	var sharedState string
	require.NoError(t, store.db.QueryRow(
		`SELECT ref_count, state FROM outbox_objects WHERE sha256 = ? AND length = 11`,
		shared,
	).Scan(&sharedReferences, &sharedState))
	assert.Equal(t, 1, sharedReferences)
	assert.Equal(t, "live", sharedState)
	_, err = os.Stat(store.ObjectPath(rawsync.ObjectRef{SHA256: shared, Length: 11}))
	assert.NoError(t, err)

	var invalidObjects int
	require.NoError(t, store.db.QueryRow(
		`SELECT count(*) FROM outbox_objects WHERE sha256 = ? AND length = 5`,
		invalidOnly,
	).Scan(&invalidObjects))
	assert.Zero(t, invalidObjects)
	_, err = os.Stat(store.ObjectPath(rawsync.ObjectRef{SHA256: invalidOnly, Length: 5}))
	assert.ErrorIs(t, err, os.ErrNotExist)
	var descendantObjects int
	require.NoError(t, store.db.QueryRow(
		`SELECT count(*) FROM outbox_objects WHERE sha256 = ? AND length = 13`,
		invalidDescendant,
	).Scan(&descendantObjects))
	assert.Zero(t, descendantObjects)
	_, err = os.Stat(store.ObjectPath(rawsync.ObjectRef{
		SHA256: invalidDescendant, Length: 13,
	}))
	assert.ErrorIs(t, err, os.ErrNotExist)

	var acknowledgedReferences int
	var acknowledgedState string
	require.NoError(t, store.db.QueryRow(
		`SELECT ref_count, state FROM outbox_objects WHERE sha256 = ? AND length = 7`,
		acknowledgedOnly,
	).Scan(&acknowledgedReferences, &acknowledgedState))
	assert.Zero(t, acknowledgedReferences)
	assert.Equal(t, "remote", acknowledgedState)
	_, err = os.Stat(store.ObjectPath(rawsync.ObjectRef{SHA256: acknowledgedOnly, Length: 7}))
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestOpenWithOptionsReconcilesVersionFourRootCoverageGap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "checkpoint.db")
	db, err := sql.Open(checkpointDriverName, checkpointDSN(path, false))
	require.NoError(t, err)
	for _, statements := range [][]string{
		versionOneSchemaStatements,
		versionTwoMigrationStatements,
		versionThreeMigrationStatements,
		versionFourMigrationStatements,
	} {
		for _, statement := range statements {
			_, err = db.Exec(statement)
			require.NoError(t, err)
		}
	}
	const timestamp = "2026-08-25T00:00:00Z"
	_, err = db.Exec(`INSERT INTO configured_roots
		(id, provider, local_root, created_at, updated_at)
		VALUES ('root-a', 'claude', '/capture', ?, ?)`, timestamp, timestamp)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO raw_coverage
		(provider, configured_root_id, state, reason, degraded_at, updated_at)
		VALUES ('claude', 'root-a', 'degraded', 'outbox_full', ?, ?)`,
		timestamp, timestamp)
	require.NoError(t, err)
	_, err = db.Exec(`PRAGMA user_version = 4`)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	store, err := OpenWithOptions(t.Context(), path, Options{
		SpoolDir: filepath.Join(t.TempDir(), "spool"), MaxOutboxBytes: 1 << 20,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })

	var failures int
	require.NoError(t, store.db.QueryRow(
		`SELECT count(*) FROM raw_coverage_failures WHERE source_key = ''`,
	).Scan(&failures))
	assert.Equal(t, 1, failures)
	coverage, ok, err := store.Coverage(
		t.Context(), parser.AgentClaude, "root-a",
	)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, CoverageDegraded, coverage.State)

	require.NoError(t, store.CompleteRootReconciliation(t.Context(), "root-a"))
	coverage, ok, err = store.Coverage(t.Context(), parser.AgentClaude, "root-a")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, CoverageComplete, coverage.State)

	source := SourceIdentity{
		Provider: parser.AgentClaude, ConfiguredRootID: "root-a", SourceKey: "source-a",
	}
	_, err = store.ReserveSourceCapture(t.Context(), source, 1<<20+1)
	require.ErrorIs(t, err, ErrOutboxFull)
	require.NoError(t, store.CompleteRootReconciliation(t.Context(), "root-a"))
	coverage, ok, err = store.Coverage(t.Context(), parser.AgentClaude, "root-a")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, CoverageDegraded, coverage.State)
	assert.Equal(t, "outbox_full", coverage.Reason)
}

func TestOpenWithOptionsHoldsCheckpointAndSpoolProcessLocksUntilClose(t *testing.T) {
	base := t.TempDir()
	checkpointPath := filepath.Join(base, "checkpoint.db")
	spoolDir := filepath.Join(base, "spool")
	first, err := OpenWithOptions(t.Context(), checkpointPath, Options{
		SpoolDir: spoolDir, MaxOutboxBytes: 1 << 20,
	})
	require.NoError(t, err)

	_, err = OpenWithOptions(t.Context(), checkpointPath, Options{
		SpoolDir: filepath.Join(base, "other-spool"), MaxOutboxBytes: 1 << 20,
	})
	require.ErrorIs(t, err, ErrStoreLocked)
	_, err = OpenWithOptions(t.Context(), filepath.Join(base, "other.db"), Options{
		SpoolDir: spoolDir, MaxOutboxBytes: 1 << 20,
	})
	require.ErrorIs(t, err, ErrStoreLocked)
	require.NoError(t, first.Close())

	reopened, err := OpenWithOptions(t.Context(), checkpointPath, Options{
		SpoolDir: spoolDir, MaxOutboxBytes: 1 << 20,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reopened.Close()) })
}

func TestOpenWithOptionsRejectsChangedSpoolBeforeRecovery(t *testing.T) {
	base := t.TempDir()
	checkpointPath := filepath.Join(base, "checkpoint.db")
	spoolDir := filepath.Join(base, "spool")
	store, err := OpenWithOptions(t.Context(), checkpointPath, Options{
		SpoolDir: spoolDir, MaxOutboxBytes: 1 << 20,
	})
	require.NoError(t, err)
	sourceRoot := filepath.Join(base, "sources")
	require.NoError(t, os.MkdirAll(sourceRoot, 0o755))
	root, err := store.ResolveConfiguredRoot(t.Context(), parser.AgentClaude, sourceRoot)
	require.NoError(t, err)
	ref := rawsync.ObjectRef{SHA256: abcObjectSHA256, Length: 3}
	installOutboxTestObject(t, store, ref, []byte("abc"))
	reservation, err := store.ReserveCapture(t.Context(), root.ID, 1795)
	require.NoError(t, err)
	generation := testCapturedGeneration(1, root, "", ref)
	require.NoError(t, store.CommitCapture(t.Context(), reservation.ID, generation))
	require.NoError(t, store.Close())

	mismatched, err := OpenWithOptions(t.Context(), checkpointPath, Options{
		SpoolDir: filepath.Join(base, "other-spool"), MaxOutboxBytes: 1 << 20,
	})
	if mismatched != nil {
		require.NoError(t, mismatched.Close())
	}
	require.ErrorIs(t, err, ErrSpoolMismatch)

	reopened, err := OpenWithOptions(t.Context(), checkpointPath, Options{
		SpoolDir: spoolDir, MaxOutboxBytes: 1 << 20,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reopened.Close()) })
	queued, ok, err := reopened.NextGeneration(t.Context())
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, generation.CaptureID, queued.CaptureID)
	assert.FileExists(t, reopened.ObjectPath(ref))
}

func TestOpenWithOptionsRejectsCheckpointAndSpoolContentionAcrossProcesses(t *testing.T) {
	if os.Getenv("AGENTSVIEW_RAWCHECKPOINT_LOCK_HELPER") == "1" {
		_, err := OpenWithOptions(t.Context(), os.Getenv("AGENTSVIEW_RAWCHECKPOINT_PATH"), Options{
			SpoolDir: os.Getenv("AGENTSVIEW_RAWCHECKPOINT_SPOOL"), MaxOutboxBytes: 1 << 20,
		})
		if !errors.Is(err, ErrStoreLocked) {
			_, _ = fmt.Fprintf(os.Stderr, "expected store lock, got %v", err)
			os.Exit(2)
		}
		os.Exit(0)
	}
	base := t.TempDir()
	checkpointPath := filepath.Join(base, "checkpoint.db")
	spoolDir := filepath.Join(base, "spool")
	store, err := OpenWithOptions(t.Context(), checkpointPath, Options{
		SpoolDir: spoolDir, MaxOutboxBytes: 1 << 20,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })

	for _, tc := range []struct {
		name       string
		checkpoint string
		spool      string
	}{
		{name: "checkpoint", checkpoint: checkpointPath, spool: filepath.Join(base, "other-spool")},
		{name: "spool", checkpoint: filepath.Join(base, "other.db"), spool: spoolDir},
	} {
		t.Run(tc.name, func(t *testing.T) {
			command := exec.Command(os.Args[0],
				"-test.run=^TestOpenWithOptionsRejectsCheckpointAndSpoolContentionAcrossProcesses$",
			)
			command.Env = append(os.Environ(),
				"AGENTSVIEW_RAWCHECKPOINT_LOCK_HELPER=1",
				"AGENTSVIEW_RAWCHECKPOINT_PATH="+tc.checkpoint,
				"AGENTSVIEW_RAWCHECKPOINT_SPOOL="+tc.spool,
			)
			output, err := command.CombinedOutput()
			require.NoError(t, err, string(output))
		})
	}
}

func TestResolveConfiguredRootPersistsCanonicalProviderIdentity(t *testing.T) {
	base := t.TempDir()
	realRoot := filepath.Join(base, "real")
	linkedRoot := filepath.Join(base, "linked")
	otherRoot := filepath.Join(base, "other")
	require.NoError(t, os.MkdirAll(realRoot, 0o755))
	require.NoError(t, os.MkdirAll(otherRoot, 0o755))
	if err := os.Symlink(realRoot, linkedRoot); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	canonicalRealRoot, err := filepath.EvalSymlinks(realRoot)
	require.NoError(t, err)
	dbPath := filepath.Join(base, "checkpoint.db")
	spoolDir := filepath.Join(base, "spool")
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	store, err := OpenWithOptions(t.Context(), dbPath, Options{
		SpoolDir:       spoolDir,
		MaxOutboxBytes: 1 << 20,
		Now:            func() time.Time { return now },
	})
	require.NoError(t, err)

	first, err := store.ResolveConfiguredRoot(t.Context(), parser.AgentClaude, linkedRoot)
	require.NoError(t, err)
	viaRealPath, err := store.ResolveConfiguredRoot(t.Context(), parser.AgentClaude, realRoot)
	require.NoError(t, err)
	other, err := store.ResolveConfiguredRoot(t.Context(), parser.AgentClaude, otherRoot)
	require.NoError(t, err)
	otherProvider, err := store.ResolveConfiguredRoot(t.Context(), parser.AgentCodex, realRoot)
	require.NoError(t, err)

	assert.Equal(t, first.ID, viaRealPath.ID)
	assert.Equal(t, canonicalRealRoot, first.LocalPath)
	assert.Equal(t, parser.AgentClaude, first.Provider)
	assert.Equal(t, now, first.CreatedAt)
	assert.Equal(t, now, first.UpdatedAt)
	assert.NotEqual(t, first.ID, other.ID)
	assert.NotEqual(t, first.ID, otherProvider.ID)
	assert.Regexp(t, `^[0-9a-f]{32}$`, first.ID)
	require.NoError(t, store.Close())

	reopened, err := OpenWithOptions(t.Context(), dbPath, Options{
		SpoolDir:       spoolDir,
		MaxOutboxBytes: 1 << 20,
		Now:            func() time.Time { return now.Add(time.Hour) },
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reopened.Close()) })
	persisted, err := reopened.ResolveConfiguredRoot(t.Context(), parser.AgentClaude, realRoot)
	require.NoError(t, err)
	assert.Equal(t, first, persisted)
}

func TestResolveConfiguredRootDoesNotExposeLocalPathInFilesystemError(t *testing.T) {
	store, _ := openOutboxTestStore(t, 1<<20)
	privateRoot := filepath.Join(t.TempDir(), "private", "missing")

	_, err := store.ResolveConfiguredRoot(t.Context(), parser.AgentClaude, privateRoot)

	require.Error(t, err)
	assert.NotContains(t, err.Error(), filepath.Dir(filepath.Dir(privateRoot)))
}

const (
	emptyObjectSHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	abcObjectSHA256   = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
)

func openOutboxTestStore(t *testing.T, maxBytes int64) (*Store, ConfiguredRoot) {
	t.Helper()
	base := t.TempDir()
	rootPath := filepath.Join(base, "sources")
	require.NoError(t, os.MkdirAll(rootPath, 0o755))
	store, err := OpenWithOptions(t.Context(), filepath.Join(base, "checkpoint.db"), Options{
		SpoolDir:       filepath.Join(base, "spool"),
		MaxOutboxBytes: maxBytes,
		Now: func() time.Time {
			return time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	root, err := store.ResolveConfiguredRoot(t.Context(), parser.AgentClaude, rootPath)
	require.NoError(t, err)
	return store, root
}

func installOutboxTestObject(
	t *testing.T,
	store *Store,
	ref rawsync.ObjectRef,
	content []byte,
) {
	t.Helper()
	path := store.ObjectPath(ref)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, content, 0o600))
}

func TestInsertCapturedObjectsPromotesRemoteRowWhenLocalBytesArePresent(t *testing.T) {
	store, _ := openOutboxTestStore(t, 1<<20)
	ref := rawsync.ObjectRef{SHA256: abcObjectSHA256, Length: 3}
	_, err := store.db.Exec(`INSERT INTO outbox_objects
		(sha256, length, spool_name, ref_count, state, created_at)
		VALUES (?, ?, 'objects/remote', 0, 'remote', '2026-08-25T00:00:00Z')`,
		ref.SHA256, ref.Length)
	require.NoError(t, err)
	installOutboxTestObject(t, store, ref, []byte("abc"))

	err = store.withImmediateWrite(t.Context(), "test promote remote object", func(conn *sql.Conn) error {
		return insertCapturedObjectsConn(t.Context(), conn, store,
			map[string]rawsync.ObjectRef{ref.SHA256: ref}, "2026-08-25T00:00:01Z")
	})

	require.NoError(t, err)
	var state string
	require.NoError(t, store.db.QueryRow(`SELECT state FROM outbox_objects
		WHERE sha256 = ? AND length = ?`, ref.SHA256, ref.Length).Scan(&state))
	assert.Equal(t, "live", state)
	usage, err := store.OutboxUsage(t.Context())
	require.NoError(t, err)
	assert.Equal(t, ref.Length, usage.UsedBytes)
}

func TestCollectGarbageWaitsForObjectPublication(t *testing.T) {
	store, _ := openOutboxTestStore(t, 1<<20)
	ref := rawsync.ObjectRef{SHA256: abcObjectSHA256, Length: 3}
	_, err := store.db.Exec(`INSERT INTO outbox_objects
		(sha256, length, spool_name, ref_count, state, created_at)
		VALUES (?, ?, 'objects/pending', 0, 'garbage_pending', '2026-08-25T00:00:00Z')`,
		ref.SHA256, ref.Length)
	require.NoError(t, err)
	finishPublication := store.BeginObjectPublication()
	installOutboxTestObject(t, store, ref, []byte("abc"))
	type collectionOutcome struct {
		report GarbageCollectionReport
		err    error
	}
	started := make(chan struct{})
	completed := make(chan collectionOutcome, 1)
	go func() {
		close(started)
		report, err := store.CollectGarbage(t.Context())
		completed <- collectionOutcome{report: report, err: err}
	}()
	<-started

	select {
	case outcome := <-completed:
		finishPublication()
		require.FailNow(t, "garbage collection completed during publication",
			"report=%+v error=%v", outcome.report, outcome.err)
	case <-time.After(100 * time.Millisecond):
		assert.FileExists(t, store.ObjectPath(ref))
	}

	finishPublication()
	outcome := <-completed
	require.NoError(t, outcome.err)
	assert.Equal(t, GarbageCollectionReport{Objects: 1, Bytes: 3}, outcome.report)
	assert.NoFileExists(t, store.ObjectPath(ref))
}

func testCapturedGeneration(
	sequence int,
	root ConfiguredRoot,
	predecessor string,
	ref rawsync.ObjectRef,
) CapturedGeneration {
	captureID := fmt.Sprintf("%032x", sequence)
	return CapturedGeneration{
		CaptureID: captureID,
		Source: SourceIdentity{
			Provider:         root.Provider,
			ConfiguredRootID: root.ID,
			SourceKey:        "source-1",
		},
		PredecessorCaptureID: predecessor,
		CapturedAt:           time.Date(2026, 8, 25, 12, 0, sequence, 0, time.UTC),
		Kind:                 rawsync.ManifestSnapshot,
		Entries: []CapturedEntry{{
			Path:         "project/session.jsonl",
			Length:       ref.Length,
			ModTimeNS:    int64(sequence),
			FileIdentity: "device:1:inode:2",
			PrefixSHA256: ref.SHA256,
			Appendable:   true,
			Objects:      []rawsync.ObjectRef{ref},
		}},
	}
}

func TestReserveCaptureIsAtomicAndMarksOnlyItsRootDegraded(t *testing.T) {
	store, root := openOutboxTestStore(t, 2048)

	start := make(chan struct{})
	results := make(chan error, 2)
	var reservationsMu sync.Mutex
	var reservations []Reservation
	for range 2 {
		go func() {
			<-start
			reservation, err := store.ReserveCapture(t.Context(), root.ID, 1536)
			if err == nil {
				reservationsMu.Lock()
				reservations = append(reservations, reservation)
				reservationsMu.Unlock()
			}
			results <- err
		}()
	}
	close(start)

	var successes, full int
	for range 2 {
		err := <-results
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrOutboxFull):
			full++
		default:
			require.NoError(t, err)
		}
	}
	assert.Equal(t, 1, successes)
	assert.Equal(t, 1, full)
	usage, err := store.OutboxUsage(t.Context())
	require.NoError(t, err)
	assert.Equal(t, int64(0), usage.UsedBytes)
	assert.Equal(t, int64(1536), usage.ReservedBytes)
	assert.Equal(t, int64(2048), usage.LimitBytes)
	coverage, ok, err := store.Coverage(t.Context(), root.Provider, root.ID)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, CoverageDegraded, coverage.State)
	assert.Equal(t, "outbox_full", coverage.Reason)

	require.Len(t, reservations, 1)
	require.NoError(t, store.ReleaseReservation(t.Context(), reservations[0].ID))
	usage, err = store.OutboxUsage(t.Context())
	require.NoError(t, err)
	assert.Zero(t, usage.ReservedBytes)
}

func TestReserveCaptureRejectsMetadataOnlyOverflowWithoutObjects(t *testing.T) {
	store, root := openOutboxTestStore(t, 1791)

	_, err := store.ReserveCapture(t.Context(), root.ID, 1792)

	require.ErrorIs(t, err, ErrOutboxFull)
	usage, readErr := store.OutboxUsage(t.Context())
	require.NoError(t, readErr)
	assert.Zero(t, usage.UsedBytes)
	assert.Zero(t, usage.ReservedBytes)
	var generations, objects int
	require.NoError(t, store.db.QueryRow(`SELECT count(*) FROM outbox_generations`).Scan(&generations))
	require.NoError(t, store.db.QueryRow(`SELECT count(*) FROM outbox_objects`).Scan(&objects))
	assert.Zero(t, generations)
	assert.Zero(t, objects)
}

func TestSuccessfulCaptureClearsItsOwnSourceCoverageFailure(t *testing.T) {
	store, root := openOutboxTestStore(t, 2000)
	source := SourceIdentity{
		Provider: root.Provider, ConfiguredRootID: root.ID, SourceKey: "source-1",
	}
	_, err := store.ReserveSourceCapture(t.Context(), source, 2001)
	require.ErrorIs(t, err, ErrOutboxFull)
	coverage, ok, err := store.Coverage(t.Context(), root.Provider, root.ID)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, CoverageDegraded, coverage.State)
	ref := rawsync.ObjectRef{SHA256: validCheckpointDigest(10), Length: 1}
	installOutboxTestObject(t, store, ref, []byte{1})
	reservation, err := store.ReserveSourceCapture(t.Context(), source, 1793)
	require.NoError(t, err)
	generation := testCapturedGeneration(1, root, "", ref)
	require.NoError(t, store.CommitCapture(t.Context(), reservation.ID, generation))

	coverage, ok, err = store.Coverage(t.Context(), root.Provider, root.ID)

	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, CoverageComplete, coverage.State)
	assert.NotNil(t, coverage.RecoveredAt)
}

func TestCompleteUnchangedCaptureRejectsReservationForAnotherSource(t *testing.T) {
	store, root := openOutboxTestStore(t, 2000)
	sourceA := SourceIdentity{
		Provider: root.Provider, ConfiguredRootID: root.ID, SourceKey: "source-a",
	}
	sourceB := SourceIdentity{
		Provider: root.Provider, ConfiguredRootID: root.ID, SourceKey: "source-b",
	}
	_, err := store.ReserveSourceCapture(t.Context(), sourceB, 2001)
	require.ErrorIs(t, err, ErrOutboxFull)
	reservation, err := store.ReserveSourceCapture(t.Context(), sourceA, 128)
	require.NoError(t, err)

	err = store.CompleteUnchangedCapture(t.Context(), reservation.ID, sourceB)

	assert.ErrorIs(t, err, ErrCaptureConflict)
	usage, readErr := store.OutboxUsage(t.Context())
	require.NoError(t, readErr)
	assert.Equal(t, int64(128), usage.ReservedBytes)
	coverage, ok, readErr := store.Coverage(t.Context(), root.Provider, root.ID)
	require.NoError(t, readErr)
	require.True(t, ok)
	assert.Equal(t, CoverageDegraded, coverage.State)
	assert.Equal(t, "outbox_full", coverage.Reason)
}

func TestCompleteUnchangedCaptureAcceptsRootScopedReservation(t *testing.T) {
	store, root := openOutboxTestStore(t, 2000)
	reservation, err := store.ReserveCapture(t.Context(), root.ID, 128)
	require.NoError(t, err)
	source := SourceIdentity{
		Provider: root.Provider, ConfiguredRootID: root.ID, SourceKey: "source-a",
	}

	err = store.CompleteUnchangedCapture(t.Context(), reservation.ID, source)

	require.NoError(t, err)
	usage, err := store.OutboxUsage(t.Context())
	require.NoError(t, err)
	assert.Zero(t, usage.ReservedBytes)
}

func TestCommitCaptureQueuesOfflineGenerationsAndChargesDuplicateObjectOnce(t *testing.T) {
	store, root := openOutboxTestStore(t, 1<<20)
	ref := rawsync.ObjectRef{SHA256: abcObjectSHA256, Length: 3}
	installOutboxTestObject(t, store, ref, []byte("abc"))

	firstReservation, err := store.ReserveCapture(t.Context(), root.ID, 1795)
	require.NoError(t, err)
	first := testCapturedGeneration(1, root, "", ref)
	require.NoError(t, store.CommitCapture(t.Context(), firstReservation.ID, first))

	secondReservation, err := store.ReserveCapture(t.Context(), root.ID, 1792)
	require.NoError(t, err)
	second := testCapturedGeneration(2, root, first.CaptureID, ref)
	require.NoError(t, store.CommitCapture(t.Context(), secondReservation.ID, second))

	usage, err := store.OutboxUsage(t.Context())
	require.NoError(t, err)
	assert.Equal(t, int64(3587), usage.UsedBytes)
	assert.Zero(t, usage.ReservedBytes)
	var objectRows, refCount int
	require.NoError(t, store.db.QueryRow(`SELECT count(*), sum(ref_count)
		FROM outbox_objects`).Scan(&objectRows, &refCount))
	assert.Equal(t, 1, objectRows)
	assert.Equal(t, 2, refCount)

	base, ok, err := store.CaptureBase(t.Context(), second.Source)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, second.CaptureID, base.CaptureID)
	require.Len(t, base.Entries, 1)
	assert.Equal(t, second.Entries[0], base.Entries[0])

	next, ok, err := store.NextGeneration(t.Context())
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, first.CaptureID, next.CaptureID)
	assert.Empty(t, next.PredecessorCaptureID)
	assert.Equal(t, first.Entries, next.Entries)
}

func TestCommitCaptureRejectsProviderThatDoesNotOwnReservedRoot(t *testing.T) {
	store, root := openOutboxTestStore(t, 1<<20)
	ref := rawsync.ObjectRef{SHA256: abcObjectSHA256, Length: 3}
	installOutboxTestObject(t, store, ref, []byte("abc"))
	reservation, err := store.ReserveCapture(t.Context(), root.ID, 1795)
	require.NoError(t, err)
	generation := testCapturedGeneration(1, root, "", ref)
	generation.Source.Provider = parser.AgentCodex

	err = store.CommitCapture(t.Context(), reservation.ID, generation)

	require.ErrorIs(t, err, ErrCaptureConflict)
	_, ok, readErr := store.NextGeneration(t.Context())
	require.NoError(t, readErr)
	assert.False(t, ok)
}

func TestCommitCaptureRejectsReservationForAnotherSource(t *testing.T) {
	store, root := openOutboxTestStore(t, 1<<20)
	ref := rawsync.ObjectRef{SHA256: abcObjectSHA256, Length: 3}
	installOutboxTestObject(t, store, ref, []byte("abc"))
	reservationSource := SourceIdentity{
		Provider: root.Provider, ConfiguredRootID: root.ID, SourceKey: "source-a",
	}
	reservation, err := store.ReserveSourceCapture(t.Context(), reservationSource, 1795)
	require.NoError(t, err)
	generation := testCapturedGeneration(1, root, "", ref)
	generation.Source.SourceKey = "source-b"

	err = store.CommitCapture(t.Context(), reservation.ID, generation)

	assert.ErrorIs(t, err, ErrCaptureConflict)
	usage, readErr := store.OutboxUsage(t.Context())
	require.NoError(t, readErr)
	assert.Equal(t, int64(1795), usage.ReservedBytes)
	_, ok, readErr := store.NextGeneration(t.Context())
	require.NoError(t, readErr)
	assert.False(t, ok)
}

func TestZeroByteGenerationsConsumeMetadataCapacity(t *testing.T) {
	store, root := openOutboxTestStore(t, 3584)
	ref := rawsync.ObjectRef{SHA256: emptyObjectSHA256, Length: 0}
	installOutboxTestObject(t, store, ref, nil)

	predecessor := ""
	for sequence := 1; sequence <= 2; sequence++ {
		reservation, err := store.ReserveCapture(t.Context(), root.ID, 1792)
		require.NoError(t, err)
		generation := testCapturedGeneration(sequence, root, predecessor, ref)
		require.NoError(t, store.CommitCapture(t.Context(), reservation.ID, generation))
		predecessor = generation.CaptureID
	}

	_, err := store.ReserveCapture(t.Context(), root.ID, 1792)
	require.ErrorIs(t, err, ErrOutboxFull)
	usage, err := store.OutboxUsage(t.Context())
	require.NoError(t, err)
	assert.Equal(t, int64(3584), usage.UsedBytes)
	var generations int
	require.NoError(t, store.db.QueryRow(`SELECT count(*) FROM outbox_generations`).Scan(&generations))
	assert.Equal(t, 2, generations)
}

func TestSetDeviceReclaimsQueuedObjectsAndPreservesConfiguredRoots(t *testing.T) {
	store, root := openOutboxTestStore(t, 1<<20)
	require.NoError(t, store.SetDevice(t.Context(), "dev_1"))
	ref := rawsync.ObjectRef{SHA256: abcObjectSHA256, Length: 3}
	installOutboxTestObject(t, store, ref, []byte("abc"))
	reservation, err := store.ReserveCapture(t.Context(), root.ID, 1795)
	require.NoError(t, err)
	generation := testCapturedGeneration(1, root, "", ref)
	require.NoError(t, store.CommitCapture(t.Context(), reservation.ID, generation))

	require.NoError(t, store.SetDevice(t.Context(), "dev_2"))

	usage, err := store.OutboxUsage(t.Context())
	require.NoError(t, err)
	assert.Zero(t, usage.UsedBytes)
	assert.NoFileExists(t, store.ObjectPath(ref))
	var generations, objects, roots int
	require.NoError(t, store.db.QueryRow(`SELECT count(*) FROM outbox_generations`).Scan(&generations))
	require.NoError(t, store.db.QueryRow(`SELECT count(*) FROM outbox_objects`).Scan(&objects))
	require.NoError(t, store.db.QueryRow(`SELECT count(*) FROM configured_roots`).Scan(&roots))
	assert.Zero(t, generations)
	assert.Zero(t, objects)
	assert.Equal(t, 1, roots)
	persisted, err := store.ResolveConfiguredRoot(t.Context(), root.Provider, root.LocalPath)
	require.NoError(t, err)
	assert.Equal(t, root.ID, persisted.ID)
}
