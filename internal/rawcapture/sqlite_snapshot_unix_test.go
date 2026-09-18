//go:build unix

package rawcapture

import (
	"context"
	"database/sql"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/parser"
)

func TestSQLiteSnapshotConnectionIdentityIsIndependentOfPath(t *testing.T) {
	require := require.New(t)

	dir := t.TempDir()
	path := filepath.Join(dir, "source.db")
	writeSQLiteCaptureTestDB(t, path, "connected")
	expected, err := os.Stat(path)
	require.NoError(err)
	source, err := openSQLiteSnapshotSource(t.Context(), path, expected)
	require.NoError(err)
	defer source.Close()
	replacement := filepath.Join(dir, "replacement.db")
	writeSQLiteCaptureTestDB(t, replacement, "replacement")
	replacementFile, err := os.Open(replacement)
	require.NoError(err)
	replacementInfo, err := replacementFile.Stat()
	require.NoError(err)
	replacementIdentity := stableFileIdentity(replacementFile, replacementInfo)
	require.NoError(replacementFile.Close())
	require.NotEqual(source.expectedIdentity, replacementIdentity)

	source.expectedIdentity = replacementIdentity

	require.ErrorIs(source.verifyCurrent(), ErrSourceChanged)
}

func TestOpenSQLiteSnapshotSourceAcceptsSymlinkedAncestor(t *testing.T) {
	require := require.New(t)

	root := t.TempDir()
	realDir := filepath.Join(root, "real")
	require.NoError(os.Mkdir(realDir, 0o700))
	linkedDir := filepath.Join(root, "linked")
	if err := os.Symlink(realDir, linkedDir); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	path := filepath.Join(linkedDir, "source.db")
	writeSQLiteCaptureTestDB(t, path, "connected")
	expected, err := os.Stat(path)
	require.NoError(err)

	source, err := openSQLiteSnapshotSource(t.Context(), path, expected)
	require.NoError(err)
	require.NoError(source.Close())
}

func TestOpenSQLiteSnapshotSourcePreservesPermissionError(t *testing.T) {
	require := require.New(t)

	dir := t.TempDir()
	path := filepath.Join(dir, "source.db")
	writeSQLiteCaptureTestDB(t, path, "connected")
	expected, err := os.Stat(path)
	require.NoError(err)
	require.NoError(os.Chmod(path, 0))
	t.Cleanup(func() { require.NoError(os.Chmod(path, 0o600)) })

	source, err := openSQLiteSnapshotSource(t.Context(), path, expected)
	if err == nil {
		require.NoError(source.Close())
		t.Skip("filesystem permissions are not enforced for this process")
	}
	require.ErrorIs(err, fs.ErrPermission)
	require.NotErrorIs(err, ErrSourceChanged)
}

func TestCapturerCleansSQLiteSnapshotWhenSourceChangesAfterBackup(t *testing.T) {
	store, _ := openCapturerTestStore(t, 1<<20)
	root := t.TempDir()
	path := filepath.Join(root, "source.db")
	writeSQLiteCaptureTestDB(t, path, "original")
	provider := &captureTestProvider{
		Def: parser.AgentDef{Type: parser.AgentForge},
		Caps: parser.Capabilities{RawCapture: parser.RawCaptureCapabilities{
			Support: parser.CapabilitySupported, Shape: parser.RawCaptureShapeSQLite,
			Append:   parser.RawCaptureAppendReplaceOnly,
			Snapshot: parser.RawCaptureSnapshotOnlineBackup,
		}},
		plan: parser.RawCapturePlan{
			ConfiguredRoot: root, CaptureRoot: root, SourceKey: "source.db",
			Entries: []parser.RawCaptureEntry{{Path: "source.db", LocalPath: path}},
		},
	}
	capturer := New(store)
	capturer.sqliteBackup = func(
		ctx context.Context, connection *sql.Conn, destination string, maxBytes int64,
	) error {
		if err := sqliteOnlineBackup(ctx, connection, destination, maxBytes); err != nil {
			return err
		}
		relocated := path + ".original"
		require.NoError(t, os.Rename(path, relocated))
		writeSQLiteCaptureTestDB(t, path, "replacement")
		return nil
	}

	_, err := capturer.Capture(
		t.Context(), provider, parser.SourceRef{Provider: parser.AgentForge, Key: "source.db"},
	)

	require.ErrorIs(t, err, ErrSourceChanged)
	temporary, err := os.ReadDir(store.CaptureTempDir())
	require.NoError(t, err)
	require.Empty(t, temporary)
}
