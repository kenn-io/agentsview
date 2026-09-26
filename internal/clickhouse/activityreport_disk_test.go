package clickhouse

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The sweep removes reports whose last open is more than 30 days old and
// temporary files an interrupted write left over an hour ago. It keeps
// newer reports, writes still in progress, and the kept day selections.
func TestActivityReportSweepRemovesOnlyStaleFiles(t *testing.T) {
	d := activityReportDisk{dir: t.TempDir()}
	now := time.Now()
	files := map[string]time.Duration{
		"unopened.json":                   31 * 24 * time.Hour,
		"opened.json":                     29 * 24 * time.Hour,
		activityDaySelectionsFile:         365 * 24 * time.Hour,
		activityReportTempPrefix + "old":  2 * time.Hour,
		activityReportTempPrefix + "busy": time.Minute,
	}
	for name, age := range files {
		path := filepath.Join(d.dir, name)
		require.NoError(t, os.WriteFile(path, nil, 0o600))
		require.NoError(t, os.Chtimes(path, now.Add(-age), now.Add(-age)))
	}
	removed, err := d.sweep(now)
	require.NoError(t, err)
	require.Equal(t, 2, removed)
	for name := range files {
		gone := name == "unopened.json" || name == activityReportTempPrefix+"old"
		_, err := os.Stat(filepath.Join(d.dir, name))
		require.Equal(t, gone, os.IsNotExist(err), name)
	}
}
