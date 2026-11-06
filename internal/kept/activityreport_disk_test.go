package kept

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/kit/atomicfile"
)

// The sweep removes reports whose last open is more than 30 days old and
// temporary files an interrupted write left over an hour ago. It keeps
// newer reports and writes still in progress.
func TestKeptReportSweepRemovesOnlyStaleFiles(t *testing.T) {
	d := activityReportDisk{dir: t.TempDir()}
	now := time.Now()
	files := []struct {
		name string
		age  time.Duration
		temp bool
		gone bool
	}{
		{name: "unopened.json", age: 31 * 24 * time.Hour, gone: true},
		{name: "opened.json", age: 29 * 24 * time.Hour},
		{name: "old.json", age: 2 * time.Hour, temp: true, gone: true},
		{name: "busy.json", age: time.Minute, temp: true},
	}
	for i := range files {
		file := &files[i]
		path := filepath.Join(d.dir, file.name)
		if file.temp {
			staged, err := atomicfile.Create(path, atomicfile.WithPrivate(), atomicfile.WithoutSync())
			require.NoError(t, err)
			path = staged.TempName()
			file.name = filepath.Base(path)
			require.NoError(t, staged.Abort())
		}
		require.NoError(t, os.WriteFile(path, nil, 0o600))
		require.NoError(t, os.Chtimes(path, now.Add(-file.age), now.Add(-file.age)))
	}
	removed, err := d.sweep(now)
	require.NoError(t, err)
	require.Equal(t, 2, removed)
	for _, file := range files {
		_, err := os.Stat(filepath.Join(d.dir, file.name))
		require.Equal(t, file.gone, os.IsNotExist(err), file.name)
	}
}
