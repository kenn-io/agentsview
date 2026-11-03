package rawcapture

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
)

func sqliteArchiveFixture(t *testing.T) (*captureTestProvider, parser.SourceRef, *sql.DB) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	dbPath := filepath.Join(root, "state.db")
	writer, err := sql.Open(sqliteSnapshotDriverName, dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, writer.Close()) })
	_, err = writer.ExecContext(t.Context(), `PRAGMA journal_mode=WAL; CREATE TABLE items(value TEXT); INSERT INTO items VALUES('from WAL');`)
	require.NoError(t, err)
	companion := filepath.Join(root, "session.jsonl")
	require.NoError(t, os.WriteFile(companion, []byte("companion\n"), 0o600))
	provider := &captureTestProvider{Def: parser.AgentDef{Type: parser.AgentHermes}, Caps: parser.Capabilities{RawCapture: parser.RawCaptureCapabilities{Support: parser.CapabilitySupported, Shape: parser.RawCaptureShapeSQLiteArchive, Snapshot: parser.RawCaptureSnapshotOnlineBackup}}, plan: parser.RawCapturePlan{ConfiguredRoot: root, CaptureRoot: root, SourceKey: dbPath, SnapshotPath: "state.db", Entries: []parser.RawCaptureEntry{{Path: "session.jsonl", LocalPath: companion}, {Path: "state.db", LocalPath: dbPath}}}}
	return provider, parser.SourceRef{Provider: parser.AgentHermes, Key: dbPath}, writer
}

func TestCapturerSQLiteArchiveBacksUpOnlyDatabase(t *testing.T) {
	provider, source, _ := sqliteArchiveFixture(t)
	store, _ := openCapturerTestStore(t, 1<<20)
	capturer := New(store)
	first, err := capturer.Capture(t.Context(), provider, source)
	require.NoError(t, err)
	require.Equal(t, StatusCaptured, first.Status)
	base, ok, err := store.CaptureBase(t.Context(), first.Source)
	require.NoError(t, err)
	require.True(t, ok)
	require.Len(t, base.Entries, 2)
	companion, err := os.ReadFile(store.ObjectPath(base.Entries[0].Objects[0]))
	require.NoError(t, err)
	assert.Equal(t, "companion\n", string(companion))
	assert.NotContains(t, base.Entries[0].FileIdentity, "sqlite-online-backup:")
	assert.Equal(t, "sqlite-online-backup:state.db", base.Entries[1].FileIdentity)
	backup, err := sql.Open(sqliteSnapshotDriverName, sqliteSnapshotDSN(store.ObjectPath(base.Entries[1].Objects[0]), true))
	require.NoError(t, err)
	var value string
	require.NoError(t, backup.QueryRowContext(t.Context(), `SELECT value FROM items`).Scan(&value))
	assert.Equal(t, "from WAL", value)
	require.NoError(t, backup.Close())
	second, err := capturer.Capture(t.Context(), provider, source)
	require.NoError(t, err)
	assert.Equal(t, StatusUnchanged, second.Status)
	require.NoError(t, os.WriteFile(provider.plan.Entries[0].LocalPath, []byte("replacement\n"), 0o600))
	third, err := capturer.Capture(t.Context(), provider, source)
	require.NoError(t, err)
	assert.Equal(t, StatusCaptured, third.Status)
}

func TestCapturerSQLiteArchiveRejectsMembershipChange(t *testing.T) {
	for _, unchanged := range []bool{false, true} {
		t.Run(map[bool]string{false: "new", true: "unchanged"}[unchanged], func(t *testing.T) {
			provider, source, _ := sqliteArchiveFixture(t)
			store, _ := openCapturerTestStore(t, 1<<20)
			capturer := New(store)
			if unchanged {
				_, err := capturer.Capture(t.Context(), provider, source)
				require.NoError(t, err)
			}
			provider.planHook = func(call int32) {
				if call%2 == 0 {
					provider.plan.Entries = provider.plan.Entries[1:]
				}
			}
			_, err := capturer.Capture(t.Context(), provider, source)
			require.ErrorIs(t, err, ErrSourceChanged)
		})
	}
}

func TestCapturerSQLiteArchiveRejectsCompanionMutationDuringBackup(t *testing.T) {
	provider, source, _ := sqliteArchiveFixture(t)
	store, _ := openCapturerTestStore(t, 1<<20)
	capturer := New(store)
	backup := capturer.sqliteBackup
	capturer.sqliteBackup = func(ctx context.Context, conn *sql.Conn, destination string, maxBytes int64) error {
		require.NoError(t, os.WriteFile(provider.plan.Entries[0].LocalPath, []byte("changed during backup"), 0o600))
		return backup(ctx, conn, destination, maxBytes)
	}
	_, err := capturer.Capture(t.Context(), provider, source)
	require.ErrorIs(t, err, ErrSourceChanged)
}

func TestCapturerSQLiteArchiveReservesCompanionBeforeBackup(t *testing.T) {
	provider, source, _ := sqliteArchiveFixture(t)
	require.NoError(t, os.WriteFile(provider.plan.Entries[0].LocalPath, make([]byte, 1<<20), 0o600))
	store, _ := openCapturerTestStore(t, 1<<20)
	capturer := New(store)
	capturer.sqliteBackup = func(context.Context, *sql.Conn, string, int64) error {
		require.FailNow(t, "backup must not run without companion capacity")
		return nil
	}
	result, err := capturer.Capture(t.Context(), provider, source)
	require.NoError(t, err)
	assert.Equal(t, StatusDegraded, result.Status)
}

func TestCapturerFileOnlyArchiveRechecksMembershipAfterUnchangedRead(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	file := filepath.Join(root, "first.jsonl")
	require.NoError(t, os.WriteFile(file, []byte("original\n"), 0o600))
	provider := &captureTestProvider{Def: parser.AgentDef{Type: parser.AgentHermes}, Caps: parser.Capabilities{RawCapture: parser.RawCaptureCapabilities{Support: parser.CapabilitySupported, Shape: parser.RawCaptureShapeSQLiteArchive, Snapshot: parser.RawCaptureSnapshotOnlineBackup}}, plan: parser.RawCapturePlan{ConfiguredRoot: root, CaptureRoot: root, SourceKey: filepath.Join(root, "state.db"), Entries: []parser.RawCaptureEntry{{Path: "first.jsonl", LocalPath: file}}}}
	source := parser.SourceRef{Provider: parser.AgentHermes, Key: provider.plan.SourceKey}
	store, _ := openCapturerTestStore(t, 1<<20)
	capturer := New(store)
	_, err = capturer.Capture(t.Context(), provider, source)
	require.NoError(t, err)
	mutated := false
	capturer.capturePhase = func(phase capturePhase, path string) {
		if phase != capturePhaseAfterRead || path != file || len(provider.plan.Entries) != 1 {
			return
		}
		added := filepath.Join(root, "second.jsonl")
		require.NoError(t, os.WriteFile(added, []byte("new member\n"), 0o600))
		provider.plan.Entries = append(provider.plan.Entries, parser.RawCaptureEntry{Path: "second.jsonl", LocalPath: added})
		mutated = true
	}
	_, err = capturer.Capture(t.Context(), provider, source)
	require.True(t, mutated)
	require.ErrorIs(t, err, ErrSourceChanged)
}
