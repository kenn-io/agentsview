//go:build linux

package rawcapture

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/parser"
)

func TestCapturerChecksCapacityBeforeReadingUnchangedCompanion(t *testing.T) {
	const maxBytes = int64(1 << 20)
	store, _ := openCapturerTestStore(t, maxBytes)
	provider, source, transcript := captureFileProvider(t, "one\n")
	companionPath := filepath.Join(filepath.Dir(transcript), "companion.json")
	require.NoError(t, os.WriteFile(companionPath, []byte("companion\n"), 0o600))
	provider.plan.Entries = append(provider.plan.Entries, parser.RawCaptureEntry{
		Path: "project/companion.json", LocalPath: companionPath,
	})
	capturer := New(store)
	first, err := capturer.Capture(t.Context(), provider, source)
	require.NoError(t, err)
	usage, err := store.OutboxUsage(t.Context())
	require.NoError(t, err)
	_, err = store.ReserveCapture(
		t.Context(), first.Source.ConfiguredRootID, maxBytes-usage.UsedBytes,
	)
	require.NoError(t, err)
	require.NoError(t, appendFile(transcript, "two\n"))
	info, err := os.Stat(companionPath)
	require.NoError(t, err)
	wantAtime := time.Unix(1, 0)
	require.NoError(t, os.Chtimes(companionPath, wantAtime, info.ModTime()))

	result, err := capturer.Capture(t.Context(), provider, source)

	require.NoError(t, err)
	assert.Equal(t, StatusDegraded, result.Status)
	var stat syscall.Stat_t
	require.NoError(t, syscall.Stat(companionPath, &stat))
	assert.Equal(t, wantAtime.UnixNano(), stat.Atim.Sec*int64(time.Second)+stat.Atim.Nsec)
}
