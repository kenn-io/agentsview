package remotesync

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A mirror that only ever proved session titles must stay unapplied. Treating
// its sidecar as current would skip the import that reconciles the workspace
// folder and parent conversation of sessions it already holds.
func TestAntigravityTitleVerificationTreatsTitleOnlyRecordsAsUnapplied(t *testing.T) {
	root := filepath.Join(t.TempDir(), "mirror")
	sidecar := mirrorAntigravityTitlesPath(root)

	require.NoError(t, os.WriteFile(sidecar, []byte(`{"version":1,"states":{}}`), 0o600))
	assert.Empty(t, loadAntigravityTitleVerification(root).States,
		"a title-only record must not count as applied")

	require.NoError(t, os.WriteFile(
		sidecar,
		[]byte(`{"version":2,"states":{"store":{"size":1,"modTime":2}}}`),
		0o600,
	))
	applied := loadAntigravityTitleVerification(root)
	assert.Equal(t, antigravityTitleVerificationVersion, applied.Version)
	assert.Len(t, applied.States, 1)
}
