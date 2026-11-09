//go:build chtest && linux

package clickhouse

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"golang.org/x/sys/unix"
)

func TestUsageDiskPushDuringLoad(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	local, syncer, first := newPreparedUsageStore(t)
	require.NoError(t, first.openActivityReportDisk(syncer.target))
	f := db.UsageFilter{Timezone: "UTC", Breakdowns: true}
	before, err := first.GetDailyUsage(t.Context(), f)
	require.NoError(t, err)
	files, err := filepath.Glob(filepath.Join(os.Getenv("CACHE_DIRECTORY"), "clickhouse-usage", "*", "*.gob"))
	require.NoError(t, err)
	require.Len(t, files, 1)
	body, err := os.ReadFile(files[0])
	require.NoError(t, err)
	require.NoError(t, os.Remove(files[0]))
	require.NoError(t, unix.Mkfifo(files[0], 0o600))
	republishRoundSession(t, local, 123)
	done := make(chan error, 1)
	go func() {
		file, err := os.OpenFile(files[0], os.O_WRONLY, 0)
		if err != nil {
			done <- err
			return
		}
		_, err = file.Write(body)
		if err == nil {
			result, resultErr := syncer.Push(t.Context(), false, nil)
			err = resultErr
			if err == nil && result.Errors != 0 {
				err = fmt.Errorf("push reported %d errors", result.Errors)
			}
		}
		closeErr := file.Close()
		if err == nil {
			err = closeErr
		}
		done <- err
	}()
	restarted := newTestStore(t, syncer.conn, syncer.target)
	require.NoError(t, restarted.openActivityReportDisk(syncer.target))
	got, err := restarted.GetDailyUsage(t.Context(), f)
	require.NoError(t, <-done)
	require.NoError(t, err)
	require.Equal(t, before, got)
	require.NoError(t, os.Remove(files[0]))
	fresh := newTestStore(t, syncer.conn, syncer.target)
	want, err := fresh.GetDailyUsage(t.Context(), f)
	require.NoError(t, err)
	next, err := restarted.GetDailyUsage(t.Context(), f)
	require.NoError(t, err)
	require.Equal(t, want, next)
}
