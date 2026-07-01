package db

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOpenTestDBWithTemplateFallsBackWhenTemplateUnavailable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")

	// Simulate a poisoned template build that also left a partial
	// copy behind: the fallback must discard it and open a fresh,
	// fully migrated database instead of failing the test.
	d, err := openTestDBWithTemplate(path, func(dst string) error {
		require.NoError(t, os.WriteFile(dst, []byte("garbage"), 0o600),
			"writing partial template copy")
		return errors.New("template poisoned")
	})
	require.NoError(t, err, "fallback open")
	t.Cleanup(func() { require.NoError(t, d.Close()) })

	var sessions int
	require.NoError(t,
		d.getReader().QueryRow("SELECT COUNT(*) FROM sessions").Scan(&sessions),
		"querying sessions on fallback db")
	require.Zero(t, sessions, "fresh fallback db should be empty")
}
