//go:build darwin || linux

package rawcapture

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
	"golang.org/x/sys/unix"
)

func TestCapturerSQLiteArchiveWith600TranscriptsFits1024Descriptors(t *testing.T) {
	const descriptorLimit = 1024
	limit := unix.Rlimit{}
	require.NoError(t, unix.Getrlimit(unix.RLIMIT_NOFILE, &limit))
	if limit.Max < descriptorLimit {
		t.Skipf("hard open-file limit %d is below %d", limit.Max, descriptorLimit)
	}
	originalLimit := limit
	limit.Cur = descriptorLimit
	require.NoError(t, unix.Setrlimit(unix.RLIMIT_NOFILE, &limit))
	t.Cleanup(func() { require.NoError(t, unix.Setrlimit(unix.RLIMIT_NOFILE, &originalLimit)) })

	provider, source, _ := sqliteArchiveFixture(t)
	root := provider.plan.CaptureRoot
	entries := []parser.RawCaptureEntry{{Path: "state.db", LocalPath: filepath.Join(root, "state.db")}}
	sessions := filepath.Join(root, "sessions")
	require.NoError(t, os.Mkdir(sessions, 0o700))
	for i := range 600 {
		name := fmt.Sprintf("%03d.jsonl", i)
		local := filepath.Join(sessions, name)
		require.NoError(t, os.WriteFile(local, []byte("transcript\n"), 0o600))
		entries = append(entries, parser.RawCaptureEntry{Path: "sessions/" + name, LocalPath: local})
	}
	provider.plan.Entries = entries

	store, _ := openCapturerTestStore(t, 32<<20)
	result, err := New(store).Capture(t.Context(), provider, source)
	require.NoError(t, err)
	require.Equal(t, StatusCaptured, result.Status)

	base, found, err := store.CaptureBase(t.Context(), result.Source)
	require.NoError(t, err)
	require.True(t, found)
	require.Len(t, base.Entries, 601)
}
