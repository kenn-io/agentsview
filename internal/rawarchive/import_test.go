package rawarchive

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/testjsonl"
	"go.kenn.io/docbank"
)

func newImportCapture(t *testing.T, device, machine string, roots ...RootSpec) CaptureOptions {
	t.Helper()
	data := t.TempDir()
	database, err := db.OpenIsolatedContext(t.Context(), filepath.Join(data, "sessions.db"))
	require.NoError(t, err)
	require.NoError(t, database.Close())
	dbtest.WriteTestFile(t, filepath.Join(data, "telemetry-install-id"), []byte(device))
	return CaptureOptions{DataDir: data, Roots: roots, Settings: RecoverySettings{LocalMachineName: machine}, ReaderBuild: "test-build"}
}

func loadTestCapture(t *testing.T, opts *CaptureOptions) ImportSpec {
	t.Helper()
	opts.Destination = filepath.Join(t.TempDir(), "capture")
	_, err := Capture(t.Context(), *opts)
	require.NoError(t, err)
	opts.IdentityFrom = filepath.Join(opts.Destination, "capture.json")
	spec, err := LoadImportSpec(t.Context(), opts.IdentityFrom)
	require.NoError(t, err)
	return spec
}

func TestImportRequiresValidatedCapture(t *testing.T) {
	ctx := t.Context()
	database := dbtest.OpenTestDB(t)
	require.NoError(t, database.EnableArchiveOnly(ctx))
	archive, err := Open(ctx, database, t.TempDir(), nil)
	require.NoError(t, err)
	defer archive.Close()
	root := t.TempDir()
	dbtest.WriteTestFile(t, filepath.Join(root, "source.txt"), []byte("original"))
	_, err = archive.Import(ctx, ImportSpec{
		DeviceID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Machine: "source",
		Roots: []RootSpec{{ID: "files", Provider: "files", Path: root, OriginalPath: root}},
	})
	require.ErrorContains(t, err, "validated capture")
	files, err := database.ListRawArchiveFiles(ctx, 0, 100)
	require.NoError(t, err)
	assert.Empty(t, files)
}

func TestImportRejectsChangedValidatedFile(t *testing.T) {
	for _, retry := range []bool{false, true} {
		t.Run(map[bool]string{false: "new-object", true: "existing-object"}[retry], func(t *testing.T) {
			ctx := t.Context()
			root := t.TempDir()
			dbtest.WriteTestFile(t, filepath.Join(root, "source.txt"), []byte("original"))
			opts := newImportCapture(t, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "source", RootSpec{Provider: "files", Path: root})
			spec := loadTestCapture(t, &opts)
			database := dbtest.OpenTestDB(t)
			require.NoError(t, database.EnableArchiveOnly(ctx))
			archive, err := Open(ctx, database, t.TempDir(), nil)
			require.NoError(t, err)
			defer archive.Close()
			if retry {
				_, err = archive.Import(ctx, spec)
				require.NoError(t, err)
			}
			before, err := database.ListRawArchiveFiles(ctx, 0, 100)
			require.NoError(t, err)
			// Keep the size unchanged so only verification of the actual bytes can
			// reject the stale inventory, even when its object already exists.
			require.NoError(t, os.WriteFile(filepath.Join(spec.Roots[0].Path, "source.txt"), []byte("modified"), 0o600))
			_, err = archive.Import(ctx, spec)
			require.ErrorIs(t, err, docbank.ErrDigestMismatch)
			after, err := database.ListRawArchiveFiles(ctx, 0, 100)
			require.NoError(t, err)
			assert.Equal(t, before, after, "rejected bytes never enter the archive inventory")
			if retry {
				target := filepath.Join(t.TempDir(), "extracted")
				_, err = archive.Extract(ctx, target, spec.capture.CaptureID)
				require.NoError(t, err)
				content, err := os.ReadFile(filepath.Join(target, "roots", spec.Roots[0].ID, "source.txt"))
				require.NoError(t, err)
				assert.Equal(t, "original", string(content))
			}
		})
	}
}

func TestImportOfCopiedCaptureUsesInventoryTimes(t *testing.T) {
	ctx := t.Context()
	const id = "019eb791-cf7d-75c1-8439-9ed74c122e02"
	root := t.TempDir()
	dbtest.WriteTestFile(t, filepath.Join(root, "project", id+".jsonl"), []byte(testjsonl.NewSessionBuilder().
		AddClaudeUserWithSessionID("2026-01-01T00:00:00Z", "archived history", id).String()))
	opts := newImportCapture(t, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "source", RootSpec{Provider: "claude", Path: root})
	spec := loadTestCapture(t, &opts)
	database := dbtest.OpenTestDB(t)
	require.NoError(t, database.EnableArchiveOnly(ctx))
	archive, err := Open(ctx, database, t.TempDir(), nil)
	require.NoError(t, err)
	defer archive.Close()
	first, err := archive.Import(ctx, spec)
	require.NoError(t, err)
	require.Empty(t, first.Gaps)

	// A copy that does not preserve timestamps is still the same capture.
	copied := filepath.Join(t.TempDir(), "copied")
	copiedAt := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	require.NoError(t, filepath.WalkDir(opts.Destination, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(opts.Destination, path)
		if err != nil {
			return err
		}
		target := filepath.Join(copied, rel)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := os.WriteFile(target, data, 0o600); err != nil {
			return err
		}
		return os.Chtimes(target, copiedAt, copiedAt)
	}))
	retry, err := LoadImportSpec(ctx, filepath.Join(copied, "capture.json"))
	require.NoError(t, err)
	second, err := archive.Import(ctx, retry)
	require.NoError(t, err)
	assert.Empty(t, second.Gaps, "retrying an identical capture must not report a source conflict")
	assert.Equal(t, first.Sources, second.Sources)
	sources, err := database.ListRawArchiveSources(ctx, "", 10)
	require.NoError(t, err)
	assert.Len(t, sources, first.Sources)
}
