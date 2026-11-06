package kept

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/activity"
)

func TestKeptReportsRoundTrip(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	q, err := activity.ResolveQuery(activity.QueryInput{Preset: "day", Date: "2026-01-01", Timezone: "UTC"}, time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	require.True(t, DayPreset(q, time.Now()))
	var first Reports
	require.NoError(t, first.OpenDisk("test-reports", "mirror"))
	entry := Report{Done: activity.Progress{Phase: activity.ProgressDone, SessionsTotal: 3, RowsProcessed: 12}}
	selection := Selection("filter", q)
	first.Put(selection, "version", true, entry)
	// Replacing a saved report must also replace what a restart reads.
	entry.Done.RowsProcessed = 24
	first.Put(selection, "version", true, entry)
	first.Put("range", "version", false, entry)
	_, err = os.Stat(first.Path("range"))
	require.ErrorIs(t, err, os.ErrNotExist)
	got, ok := first.Get("range", "version", false)
	require.True(t, ok)
	require.Equal(t, entry, got)
	first.Clear()
	_, ok = first.Get("range", "version", false)
	require.False(t, ok)

	var restarted Reports
	require.NoError(t, restarted.OpenDisk("test-reports", "mirror"))
	got, ok = restarted.Get(selection, "version", true)
	require.True(t, ok)
	require.Equal(t, entry.Done, got.Done)
	wantDigest, err := activity.ArtifactDigest(entry.Artifacts)
	require.NoError(t, err)
	gotDigest, err := activity.ArtifactDigest(got.Artifacts)
	require.NoError(t, err)
	require.Equal(t, wantDigest, gotDigest)
	_, ok = restarted.Get(selection, "other-version", true)
	require.False(t, ok)
	// A file written by another binary is rebuilt even under the same key.
	restarted.Clear()
	restarted.disk.build = "another-binary"
	_, ok = restarted.Get(selection, "version", true)
	require.False(t, ok)
}
