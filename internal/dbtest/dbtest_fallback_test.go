package dbtest

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

func TestEnsureTestDBAtFallsBackWhenTemplateUnavailable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")

	// Simulate a poisoned template build that also left a partial
	// copy behind: the fallback must discard it and still produce a
	// usable current-schema database.
	ensureTestDBAtWith(t, path, func(dst string) error {
		require.NoError(t, os.WriteFile(dst, []byte("garbage"), 0o600),
			"writing partial template copy")
		return errors.New("template poisoned")
	})

	d, err := db.Open(path)
	require.NoError(t, err, "opening fallback test db")
	require.NoError(t, d.Close(), "closing fallback test db")
}

func TestEnsureTestDBAtLeavesExistingFileIntact(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")

	EnsureTestDBAt(t, path)
	info, err := os.Stat(path)
	require.NoError(t, err, "statting created test db")

	// A second call must not rebuild or replace the existing file,
	// and must not consult the template at all.
	ensureTestDBAtWith(t, path, func(string) error {
		t.Fatal("copyTemplate must not run for an existing file")
		return nil
	})
	again, err := os.Stat(path)
	require.NoError(t, err, "statting test db after second ensure")
	require.Equal(t, info.ModTime(), again.ModTime(),
		"existing test db was modified")
}
