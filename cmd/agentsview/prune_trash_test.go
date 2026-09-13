package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/trash"
)

func TestParseAgeDuration(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    int
		wantErr bool
	}{
		{name: "days", input: "30d", want: 30},
		{name: "single day", input: "1d", want: 1},
		{name: "weeks", input: "2w", want: 14},
		{name: "years", input: "1y", want: 365},
		{name: "spaces tolerated", input: " 7d ", want: 7},
		{name: "uppercase tolerated", input: "7D", want: 7},
		{name: "zero rejected", input: "0d", wantErr: true},
		{name: "negative rejected", input: "-5d", wantErr: true},
		{name: "bare number rejected", input: "30", wantErr: true},
		{name: "empty rejected", input: "", wantErr: true},
		{name: "unknown unit rejected", input: "30h", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseAgeDuration(tt.input)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestPruneAgeResolvesToBefore(t *testing.T) {
	cfg, err := parsePruneFlags([]string{"--age", "30d"})
	require.NoError(t, err)
	assert.NotEmpty(t, cfg.Filter.Before, "age resolves to a before date")
	assert.True(t, cfg.Filter.HasFilters())

	_, err = parsePruneFlags([]string{"--age", "30d", "--before", "2026-01-01"})
	assert.Error(t, err, "age and before are mutually exclusive")
}

func TestParsePruneFlagsSourceOnly(t *testing.T) {
	cfg, err := parsePruneFlags([]string{"--project", "p", "--source-only"})
	require.NoError(t, err)
	assert.True(t, cfg.SourceOnly)
}

// TestPruneTrashesSourcesAndRestores covers the full loop: prune
// trashes the source file and excludes the archive row, then
// prune restore puts the file back and re-enables the row.
func TestPruneTrashesSourcesAndRestores(t *testing.T) {
	d := dbtest.OpenTestDB(t)

	root := t.TempDir()
	first := filepath.Join(root, "one", "session-1.jsonl")
	second := filepath.Join(root, "two", "session-2.jsonl")
	writeTrashFixture(t, first, "alpha")
	writeTrashFixture(t, second, "beta")

	dbtest.SeedSession(t, d, "pr-1", "proj-a", func(s *db.Session) {
		s.FilePath = new(first)
	})
	dbtest.SeedSession(t, d, "pr-2", "proj-b", func(s *db.Session) {
		s.FilePath = new(second)
	})

	trashStore := trash.New(t.TempDir())
	var out bytes.Buffer
	pruner := &Pruner{DB: d, Out: &out, Trash: trashStore}

	cfg, err := parsePruneFlags([]string{"--project", "proj-a", "--yes"})
	require.NoError(t, err)
	require.NoError(t, pruner.Prune(cfg))

	assert.NoFileExists(t, first, "matching source file must be trashed")
	assert.FileExists(t, second, "non-matching source stays")
	assert.True(t, d.IsSessionExcluded("pr-1"), "archived row is excluded")
	assert.False(t, d.IsSessionExcluded("pr-2"))
	assert.Contains(t, out.String(), "trashed 1 source files")
	assert.Contains(t, out.String(), "prune restore")

	// Restore brings the file back and re-enables the archive row.
	require.NoError(t, pruneRestore(PruneRestoreConfig{Yes: true},
		pruneRestoreDeps{
			store:  trashStore,
			openDB: func() (*db.DB, func(), error) { return d, func() {}, nil },
		}))
	assert.FileExists(t, first, "restored from trash")
	assert.False(t, d.IsSessionExcluded("pr-1"), "row re-importable")
}

func TestPruneSourceOnlyKeepsArchiveRows(t *testing.T) {
	d := dbtest.OpenTestDB(t)

	src := filepath.Join(t.TempDir(), "keep-archive", "s.jsonl")
	writeTrashFixture(t, src, "content")
	dbtest.SeedSession(t, d, "so-1", "proj-so", func(s *db.Session) {
		s.FilePath = new(src)
	})

	pruner := &Pruner{
		DB: d, Out: &bytes.Buffer{}, In: os.Stdin,
		Trash: trash.New(t.TempDir()),
	}
	cfg, err := parsePruneFlags([]string{
		"--project", "proj-so", "--source-only", "--yes",
	})
	require.NoError(t, err)
	require.NoError(t, pruner.Prune(cfg))

	assert.NoFileExists(t, src, "source is trashed")
	assert.False(t, d.IsSessionExcluded("so-1"), "archive row kept")

	kept, getErr := d.GetSession(context.Background(), "so-1")
	require.NoError(t, getErr)
	assert.NotNil(t, kept, "session row still present")
}

func TestPruneDryRunLeavesFilesAlone(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	src := filepath.Join(t.TempDir(), "dry", "s.jsonl")
	writeTrashFixture(t, src, "data")
	dbtest.SeedSession(t, d, "dry-1", "proj-dry", func(s *db.Session) {
		s.FilePath = new(src)
	})

	var out bytes.Buffer
	pruner := &Pruner{DB: d, Out: &out, Trash: trash.New(t.TempDir())}
	cfg, err := parsePruneFlags([]string{"--project", "proj-dry", "--dry-run"})
	require.NoError(t, err)
	require.NoError(t, pruner.Prune(cfg))

	assert.FileExists(t, src, "dry run never touches files")
	assert.False(t, d.IsSessionExcluded("dry-1"))
	assert.Contains(t, out.String(), "Dry run")
}

func TestTrashSourcesSkipsMissingAndNilPaths(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	missing := filepath.Join(t.TempDir(), "gone.jsonl")

	trashStore := trash.New(t.TempDir())
	pruner := &Pruner{DB: d, Out: &bytes.Buffer{}, Trash: trashStore}

	withoutPath := db.Session{ID: "np-1", Project: "p"}
	withMissing := db.Session{ID: "np-2", Project: "p", FilePath: new(missing)}

	trashed, skipped, reclaimed := pruner.trashSources(
		[]db.Session{withoutPath, withMissing})
	assert.Equal(t, 0, trashed)
	assert.Equal(t, 2, skipped)
	assert.Equal(t, int64(0), reclaimed)
}

func writeTrashFixture(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
}
