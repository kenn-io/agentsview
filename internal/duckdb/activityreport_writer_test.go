package duckdb

import (
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"
)

func TestActivityWriterReturnsBeforeSaveAndDrainsOnClose(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var writer activityWriter
		started := make(chan struct{})
		release := make(chan struct{})
		writer.put("first", 1, func() {
			close(started)
			<-release
		})
		<-started
		saved := make(chan string, 1)
		writer.put("next", 1, func() { saved <- "old" })
		writer.put("next", 1, func() { saved <- "new" })
		closed := make(chan struct{})
		go func() {
			writer.close()
			close(closed)
		}()
		synctest.Wait()
		select {
		case <-closed:
			t.Fatal("close returned before the pending writes finished")
		default:
		}
		close(release)
		<-closed
		require.Equal(t, "new", <-saved)
		select {
		case value := <-saved:
			t.Fatalf("superseded write ran: %s", value)
		default:
		}
	})
}
