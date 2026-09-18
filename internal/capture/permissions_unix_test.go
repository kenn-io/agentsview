//go:build !windows

package capture

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCreateStateRejectsReplaceableParent(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "shared")
	require.NoError(t, os.Mkdir(parent, 0o777))
	require.NoError(t, os.Chmod(parent, 0o777))

	_, err := createState(filepath.Join(parent, "capture"), permissionTestManifest(t))

	require.ErrorContains(t, err, "writable by another user")
}

func TestCreateStateAllowsTrustedStickyParent(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "shared")
	require.NoError(t, os.Mkdir(parent, 0o777))
	require.NoError(t, os.Chmod(parent, os.ModeSticky|0o777))

	state, err := createState(
		filepath.Join(parent, "capture"), permissionTestManifest(t))
	require.NoError(t, err)
	state.close()
}

func TestOpenStateRejectsParentMadeReplaceable(t *testing.T) {
	require := require.New(t)

	parent := filepath.Join(t.TempDir(), "private")
	require.NoError(os.Mkdir(parent, 0o700))
	dir := filepath.Join(parent, "capture")
	state, err := createState(dir, permissionTestManifest(t))
	require.NoError(err)
	state.close()
	require.NoError(os.Chmod(parent, 0o777))

	_, err = openState(dir)

	require.ErrorContains(err, "writable by another user")
}

func TestOpenCaptureEngineCreatesOwnerOnlyArchive(t *testing.T) {
	require := require.New(t)

	state := &captureState{dir: t.TempDir(), manifest: manifest{
		Provider: string(ProviderClaude), Limits: DefaultLimits(),
	}}
	database, engine, err := openCaptureEngine(t.Context(), state, nil)
	require.NoError(err)
	engine.Close()
	require.NoError(database.Close())

	info, err := os.Stat(state.archivePath())
	require.NoError(err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func permissionTestManifest(t *testing.T) manifest {
	t.Helper()
	return manifest{
		OccurrenceID:      "private-parent",
		Provider:          string(ProviderClaude),
		ProviderSessionID: "11111111-1111-4111-8111-111111111111",
		ProviderRoot:      t.TempDir(),
		ProviderWorkDir:   t.TempDir(),
		StartedAt:         time.Now(),
		Invocation:        invocationName(ProviderClaude),
		Limits:            DefaultLimits(),
	}
}

func TestOpenStateDoesNotSecureAnInvalidCaptureDirectory(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	dir := t.TempDir()
	require.NoError(os.WriteFile(
		filepath.Join(dir, manifestFileName), []byte("not json"), 0o600))
	require.NoError(os.Chmod(dir, 0o755))

	_, err := openState(dir)

	require.ErrorContains(err, "decoding capture manifest")
	info, statErr := os.Stat(dir)
	require.NoError(statErr)
	assert.Equal(os.FileMode(0o755), info.Mode().Perm())
	assert.NoFileExists(filepath.Join(dir, lockFileName))
}
