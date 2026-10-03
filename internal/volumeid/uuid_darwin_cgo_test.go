//go:build darwin && cgo

package volumeid

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// diskutilVolumeUUID reads a volume's UUID the way a person would, as an
// independent check on the getattrlist reply layout.
func diskutilVolumeUUID(t *testing.T, mount string) [16]byte {
	t.Helper()
	out, err := exec.Command("diskutil", "info", mount).Output()
	if err != nil {
		t.Skipf("diskutil unavailable: %v", err)
	}
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		key, value, ok := strings.Cut(sc.Text(), ":")
		if !ok || strings.TrimSpace(key) != "Volume UUID" {
			continue
		}
		b, err := hex.DecodeString(strings.ReplaceAll(strings.TrimSpace(value), "-", ""))
		require.NoError(t, err)
		require.Len(t, b, 16)
		return [16]byte(b)
	}
	t.Skipf("diskutil reports no Volume UUID for %s", mount)
	return [16]byte{}
}

func TestMountedVolumeUUIDMatchesDiskutil(t *testing.T) {
	// The data volume exists on every macOS since 10.15; diskutil skips the
	// test on a host that lays volumes out differently.
	const mount = "/System/Volumes/Data"
	want := diskutilVolumeUUID(t, mount)

	got, answer := mountedVolumeUUID(mount)
	require.Equal(t, uuidFound, answer)
	assert.Equal(t, want, got)
}

func TestMountedVolumeUUIDTellsNoUUIDFromAFailedCall(t *testing.T) {
	// devfs has no UUID, so the caller uses its mount point. A call that
	// fails must say so instead, or a transient error would look like a
	// volume without one and change the volume's value.
	_, answer := mountedVolumeUUID("/dev")
	assert.Equal(t, uuidNone, answer)

	_, answer = mountedVolumeUUID(filepath.Join(t.TempDir(), "not-mounted-here"))
	assert.Equal(t, uuidFailed, answer)
}

func TestStableMapsARealFileToItsVolume(t *testing.T) {
	info, err := os.Stat(t.TempDir())
	require.NoError(t, err)
	stat, ok := info.Sys().(*syscall.Stat_t)
	require.True(t, ok)
	dev := uint64(stat.Dev)

	got := Stable(dev)
	assert.NotEqual(t, dev, got, "the temp directory's volume is in the mount table")
	assert.Equal(t, got, Stable(dev))
}
