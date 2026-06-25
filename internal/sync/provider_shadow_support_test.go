package sync

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// writeProviderShadowSourceFile writes a provider source fixture, creating the
// parent directory. It is the shared helper for the per-provider shadow/parse
// tests (the Codex fold is the lowest caller; later provider folds reuse it).
func writeProviderShadowSourceFile(t *testing.T, path, content string) {
	t.Helper()

	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
}
