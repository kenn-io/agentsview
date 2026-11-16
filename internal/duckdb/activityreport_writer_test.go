package duckdb

import (
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
)

func TestActivityWriterWaitsForIdleAndCoalesces(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var writer activityWriter
		saved := make(chan string, 2)
		writer.put("key", 1, func() { saved <- "old" })
		time.Sleep(9 * time.Second)
		synctest.Wait()
		require.Empty(t, saved)
		writer.put("key", 1, func() { saved <- "new" })
		time.Sleep(9 * time.Second)
		synctest.Wait()
		require.Empty(t, saved)
		time.Sleep(time.Second)
		synctest.Wait()
		require.Len(t, saved, 1)
		require.Equal(t, "new", <-saved)
		writer.close()
	})
}

func TestActivityWriterCloseWakesAndDrains(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var writer activityWriter
		started := make(chan struct{})
		release := make(chan struct{})
		writer.put("first", 1, func() { close(started); <-release })
		synctest.Wait()
		before := time.Now()
		closed := make(chan struct{})
		go func() { writer.close(); close(closed) }()
		<-started
		require.Zero(t, time.Since(before))
		synctest.Wait()
		select {
		case <-closed:
			t.Fatal("close returned before the write finished")
		default:
		}
		close(release)
		<-closed
	})
}
