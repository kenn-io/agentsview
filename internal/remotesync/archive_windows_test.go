//go:build windows

package remotesync

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"

	"go.kenn.io/agentsview/internal/parser"
)

func setWindowsFileSDDLForTest(t *testing.T, path, sddl string) {
	t.Helper()
	sd, err := windows.SecurityDescriptorFromString(sddl)
	require.NoError(t, err)
	dacl, _, err := sd.DACL()
	require.NoError(t, err)
	require.NotNil(t, dacl)
	err = windows.SetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, dacl, nil,
	)
	runtime.KeepAlive(sd)
	require.NoError(t, err)
}

func TestUnreadableArchiveMembersAreSkippedOnWindows(t *testing.T) {
	root := t.TempDir()
	healthyDir := filepath.Join(root, "proj-a")
	secretDir := filepath.Join(root, "proj-b", "memory")
	for _, dir := range []string{healthyDir, secretDir} {
		require.NoError(t, os.MkdirAll(dir, 0o755))
	}

	healthy := filepath.Join(healthyDir, "session.jsonl")
	secret := filepath.Join(secretDir, "dysync-troubleshooting.md")
	require.NoError(t, os.WriteFile(healthy, []byte("healthy session"), 0o644))
	require.NoError(t, os.WriteFile(secret, []byte("restricted note"), 0o644))

	setWindowsFileSDDLForTest(t, secret, "D:P(D;;FR;;;WD)(A;;FA;;;WD)")
	t.Cleanup(func() {
		sd, err := windows.SecurityDescriptorFromString("D:P(A;;FA;;;WD)")
		if err == nil {
			if dacl, _, dErr := sd.DACL(); dErr == nil && dacl != nil {
				_ = windows.SetNamedSecurityInfo(
					secret,
					windows.SE_FILE_OBJECT,
					windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
					nil, nil, dacl, nil,
				)
			}
			runtime.KeepAlive(sd)
		}
	})

	_, statErr := os.Lstat(secret)
	require.NoError(t, statErr, "Lstat should succeed while parent directory is readable")
	_, openErr := os.Open(secret)
	require.ErrorIs(t, openErr, os.ErrPermission)

	targets := TargetSet{
		Dirs: map[parser.AgentType][]string{parser.AgentClaude: {root}},
	}

	manifest, err := BuildManifest(t.Context(), targets)
	require.NoError(t, err)
	assert.Equal(t, []string{healthy}, manifestPaths(manifest))

	var fullBuf bytes.Buffer
	require.NoError(t, WriteArchive(t.Context(), &fullBuf, targets))
	fullEntries := archiveEntries(t, fullBuf.Bytes())
	assert.Equal(t, []byte("healthy session"), fullEntries[archiveNameForTest(t, healthy)])
	assert.NotContains(t, fullEntries, archiveNameForTest(t, secret))

	var mixedDelta bytes.Buffer
	require.NoError(t, WriteArchiveFiles(t.Context(), &mixedDelta, targets, []string{secret, healthy}))
	assert.Equal(t, map[string][]byte{
		archiveNameForTest(t, healthy): []byte("healthy session"),
	}, archiveEntries(t, mixedDelta.Bytes()))

	var onlyUnreadable bytes.Buffer
	require.NoError(t, WriteArchiveFiles(t.Context(), &onlyUnreadable, targets, []string{secret}))
	assert.Empty(t, archiveEntries(t, onlyUnreadable.Bytes()))

	setWindowsFileSDDLForTest(t, secret, "D:P(A;;FA;;;WD)")
	recoveredManifest, err := BuildManifest(t.Context(), targets)
	require.NoError(t, err)
	assert.Equal(t, []string{healthy, secret}, manifestPaths(recoveredManifest))

	var recoveredDelta bytes.Buffer
	require.NoError(t, WriteArchiveFiles(t.Context(), &recoveredDelta, targets, []string{secret}))
	assert.Equal(t, map[string][]byte{
		archiveNameForTest(t, secret): []byte("restricted note"),
	}, archiveEntries(t, recoveredDelta.Bytes()))
}
