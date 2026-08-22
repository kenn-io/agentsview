//go:build !windows

package capture

import (
	"context"
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
	parent := filepath.Join(t.TempDir(), "private")
	require.NoError(t, os.Mkdir(parent, 0o700))
	dir := filepath.Join(parent, "capture")
	state, err := createState(dir, permissionTestManifest(t))
	require.NoError(t, err)
	state.close()
	require.NoError(t, os.Chmod(parent, 0o777))

	_, err = openState(dir)

	require.ErrorContains(t, err, "writable by another user")
}

func TestOpenCaptureEngineCreatesOwnerOnlyArchive(t *testing.T) {
	state := &captureState{dir: t.TempDir(), manifest: manifest{
		Provider: string(ProviderClaude), Limits: DefaultLimits(),
	}}
	database, engine, err := openCaptureEngine(context.Background(), state, nil)
	require.NoError(t, err)
	engine.Close()
	require.NoError(t, database.Close())

	info, err := os.Stat(state.archivePath())
	require.NoError(t, err)
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
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, manifestFileName), []byte("not json"), 0o600))
	require.NoError(t, os.Chmod(dir, 0o755))

	_, err := openState(dir)

	require.ErrorContains(t, err, "decoding capture manifest")
	info, statErr := os.Stat(dir)
	require.NoError(t, statErr)
	assert.Equal(t, os.FileMode(0o755), info.Mode().Perm())
	assert.NoFileExists(t, filepath.Join(dir, lockFileName))
}
