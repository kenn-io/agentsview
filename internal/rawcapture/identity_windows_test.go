//go:build windows

package rawcapture

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

func TestStableFileIdentityDistinguishesFilesWithEqualCreationTimes(t *testing.T) {
	root := t.TempDir()
	firstPath := filepath.Join(root, "first.jsonl")
	secondPath := filepath.Join(root, "second.jsonl")
	require.NoError(t, os.WriteFile(firstPath, []byte("same\n"), 0o600))
	require.NoError(t, os.WriteFile(secondPath, []byte("same\n"), 0o600))
	first, err := os.OpenFile(firstPath, os.O_RDWR, 0)
	require.NoError(t, err)
	defer first.Close()
	second, err := os.OpenFile(secondPath, os.O_RDWR, 0)
	require.NoError(t, err)
	defer second.Close()
	created := windows.NsecToFiletime(
		time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC).UnixNano(),
	)
	require.NoError(t, windows.SetFileTime(
		windows.Handle(first.Fd()), &created, nil, nil,
	))
	require.NoError(t, windows.SetFileTime(
		windows.Handle(second.Fd()), &created, nil, nil,
	))
	firstInfo, err := first.Stat()
	require.NoError(t, err)
	secondInfo, err := second.Stat()
	require.NoError(t, err)

	firstIdentity := stableFileIdentity(first, firstInfo)
	secondIdentity := stableFileIdentity(second, secondInfo)

	assert.NotEmpty(t, firstIdentity)
	assert.NotEmpty(t, secondIdentity)
	assert.NotEqual(t, firstIdentity, secondIdentity)
}
